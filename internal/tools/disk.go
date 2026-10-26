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

	return remote.Command{Program: "df", Args: []string{"-B1", "--output=size,used,avail,itotal,iused,iavail", "--", path}}
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
	if len(fields) != 6 {
		return "", errors.New("tools: unexpected df columns")
	}
	var values [3]uint64
	for i, field := range fields[:3] {
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
	out := fmt.Sprintf("%s: total=%s used=%s available=%s use=%.1f%%",
		path, humanBytes(total), humanBytes(used), humanBytes(available), usedPercent)
	var inodes [3]uint64
	availableInodes := true
	for i, field := range fields[3:] {
		if field == "-" {
			availableInodes = false
			continue
		}
		inodes[i], err = strconv.ParseUint(field, 10, 64)
		if err != nil {
			return "", errors.New("tools: invalid inode usage numbers")
		}
	}
	if !availableInodes || inodes[0] == 0 {
		return out + "\ninodes: unavailable on this filesystem", nil
	}
	return out + fmt.Sprintf("\ninodes: total=%d used=%d available=%d use=%.1f%%",
		inodes[0], inodes[1], inodes[2], float64(inodes[1])/float64(inodes[0])*100), nil
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
