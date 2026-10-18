package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mini-sre/internal/remote"
)

type throughputFakeCall struct {
	hostID  string
	timeout time.Duration
	program string
	args    []string
	bounded bool
}

type throughputFakeRunner struct {
	mu        sync.Mutex
	calls     []throughputFakeCall
	runFn     func(context.Context, string, string, []string) (remote.Result, error)
	boundedFn func(context.Context, string, time.Duration, string, []string) (remote.Result, error)
}

func (f *throughputFakeRunner) Hosts() []string {
	return []string{"local", "client", "server"}
}

func (f *throughputFakeRunner) Run(ctx context.Context, hostID, program string, args ...string) (remote.Result, error) {
	f.record(hostID, 0, program, args, false)
	if f.runFn == nil {
		return remote.Result{}, fmt.Errorf("tools: unexpected command %q", program)
	}
	return f.runFn(ctx, hostID, program, args)
}

func (f *throughputFakeRunner) RunBounded(ctx context.Context, hostID string, timeout time.Duration, program string, args ...string) (remote.Result, error) {
	f.record(hostID, timeout, program, args, true)
	if f.boundedFn == nil {
		return remote.Result{}, fmt.Errorf("tools: unexpected bounded command %q", program)
	}
	return f.boundedFn(ctx, hostID, timeout, program, args)
}

func (f *throughputFakeRunner) record(hostID string, timeout time.Duration, program string, args []string, bounded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, throughputFakeCall{
		hostID: hostID, timeout: timeout, program: program,
		args: append([]string(nil), args...), bounded: bounded,
	})
}

func (f *throughputFakeRunner) snapshot() []throughputFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]throughputFakeCall(nil), f.calls...)
}

func throughputInt(value int) *int {
	return &value
}

func throughputBaseArgs() toolArgs {
	return toolArgs{
		ServerHostID: "server",
		ClientHostID: "client",
		Destination:  "10.0.0.2",
		BindAddress:  "10.0.0.2",
		Port:         throughputInt(5201),
		Duration:     throughputInt(10),
	}
}

func throughputTestRunner() (*throughputFakeRunner, chan struct{}) {
	done := make(chan struct{})
	var once sync.Once
	var ssCalls atomic.Int32
	runner := &throughputFakeRunner{
		runFn: func(_ context.Context, _ string, program string, args []string) (remote.Result, error) {
			switch {
			case program == "iperf3" && len(args) == 1 && args[0] == "--version":
				return remote.Result{Stdout: "iperf 3.16 (cJSON 1.7.15)\n"}, nil
			case program == "timeout" && len(args) == 1 && args[0] == "--version":
				return remote.Result{Stdout: "timeout (GNU coreutils) 9.4\n"}, nil
			case program == "ss" && ssCalls.Add(1) == 1:
				return remote.Result{}, nil
			case program == "ss":
				return remote.Result{Stdout: "LISTEN 0 128 10.0.0.2:5201 0.0.0.0:*\n"}, nil
			}
			return remote.Result{}, fmt.Errorf("unexpected command %q %v", program, args)
		},
		boundedFn: func(ctx context.Context, hostID string, _ time.Duration, _ string, _ []string) (remote.Result, error) {
			switch hostID {
			case "server":
				<-ctx.Done()
				once.Do(func() { close(done) })
				return remote.Result{ExitCode: 124, Stdout: "Server listening on 10.0.0.2 5201\n"}, ctx.Err()
			case "client":
				return remote.Result{Stdout: `{"end":{"sum_sent":{"bytes":1250000000,"bits_per_second":1000000000,"retransmits":7,"seconds":10.0},"sum_received":{"bytes":1240000000,"bits_per_second":992000000,"seconds":10.0}}}`}, nil
			}
			return remote.Result{}, fmt.Errorf("unexpected host %q", hostID)
		},
	}
	return runner, done
}

