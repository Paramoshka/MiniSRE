package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"mini-sre/internal/remote"
)

const (
	throughputDefaultPort     = 5201
	throughputDefaultDuration = 10
	throughputMaxDuration     = 10
	throughputServerSeconds   = 35
	throughputClientGrace     = 5
	throughputReadinessLimit  = 10 * time.Second
	throughputSSProbeTimeout  = 5 * time.Second
	throughputServerTimeout   = 40 * time.Second
	throughputClientExtra     = 10 * time.Second
	throughputDrainTimeout    = 5 * time.Second
)

var errThroughputListener = errors.New("iperf3 server did not start listening")

// boundedRunner keeps the throughput lifecycle testable without SSH. The
// production implementation is *remote.Runner.
type boundedRunner interface {
	Hosts() []string
	Run(ctx context.Context, hostID, program string, args ...string) (remote.Result, error)
	RunBounded(ctx context.Context, hostID string, timeout time.Duration, program string, args ...string) (remote.Result, error)
}

type throughputRun struct {
	result remote.Result
	err    error
}

// MeasureThroughput runs one temporary client-to-server iperf3 upload test.
func (t *Tools) MeasureThroughput(ctx context.Context, args toolArgs) (string, error) {
	return measureThroughput(ctx, t.Runner, args)
}

func measureThroughput(ctx context.Context, runner boundedRunner, args toolArgs) (string, error) {
	port, duration, err := validateThroughputArgs(runner, args)
	if err != nil {
		return "", err
	}
	if err := checkIperf3(ctx, runner, args.ServerHostID); err != nil {
		return "", err
	}
	if err := checkIperf3(ctx, runner, args.ClientHostID); err != nil {
		return "", err
	}
	if err := checkTimeout(ctx, runner, args.ServerHostID); err != nil {
		return "", err
	}
	if err := checkTimeout(ctx, runner, args.ClientHostID); err != nil {
		return "", err
	}
	if err := checkServerPortFree(ctx, runner, args.ServerHostID, port); err != nil {
		return "", err
	}

	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	serverCh := make(chan throughputRun, 1)
	go func() {
		result, err := runner.RunBounded(serverCtx, args.ServerHostID, throughputServerTimeout,
			"timeout", throughputServerArgs(port, args.BindAddress)...)
		serverCh <- throughputRun{result: result, err: err}
	}()

	serverResult, started, err := waitForThroughputListener(ctx, runner, args.ServerHostID, args.BindAddress, port, serverCh, throughputReadinessLimit)
	if err != nil {
		cancelServer()
		serverResult, _ = drainThroughputServer(serverCh)
		return "", throughputStartupError(args, port, err, serverResult)
	}
	if !started {
		cancelServer()
		return "", throughputServerError(args, port, serverResult)
	}

	clientResult, clientErr := runner.RunBounded(ctx, args.ClientHostID,
		time.Duration(duration)*time.Second+throughputClientExtra,
		"timeout", throughputClientArgs(args.Destination, port, duration)...)
	cancelServer()
	serverResult, serverStopped := drainThroughputServer(serverCh)

	if clientErr != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tools: throughput test from %q to %q was interrupted: %w",
				args.ClientHostID, args.ServerHostID, ctx.Err())
		}
		if isThroughputDiagnostic(clientErr, clientResult.ExitCode) {
			return formatThroughputFailure(args, port, clientResult, clientErr, serverResult), nil
		}
		return "", fmt.Errorf("tools: iperf3 client on %q failed: %w; %s",
			args.ClientHostID, clientErr, boundedThroughputOutput(clientResult))
	}
	report, err := parseThroughputReport(clientResult.Stdout)
	if err != nil {
		return "", fmt.Errorf("tools: iperf3 output from %q could not be parsed: %w; %s",
			args.ClientHostID, err, boundedThroughputOutput(clientResult))
	}
	out := formatThroughputReport(args, port, duration, report)
	if !serverStopped {
		out += "Server cleanup: not confirmed; the temporary listener may remain until its watchdog expires.\n"
	}
	return out, nil
}

