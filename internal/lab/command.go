package lab

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func checkCommand(check Check) ([]string, error) {
	if check.Interface != "" && check.Interface != "lo" && check.Interface != "eth0" && !interfacePattern.MatchString(check.Interface) {
		return nil, errors.New("lab: invalid diagnostic interface")
	}
	if check.Type != "ping" && check.Type != "traceroute" && check.Destination != "" {
		return nil, errors.New("lab: destination is only supported for ping and traceroute")
	}
	if check.Type != "tcpdump" && check.Type != "ping" && check.Interface != "" {
		return nil, errors.New("lab: interface is only supported for ping and tcpdump")
	}
	switch check.Type {
	case "interfaces":
		return []string{"ip", "-j", "address", "show"}, nil
	case "routes":
		return []string{"ip", "-j", "route", "show"}, nil
	case "neighbors":
		return []string{"ip", "-j", "neigh", "show"}, nil
	case "bgp":
		return []string{"vtysh", "-c", "show ip bgp summary json"}, nil
	case "ospf":
		return []string{"vtysh", "-c", "show ip ospf neighbor json"}, nil
	case "ping", "traceroute":
		addr, err := netip.ParseAddr(check.Destination)
		if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
			return nil, errors.New("lab: probe destination must be a unicast IPv4 address")
		}
		if check.Type == "traceroute" {
			return []string{"traceroute", "-n", "-m", "8", "-q", "1", "-w", "1", addr.String()}, nil
		}
		args := []string{"ping", "-n", "-c", "3", "-W", "2", "-w", "8"}
		if check.Interface != "" {
			args = append(args, "-I", check.Interface)
		}
		return append(args, addr.String()), nil
	case "tcpdump":
		iface := check.Interface
		if iface == "" {
			iface = "any"
		}
		return []string{"tcpdump", "-nn", "-p", "-c", "20", "-i", iface}, nil
	default:
		return nil, errors.New("lab: unknown check; use interfaces, routes, neighbors, bgp, ospf, ping, traceroute or tcpdump")
	}
}

// Commands and argv are constructed by the helper, never decoded from a request.
func execute(ctx context.Context, dir, program string, args ...string) CommandResult {
	deadline, ok := ctx.Deadline()
	if !ok {
		return CommandResult{ExitCode: -1, Stderr: "lab: execution deadline is required"}
	}
	seconds := time.Until(deadline).Seconds()
	if seconds < 0.001 {
		return CommandResult{ExitCode: -1, TimedOut: true}
	}
	// The host watchdog also survives an abruptly killed helper or lost SSH session.
	boundedArgs := append([]string{"--kill-after=1s", strconv.FormatFloat(seconds, 'f', 3, 64) + "s", "/usr/bin/" + program}, args...)
	cmd := exec.CommandContext(ctx, "/usr/bin/timeout", boundedArgs...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LC_ALL=C", "CLAB_VERSION_CHECK=disable"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	budget := &captureBudget{remaining: 64 * 1024}
	stdout, stderr := &capture{budget: budget}, &capture{budget: budget}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	result := CommandResult{Stdout: stdout.text.String(), Stderr: stderr.text.String(), ExitCode: 0, Truncated: stdout.truncated || stderr.truncated}
	if err != nil {
		result.ExitCode = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
		}
		if result.Stderr == "" {
			result.Stderr = err.Error()
		}
	}
	result.TimedOut = ctx.Err() != nil || result.ExitCode == 124 || result.ExitCode == 137
	return result
}

type captureBudget struct {
	sync.Mutex
	remaining int
}
type capture struct {
	budget    *captureBudget
	text      strings.Builder
	truncated bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.budget.Lock()
	defer c.budget.Unlock()
	n := len(p)
	if len(p) > c.budget.remaining {
		p = p[:c.budget.remaining]
		c.truncated = true
	}
	c.budget.remaining -= len(p)
	c.text.Write(p)
	return n, nil
}

func (m *Manager) command(ctx context.Context, dir, program string, args ...string) (string, error) {
	if program != "containerlab" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	result := m.run(ctx, dir, program, args...)
	if result.ExitCode != 0 || result.TimedOut || result.Truncated {
		return "", fmt.Errorf("lab: %s failed (exit=%d timeout=%t truncated=%t): %s%s", program, result.ExitCode, result.TimedOut, result.Truncated, result.Stdout, result.Stderr)
	}
	return result.Stdout, nil
}
