package tools

import (
	"context"
	"errors"
	"strings"

	"mini-sre/internal/remote"
)

func vmstatCommand() remote.Command {
	return remote.Command{Program: "vmstat", Args: []string{"-w", "-n", "-y", "-S", "K", "1", "3"}}
}

func (t *Tools) VMStat(ctx context.Context, hostID string) (string, error) {
	cmd := vmstatCommand()
	result, err := t.command(ctx, hostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", errors.New("tools: vmstat output was truncated")
	}
	// procps-ng with -n -y emits two header lines followed by three interval samples.
	if len(strings.Split(strings.TrimSpace(result.Stdout), "\n")) != 5 {
		return "", errors.New("tools: vmstat did not return three interval samples with headers")
	}
	return "vmstat: 3 samples at 1-second intervals; memory and swap: KiB; since-boot report omitted.\n" + result.Stdout, nil
}
