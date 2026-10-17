package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"mini-sre/internal/remote"
)

const tcpConnectScript = `exec 3<>/dev/tcp/"$1"/"$2" && printf 'connected\n'`

func validateConnectDestination(destination string) error {
	if destination == "" {
		return errors.New("tools: destination is required")
	}
	if address, err := netip.ParseAddr(destination); err == nil {
		if address.Zone() != "" {
			return errors.New("tools: destination must not have a zone")
		}
		return nil
	}
	name := strings.TrimSuffix(destination, ".")
	if name == "" || len(name) > 253 {
		return errors.New("tools: destination must be one IP address or DNS hostname")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("tools: destination must be one IP address or DNS hostname")
		}
		for _, char := range label {
			if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return errors.New("tools: destination must be one IP address or DNS hostname")
			}
		}
	}
	return nil
}

func tcpCommand(args toolArgs) (remote.Command, error) {
	if err := validateConnectDestination(args.Destination); err != nil {
		return remote.Command{}, err
	}
	if args.Port == nil {
		return remote.Command{}, errors.New("tools: port is required")
	}
	if *args.Port < 1 || *args.Port > 65535 {
		return remote.Command{}, errors.New("tools: port must be 1..65535")
	}
	return remote.Command{Program: "timeout", Args: []string{
		"5", "bash", "--noprofile", "--norc", "-c", tcpConnectScript, "mini-sre-tcp",
		args.Destination, strconv.Itoa(*args.Port),
	}}, nil
}

func (t *Tools) TCPConnect(ctx context.Context, args toolArgs) (string, error) {
	cmd, err := tcpCommand(args)
	if err != nil {
		return "", err
	}
	start := time.Now()
	result, err := t.Runner.Run(ctx, args.HostID, cmd.Program, cmd.Args...)
	elapsed := time.Since(start)
	if err != nil && result.ExitCode != 1 && result.ExitCode != 124 {
		return "", fmt.Errorf("tools: TCP probe could not run on the selected host (Bash and timeout are required): %w; %s", err, commandOutput(result))
	}
	host := args.HostID
	if host == "" {
		host = "local"
	}
	target := net.JoinHostPort(args.Destination, strconv.Itoa(*args.Port))
	status := "connected"
	switch {
	case result.ExitCode == 124:
		status = "timed out"
	case err != nil:
		status = "failed"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "TCP connect from %s to %s: %s (exit code %d, elapsed %s including command startup)\n",
		host, target, status, result.ExitCode, elapsed.Round(time.Millisecond))
	if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
		out.WriteString(stderr)
		out.WriteString("\n")
	}
	if result.Truncated {
		out.WriteString("[output truncated at 64 KiB]\n")
	}
	out.WriteString("A successful TCP handshake does not prove application health; no application data is sent.")
	return out.String(), nil
}