func validateThroughputArgs(runner boundedRunner, args toolArgs) (int, int, error) {
	if args.ServerHostID == "" {
		return 0, 0, errors.New("tools: server_host_id is required")
	}
	if args.ClientHostID == "" {
		return 0, 0, errors.New("tools: client_host_id is required")
	}
	if !knownHost(runner, args.ServerHostID) {
		return 0, 0, fmt.Errorf("tools: unknown host %q; use list_hosts", args.ServerHostID)
	}
	if !knownHost(runner, args.ClientHostID) {
		return 0, 0, fmt.Errorf("tools: unknown host %q; use list_hosts", args.ClientHostID)
	}
	if args.ServerHostID == args.ClientHostID {
		return 0, 0, errors.New("tools: server_host_id and client_host_id must differ; this measures network throughput, not loopback")
	}
	if err := validateConnectDestination(args.Destination); err != nil {
		return 0, 0, err
	}
	if err := validateBindAddress(args.BindAddress); err != nil {
		return 0, 0, err
	}
	port := throughputDefaultPort
	if args.Port != nil {
		port = *args.Port
	}
	if port < 1 || port > 65535 {
		return 0, 0, errors.New("tools: port must be 1..65535")
	}
	duration := throughputDefaultDuration
	if args.Duration != nil {
		duration = *args.Duration
	}
	if duration < 1 || duration > throughputMaxDuration {
		return 0, 0, fmt.Errorf("tools: duration must be 1..%d", throughputMaxDuration)
	}
	return port, duration, nil
}

func knownHost(runner boundedRunner, hostID string) bool {
	for _, id := range runner.Hosts() {
		if id == hostID {
			return true
		}
	}
	return false
}

func validateBindAddress(address string) error {
	if address == "" {
		return errors.New("tools: bind_address is required")
	}
	addr, err := netip.ParseAddr(address)
	if err != nil || addr.Zone() != "" {
		return errors.New("tools: bind_address must be one IP address without a zone")
	}
	if addr.IsUnspecified() || addr.IsMulticast() {
		return errors.New("tools: bind_address must be a unicast IP address")
	}
	return nil
}

func checkIperf3(ctx context.Context, runner boundedRunner, hostID string) error {
	result, err := runner.Run(ctx, hostID, "iperf3", "--version")
	if err != nil {
		return fmt.Errorf("tools: iperf3 is required on host %q: %w; %s", hostID, err, boundedThroughputOutput(result))
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("tools: iperf3 is required on host %q: exit code %d; %s", hostID, result.ExitCode, boundedThroughputOutput(result))
	}
	if !iperf3Version(result.Stdout + "\n" + result.Stderr) {
		return fmt.Errorf("tools: iperf3 3.x is required on host %q; got %q", hostID, strings.TrimSpace(result.Stdout+result.Stderr))
	}
	return nil
}

func iperf3Version(output string) bool {
	if line, _, ok := strings.Cut(output, "\n"); ok {
		output = line
	}
	fields := strings.Fields(output)
	for index, field := range fields {
		if field != "iperf" && field != "iperf3" {
			continue
		}
		for offset := 1; offset <= 2 && index+offset < len(fields); offset++ {
			major, ok := majorVersion(fields[index+offset])
			if !ok {
				continue
			}
			return major >= 3
		}
	}
	return false
}

func majorVersion(field string) (int, bool) {
	field = strings.Trim(field, "(),")
	major, _, ok := strings.Cut(field, ".")
	if !ok {
		return 0, false
	}
	version, err := strconv.Atoi(major)
	return version, err == nil
}

func checkTimeout(ctx context.Context, runner boundedRunner, hostID string) error {
	result, err := runner.Run(ctx, hostID, "timeout", "--version")
	if err != nil {
		return fmt.Errorf("tools: GNU timeout is required on host %q: %w; %s", hostID, err, boundedThroughputOutput(result))
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout+result.Stderr, "GNU coreutils") {
		return fmt.Errorf("tools: GNU timeout is required on host %q; got %q", hostID, strings.TrimSpace(result.Stdout+result.Stderr))
	}
	return nil
}

