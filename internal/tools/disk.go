package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mini-sre/internal/remote"
)

func diskCommand(path string) remote.Command {
	if path == "" {
		path = "/"
	}

	return remote.Command{Program: "df", Args: []string{"-B1", "--output=size,used,avail", "--", path}}
}

func (t *Tools) DiskUsage(ctx context.Context, hostID, path string) (string, error) {
	cmd := diskCommand(path)
	result, err := t.command(ctx, hostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", errors.New("tools: disk usage output was truncated")
	}
	rows := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(rows) != 2 {
		return "", errors.New("tools: unexpected df output")
	}
	fields := strings.Fields(rows[1])
	if len(fields) != 3 {
		return "", errors.New("tools: unexpected df columns")
	}
	var values [3]uint64
	for i, field := range fields {
		values[i], err = strconv.ParseUint(field, 10, 64)
		if err != nil {
			return "", errors.New("tools: invalid disk usage numbers")
		}
	}
	total, used, available := values[0], values[1], values[2]
	usedPercent := 0.0
	if total > 0 {
		usedPercent = float64(used) / float64(total) * 100
	}

	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: total=%s used=%s available=%s use=%.1f%%",
		path, humanBytes(total), humanBytes(used), humanBytes(available), usedPercent), nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