func TestMeasureThroughput(t *testing.T) {
	runner, serverDone := throughputTestRunner()
	out, err := measureThroughput(context.Background(), runner, throughputBaseArgs())
	if err != nil {
		t.Fatalf("measureThroughput: %v", err)
	}
	for _, want := range []string{
		"TCP upload from client to server at 10.0.0.2:5201",
		"Sender:   1.00 Gbit/s (1.2 GiB in 10.00s)",
		"Receiver: 992.0 Mbit/s",
		"Retransmits: 7",
		"Status: completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
	select {
	case <-serverDone:
	default:
		t.Fatal("temporary iperf3 server was not stopped")
	}

	calls := runner.snapshot()
	var serverCall, clientCall *throughputFakeCall
	for i := range calls {
		call := &calls[i]
		if !call.bounded {
			continue
		}
		switch call.hostID {
		case "server":
			serverCall = call
		case "client":
			clientCall = call
		}
	}
	if serverCall == nil || clientCall == nil {
		t.Fatalf("missing bounded calls: %+v", runner.snapshot())
	}
	wantServer := "--kill-after=1s 35s iperf3 -s -1 -p 5201 -B 10.0.0.2"
	if got := strings.Join(serverCall.args, " "); got != wantServer {
		t.Errorf("server arguments = %q, want %q", got, wantServer)
	}
	if serverCall.timeout != throughputServerTimeout {
		t.Errorf("server timeout = %s, want %s", serverCall.timeout, throughputServerTimeout)
	}
	wantClient := "--kill-after=1s 15s iperf3 -c 10.0.0.2 -p 5201 -t 10 -J"
	if got := strings.Join(clientCall.args, " "); got != wantClient {
		t.Errorf("client arguments = %q, want %q", got, wantClient)
	}
	if clientCall.timeout != 20*time.Second {
		t.Errorf("client timeout = %s, want 20s", clientCall.timeout)
	}
}

func TestMeasureThroughputClientFailure(t *testing.T) {
	runner, serverDone := throughputTestRunner()
	runner.boundedFn = func(ctx context.Context, hostID string, _ time.Duration, _ string, _ []string) (remote.Result, error) {
		switch hostID {
		case "server":
			<-ctx.Done()
			close(serverDone)
			return remote.Result{ExitCode: 124, Stdout: "Server listening on 10.0.0.2 5201\n"}, ctx.Err()
		case "client":
			return remote.Result{ExitCode: 1, Stderr: "iperf3: error - unable to connect to server: Connection refused\n"}, errors.New("exit status 1")
		}
		return remote.Result{}, fmt.Errorf("unexpected host %q", hostID)
	}
	out, err := measureThroughput(context.Background(), runner, throughputBaseArgs())
	if err != nil {
		t.Fatalf("connection failure should be diagnostic: %v", err)
	}
	for _, want := range []string{"Status: failed", "Connection refused", "Server log:", "Server listening"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
	select {
	case <-serverDone:
	default:
		t.Fatal("temporary iperf3 server was not stopped after client failure")
	}
}

func TestMeasureThroughputServerFailure(t *testing.T) {
	runner := &throughputFakeRunner{
		runFn: func(_ context.Context, _ string, program string, args []string) (remote.Result, error) {
			switch {
			case program == "iperf3" && len(args) == 1 && args[0] == "--version":
				return remote.Result{Stdout: "iperf 3.16\n"}, nil
			case program == "timeout" && len(args) == 1 && args[0] == "--version":
				return remote.Result{Stdout: "timeout (GNU coreutils) 9.4\n"}, nil
			case program == "ss":
				return remote.Result{}, nil
			}
			return remote.Result{}, fmt.Errorf("unexpected command %q", program)
		},
		boundedFn: func(_ context.Context, hostID string, _ time.Duration, _ string, _ []string) (remote.Result, error) {
			if hostID != "server" {
				return remote.Result{}, fmt.Errorf("unexpected host %q", hostID)
			}
			return remote.Result{ExitCode: 1, Stderr: "bind: address already in use\n"}, errors.New("exit status 1")
		},
	}
	_, err := measureThroughput(context.Background(), runner, throughputBaseArgs())
	if err == nil || !strings.Contains(err.Error(), "iperf3 server on \"server\" failed") || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("server failure = %v", err)
	}
	for _, call := range runner.snapshot() {
		if call.bounded && call.hostID == "client" {
			t.Fatal("client ran after the server failed to start")
		}
	}
}

func TestMeasureThroughputPreflightErrors(t *testing.T) {
	tests := []struct {
		name    string
		runFn   func(context.Context, string, string, []string) (remote.Result, error)
		wantErr string
	}{
		{
			name: "missing iperf3",
			runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
				if program == "iperf3" {
					return remote.Result{ExitCode: -1, Stderr: "executable file not found\n"}, errors.New("exec: iperf3: executable file not found")
				}
				return remote.Result{}, nil
			},
			wantErr: "iperf3 is required",
		},
		{
			name: "iperf2",
			runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
				if program == "iperf3" {
					return remote.Result{Stdout: "iperf version 2.1.9\n"}, nil
				}
				return remote.Result{}, nil
			},
			wantErr: "iperf3 3.x is required",
		},
		{
			name: "missing timeout",
			runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
				if program == "iperf3" {
					return remote.Result{Stdout: "iperf 3.16\n"}, nil
				}
				return remote.Result{ExitCode: -1}, errors.New("exec: timeout: executable file not found")
			},
			wantErr: "GNU timeout is required",
		},
		{
			name: "non-GNU timeout",
			runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
				switch program {
				case "iperf3":
					return remote.Result{Stdout: "iperf 3.16\n"}, nil
				case "timeout":
					return remote.Result{Stdout: "BusyBox v1.36.1 multi-call binary\n"}, nil
				}
				return remote.Result{}, nil
			},
			wantErr: "GNU timeout is required",
		},
		{
			name: "missing ss",
			runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
				switch program {
				case "iperf3":
					return remote.Result{Stdout: "iperf 3.16\n"}, nil
				case "timeout":
					return remote.Result{Stdout: "timeout (GNU coreutils) 9.4\n"}, nil
				}
				return remote.Result{ExitCode: -1}, errors.New("exec: ss: executable file not found")
			},
			wantErr: "ss (iproute2) is required",
		},
		{
			name: "port in use",
			runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
				switch program {
				case "iperf3":
					return remote.Result{Stdout: "iperf 3.16\n"}, nil
				case "timeout":
					return remote.Result{Stdout: "timeout (GNU coreutils) 9.4\n"}, nil
				case "ss":
					return remote.Result{Stdout: "LISTEN 0 128 10.0.0.2:5201 0.0.0.0:*\n"}, nil
				}
				return remote.Result{}, nil
			},
			wantErr: "already in use",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &throughputFakeRunner{runFn: tt.runFn}
			if _, err := measureThroughput(context.Background(), runner, throughputBaseArgs()); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("preflight error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestMeasureThroughputRejectsArguments(t *testing.T) {
	runner := &throughputFakeRunner{}
	tests := []struct {
		name    string
		mutate  func(*toolArgs)
		wantErr string
	}{
		{"missing server", func(a *toolArgs) { a.ServerHostID = "" }, "server_host_id is required"},
		{"missing client", func(a *toolArgs) { a.ClientHostID = "" }, "client_host_id is required"},
		{"unknown server", func(a *toolArgs) { a.ServerHostID = "other" }, "unknown host"},
		{"same host", func(a *toolArgs) { a.ClientHostID = a.ServerHostID }, "must differ"},
		{"missing destination", func(a *toolArgs) { a.Destination = "" }, "destination is required"},
		{"bad destination", func(a *toolArgs) { a.Destination = "bad/name" }, "destination must be"},
		{"missing bind", func(a *toolArgs) { a.BindAddress = "" }, "bind_address is required"},
		{"name as bind", func(a *toolArgs) { a.BindAddress = "example.com" }, "bind_address must be one IP"},
		{"unspecified bind", func(a *toolArgs) { a.BindAddress = "0.0.0.0" }, "unicast"},
		{"multicast bind", func(a *toolArgs) { a.BindAddress = "224.0.0.1" }, "unicast"},
		{"zoned bind", func(a *toolArgs) { a.BindAddress = "fe80::1%eth0" }, "without a zone"},
		{"port zero", func(a *toolArgs) { a.Port = throughputInt(0) }, "port must be 1..65535"},
		{"port high", func(a *toolArgs) { a.Port = throughputInt(65536) }, "port must be 1..65535"},
		{"duration zero", func(a *toolArgs) { a.Duration = throughputInt(0) }, "duration must be 1..10"},
		{"duration high", func(a *toolArgs) { a.Duration = throughputInt(11) }, "duration must be 1..10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := throughputBaseArgs()
			tt.mutate(&args)
			if _, err := measureThroughput(context.Background(), runner, args); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validation error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestMeasureThroughputInvalidJSON(t *testing.T) {
	runner, _ := throughputTestRunner()
	runner.boundedFn = func(ctx context.Context, hostID string, _ time.Duration, _ string, _ []string) (remote.Result, error) {
		switch hostID {
		case "server":
			<-ctx.Done()
			return remote.Result{ExitCode: 124}, ctx.Err()
		case "client":
			return remote.Result{Stdout: "not JSON\n"}, nil
		}
		return remote.Result{}, fmt.Errorf("unexpected host %q", hostID)
	}
	if _, err := measureThroughput(context.Background(), runner, throughputBaseArgs()); err == nil || !strings.Contains(err.Error(), "could not be parsed") {
		t.Fatalf("invalid JSON error = %v", err)
	}
}

func TestMeasureThroughputCancellation(t *testing.T) {
	runner, _ := throughputTestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	runner.boundedFn = func(ctx context.Context, hostID string, _ time.Duration, _ string, _ []string) (remote.Result, error) {
		switch hostID {
		case "server":
			<-ctx.Done()
			return remote.Result{ExitCode: 124}, ctx.Err()
		case "client":
			cancel()
			return remote.Result{}, context.Canceled
		}
		return remote.Result{}, fmt.Errorf("unexpected host %q", hostID)
	}
	if _, err := measureThroughput(ctx, runner, throughputBaseArgs()); err == nil || !strings.Contains(err.Error(), "was interrupted") {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestWaitForThroughputListenerTimeout(t *testing.T) {
	runner := &throughputFakeRunner{
		runFn: func(_ context.Context, _ string, _ string, _ []string) (remote.Result, error) {
			return remote.Result{}, nil
		},
	}
	_, started, err := waitForThroughputListener(context.Background(), runner, "server", "10.0.0.2", 5201, make(chan throughputRun), 50*time.Millisecond)
	if started || !errors.Is(err, errThroughputListener) {
		t.Fatalf("readiness timeout: started=%t err=%v", started, err)
	}
}

func TestWaitForThroughputListenerIgnoresOtherAddress(t *testing.T) {
	runner := &throughputFakeRunner{
		runFn: func(_ context.Context, _ string, program string, _ []string) (remote.Result, error) {
			if program == "ss" {
				return remote.Result{Stdout: "LISTEN 0 128 10.0.0.9:5201 0.0.0.0:*\n"}, nil
			}
			return remote.Result{}, nil
		},
	}
	_, started, err := waitForThroughputListener(context.Background(), runner, "server", "10.0.0.2", 5201, make(chan throughputRun), 50*time.Millisecond)
	if started || !errors.Is(err, errThroughputListener) {
		t.Fatalf("readiness on another address: started=%t err=%v", started, err)
	}
}

func TestIperf3Version(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"iperf 3.16 (cJSON 1.7.15)\n", true},
		{"iperf version 3.0.1\n", true},
		{"iperf version 2.1.9\n", false},
		{"iperf version 2.1.9 (OpenSSL 3.0.2)\n", false},
		{"garbage\n", false},
		{"iperf 2.1.9\ncJSON 3.1.0\n", false},
	} {
		if got := iperf3Version(tc.output); got != tc.want {
			t.Errorf("iperf3Version(%q) = %t, want %t", tc.output, got, tc.want)
		}
	}
}

func TestHumanBitrate(t *testing.T) {
	for _, tc := range []struct {
		bits float64
		want string
	}{
		{0, "0 bit/s"},
		{999, "999 bit/s"},
		{1500, "1.5 Kbit/s"},
		{1500000, "1.5 Mbit/s"},
		{1500000000, "1.50 Gbit/s"},
	} {
		if got := humanBitrate(tc.bits); got != tc.want {
			t.Errorf("humanBitrate(%v) = %q, want %q", tc.bits, got, tc.want)
		}
	}
}