func checkServerPortFree(ctx context.Context, runner boundedRunner, hostID string, port int) error {
	result, err := runner.Run(ctx, hostID, "ss", "-H", "-ltn", throughputPortFilter(port))
	if err != nil {
		return fmt.Errorf("tools: ss (iproute2) is required on server host %q to check port %d: %w; %s",
			hostID, port, err, boundedThroughputOutput(result))
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("tools: port %d is already in use on server host %q; choose another port", port, hostID)
	}
	return nil
}

func throughputPortFilter(port int) string {
	return "sport = :" + strconv.Itoa(port)
}

// waitForThroughputListener reports started=false when the server exits before
// accepting the client. Readiness is checked with ss so the one-off server is
// not consumed by a TCP probe. The listener must be bound to bindAddress, not
// merely on the same port.
func waitForThroughputListener(ctx context.Context, runner boundedRunner, hostID, bindAddress string, port int, serverCh <-chan throughputRun, limit time.Duration) (throughputRun, bool, error) {
	deadline := time.Now().Add(limit)
	want := net.JoinHostPort(bindAddress, strconv.Itoa(port))
	for {
		select {
		case run := <-serverCh:
			return run, false, nil
		default:
		}
		if !time.Now().Before(deadline) {
			return throughputRun{}, false, errThroughputListener
		}
		checkCtx, cancel := context.WithTimeout(ctx, throughputSSProbeTimeout)
		result, err := runner.Run(checkCtx, hostID, "ss", "-H", "-ltn", throughputPortFilter(port))
		cancel()
		if err != nil {
			return throughputRun{}, false, fmt.Errorf("ss (iproute2) is required on server host %q: %w; %s",
				hostID, err, boundedThroughputOutput(result))
		}
		if strings.Contains(result.Stdout, want) {
			select {
			case run := <-serverCh:
				return run, false, nil
			default:
			}
			return throughputRun{}, true, nil
		}
		select {
		case <-ctx.Done():
			return throughputRun{}, false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func throughputServerArgs(port int, bindAddress string) []string {
	return []string{
		"--kill-after=1s", strconv.Itoa(throughputServerSeconds) + "s",
		"iperf3", "-s", "-1", "-p", strconv.Itoa(port), "-B", bindAddress,
	}
}

func throughputClientArgs(destination string, port, duration int) []string {
	return []string{
		"--kill-after=1s", strconv.Itoa(duration+throughputClientGrace) + "s",
		"iperf3", "-c", destination, "-p", strconv.Itoa(port), "-t", strconv.Itoa(duration), "-J",
	}
}

func drainThroughputServer(serverCh <-chan throughputRun) (throughputRun, bool) {
	select {
	case run := <-serverCh:
		return run, true
	case <-time.After(throughputDrainTimeout):
		return throughputRun{}, false
	}
}

func throughputStartupError(args toolArgs, port int, cause error, server throughputRun) error {
	if errors.Is(cause, errThroughputListener) {
		return throughputServerError(args, port, server)
	}
	return fmt.Errorf("tools: throughput test on %q could not start: %w; server output: %s",
		args.ServerHostID, cause, boundedThroughputOutput(server.result))
}

func throughputServerError(args toolArgs, port int, server throughputRun) error {
	output := boundedThroughputOutput(server.result)
	if server.err != nil && !errors.Is(server.err, context.Canceled) {
		return fmt.Errorf("tools: iperf3 server on %q failed: %w; %s", args.ServerHostID, server.err, output)
	}
	if server.err == nil && server.result.ExitCode != 0 {
		return fmt.Errorf("tools: iperf3 server on %q exited with code %d; %s", args.ServerHostID, server.result.ExitCode, output)
	}
	return fmt.Errorf("tools: iperf3 server on %q did not start listening on %s:%d; %s",
		args.ServerHostID, args.BindAddress, port, output)
}

func isThroughputDiagnostic(err error, exitCode int) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	return exitCode == 1 || exitCode == 124 || exitCode == 137
}

func boundedThroughputOutput(result remote.Result) string {
	out := strings.TrimSpace(commandOutput(result))
	if out == "" {
		return "no output"
	}
	const limit = 4096
	if len(out) > limit {
		out = out[:limit] + "\n[output truncated at 4 KiB]"
	}
	return out
}

type throughputReport struct {
	End struct {
		SumSent struct {
			Bytes         float64 `json:"bytes"`
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int     `json:"retransmits"`
			Seconds       float64 `json:"seconds"`
		} `json:"sum_sent"`
		SumReceived struct {
			Bytes         float64 `json:"bytes"`
			BitsPerSecond float64 `json:"bits_per_second"`
			Seconds       float64 `json:"seconds"`
		} `json:"sum_received"`
	} `json:"end"`
}

func parseThroughputReport(stdout string) (throughputReport, error) {
	var report throughputReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		return report, err
	}
	if report.End.SumSent.Seconds <= 0 && report.End.SumSent.BitsPerSecond <= 0 {
		return report, errors.New("sender statistics are missing")
	}
	return report, nil
}

func formatThroughputReport(args toolArgs, port, duration int, report throughputReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Throughput test: TCP upload from %s to %s at %s:%d\n",
		args.ClientHostID, args.ServerHostID, args.Destination, port)
	fmt.Fprintf(&out, "Duration: %ds, streams: 1, server bind: %s\n", duration, args.BindAddress)
	sent := report.End.SumSent
	received := report.End.SumReceived
	fmt.Fprintf(&out, "Sender:   %s (%s in %.2fs)\n",
		humanBitrate(sent.BitsPerSecond), humanBytes(uint64(sent.Bytes)), sent.Seconds)
	if received.BitsPerSecond > 0 || received.Bytes > 0 {
		fmt.Fprintf(&out, "Receiver: %s (%s in %.2fs)\n",
			humanBitrate(received.BitsPerSecond), humanBytes(uint64(received.Bytes)), received.Seconds)
	} else {
		out.WriteString("Receiver: n/a\n")
	}
	fmt.Fprintf(&out, "Retransmits: %d\n", sent.Retransmits)
	out.WriteString("Status: completed\n")
	out.WriteString("TCP throughput only; this does not prove application performance.\n")
	return out.String()
}

