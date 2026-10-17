package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mini-sre/internal/remote"
)

const directoryFormat = `%y\0%s\0%T@\0%f\0%l\0`

type directoryEntry struct {
	kind     string
	size     uint64
	modified time.Time
	name     string
	target   string
}

func directoryLimit(args toolArgs) (int, error) {
	limit := 100
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 500 {
		return 0, errors.New("tools: limit must be 1..500")
	}
	return limit, nil
}

func (t *Tools) directoryCommand(ctx context.Context, args toolArgs) (remote.Command, error) {
	if _, err := directoryLimit(args); err != nil {
		return remote.Command{}, err
	}
	path, err := t.checkedDirectoryPath(ctx, args.HostID, args.Path)
	if err != nil {
		return remote.Command{}, err
	}
	return remote.Command{Program: "find", Args: []string{
		path, "-mindepth", "1", "-maxdepth", "1", "!", "-name", ".*", "-printf", directoryFormat,
	}}, nil
}

func (t *Tools) Directory(ctx context.Context, args toolArgs) (string, error) {
	limit, err := directoryLimit(args)
	if err != nil {
		return "", err
	}
	cmd, err := t.directoryCommand(ctx, args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	entries, complete := parseDirectory(result.Stdout)
	if result.Truncated {
		complete = false
	}
	host := args.HostID
	if host == "" {
		host = "local"
	}
	if len(entries) == 0 {
		out := fmt.Sprintf("Directory %s on %s is empty (hidden entries are not listed).", args.Path, host)
		if !complete {
			out = fmt.Sprintf("Directory %s on %s returned no complete entries; the listing was truncated.", args.Path, host)
		}
		return out, nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	count := min(limit, len(entries))
	var out strings.Builder
	fmt.Fprintf(&out, "Directory %s on %s (hidden entries are not listed):\n", args.Path, host)
	fmt.Fprintf(&out, "type size modified name\n")
	for _, entry := range entries[:count] {
		size := "-"
		if entry.kind == "f" {
			size = humanBytes(entry.size)
		}
		modified := "-"
		if !entry.modified.IsZero() {
			modified = entry.modified.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&out, "%s %s %s %s", directoryKind(entry.kind), size, modified, sanitizeName(entry.name))
		if entry.kind == "l" {
			if hiddenTarget(entry.target) {
				out.WriteString(" -> [hidden target]")
			} else {
				out.WriteString(" -> " + sanitizeName(entry.target))
			}
		}
		out.WriteString("\n")
	}
	if len(entries) > limit {
		fmt.Fprintf(&out, "[listing limited to %d entries; %d additional entries omitted]\n", limit, len(entries)-limit)
	}
	if !complete {
		out.WriteString("[listing incomplete; output was truncated]\n")
	}
	return out.String(), nil
}

func parseDirectory(raw string) ([]directoryEntry, bool) {
	fields := strings.Split(raw, "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	complete := len(fields)%5 == 0
	var entries []directoryEntry
	for index := 0; index+4 < len(fields); index += 5 {
		kind := fields[index]
		if kind == "" {
			continue
		}
		size, _ := strconv.ParseUint(fields[index+1], 10, 64)
		var modified time.Time
		if seconds, err := strconv.ParseFloat(fields[index+2], 64); err == nil {
			whole, fraction := math.Modf(seconds)
			modified = time.Unix(int64(whole), int64(fraction*1e9))
		}
		entries = append(entries, directoryEntry{
			kind:     kind,
			size:     size,
			modified: modified,
			name:     fields[index+3],
			target:   fields[index+4],
		})
	}
	return entries, complete
}

func directoryKind(kind string) string {
	switch kind {
	case "d":
		return "dir"
	case "f":
		return "file"
	case "l":
		return "link"
	case "s":
		return "socket"
	case "p":
		return "fifo"
	case "c", "b":
		return "device"
	default:
		return kind
	}
}

func hiddenTarget(target string) bool {
	for _, component := range strings.Split(target, "/") {
		if strings.HasPrefix(component, ".") {
			return true
		}
	}
	return false
}

func sanitizeName(name string) string {
	var out strings.Builder
	for _, char := range name {
		switch {
		case char == '\\':
			out.WriteString(`\\`)
		case char < 0x20 || char == 0x7f:
			fmt.Fprintf(&out, `\x%02x`, char)
		default:
			out.WriteRune(char)
		}
	}
	return out.String()
}
