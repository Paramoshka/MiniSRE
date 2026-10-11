package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mini-sre/internal/remote"
)

const loadAveragePath = "/proc/loadavg"

func loadCommand() remote.Command {
	return remote.Command{Program: "cat", Args: []string{loadAveragePath}}
}

func (t *Tools) LoadAverage(ctx context.Context, hostID string) (string, error) {
	cmd := loadCommand()
	result, err := t.command(ctx, hostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", errors.New("tools: load average output was truncated")
	}
	return formatLoadAverage(result.Stdout)
}

func formatLoadAverage(raw string) (string, error) {
	fields := strings.Fields(raw)
	if len(fields) < 3 {
		return "", fmt.Errorf("tools: unexpected /proc/loadavg format: %q", raw)
	}

	out := fmt.Sprintf("load average: %s %s %s", fields[0], fields[1], fields[2])
	if len(fields) >= 4 {
		if procs := strings.SplitN(fields[3], "/", 2); len(procs) == 2 {
			out += fmt.Sprintf(" (running %s/%s)", procs[0], procs[1])
		}
	}
	return out, nil
}