func formatThroughputFailure(args toolArgs, port int, client remote.Result, clientErr error, server throughputRun) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Throughput test: TCP upload from %s to %s at %s:%d\n",
		args.ClientHostID, args.ServerHostID, args.Destination, port)
	status := "failed"
	if errors.Is(clientErr, context.DeadlineExceeded) || errors.Is(clientErr, context.Canceled) || client.ExitCode == 124 || client.ExitCode == 137 {
		status = "timed out"
	}
	fmt.Fprintf(&out, "Status: %s (exit code %d)\n", status, client.ExitCode)
	if output := boundedThroughputOutput(client); output != "no output" {
		out.WriteString("Client output:\n")
		out.WriteString(output)
		out.WriteString("\n")
	}
	if server.result.ExitCode != 0 || server.err != nil || strings.TrimSpace(server.result.Stdout+server.result.Stderr) != "" {
		if output := boundedThroughputOutput(server.result); output != "no output" {
			out.WriteString("Server log:\n")
			out.WriteString(output)
			out.WriteString("\n")
		}
	}
	out.WriteString("No throughput result was produced.\n")
	return out.String()
}

func humanBitrate(bits float64) string {
	switch {
	case bits >= 1e9:
		return fmt.Sprintf("%.2f Gbit/s", bits/1e9)
	case bits >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.1f Kbit/s", bits/1e3)
	default:
		return fmt.Sprintf("%.0f bit/s", bits)
	}
}
