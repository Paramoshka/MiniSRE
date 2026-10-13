package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"mini-sre/internal/remote"
)

func processCommand(args toolArgs) (remote.Command, string, int, error) {
	sortBy := args.SortBy
	if sortBy == "" {
		sortBy = "memory"
	}
	sortKey := ""
	switch sortBy {
	case "memory":
		sortKey = "-rss"
	case "cpu":
		sortKey = "-pcpu"
	default:
		return remote.Command{}, "", 0, errors.New("tools: sort_by must be memory or cpu")
	}
	limit := 10
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 50 {
		return remote.Command{}, "", 0, errors.New("tools: limit must be 1..50")
	}
	return remote.Command{Program: "ps", Args: []string{"-eo", "pid,user,pcpu,pmem,rss,comm", "--sort=" + sortKey, "--cols=256"}}, sortBy, limit, nil
}

func (t *Tools) TopProcesses(ctx context.Context, args toolArgs) (string, error) {
	cmd, sortBy, limit, err := processCommand(args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	rows := strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n")
	if result.Truncated && !strings.HasSuffix(result.Stdout, "\n") {
		rows = rows[:len(rows)-1]
	}
	if len(rows) < 2 {
		return "", errors.New("tools: ps returned no complete process rows")
	}
	if result.Truncated && len(rows)-1 < limit {
		return "", errors.New("tools: ps output was truncated before the requested top processes")
	}
	count := min(limit, len(rows)-1)
	return fmt.Sprintf("Top %d processes by %s (CPU: lifetime average; RSS: KiB):\n%s",
		count, sortBy, strings.Join(rows[:count+1], "\n")), nil
}

func processSearchCommand(args toolArgs) (remote.Command, int, error) {
	if args.Pattern == "" || len(args.Pattern) > 128 || strings.ContainsAny(args.Pattern, "\x00\r\n") {
		return remote.Command{}, 0, errors.New("tools: pattern must be a nonempty literal fragment of at most 128 bytes, without NUL or newlines")
	}
	limit := 100
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 500 {
		return remote.Command{}, 0, errors.New("tools: limit must be 1..500")
	}
	return remote.Command{Program: "pgrep", Args: []string{"-i", "-l", "--", regexp.QuoteMeta(args.Pattern)}}, limit, nil
}

func (t *Tools) SearchProcesses(ctx context.Context, args toolArgs) (string, error) {
	cmd, limit, err := processSearchCommand(args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	const scope = "Only visible process names are searched, not command lines; names may be limited to 15 characters. Use a short name fragment."
	if err != nil {
		if result.ExitCode == 1 && result.Stdout == "" && !result.Truncated {
			out := "No visible process names matched. " + scope
			if result.Stderr != "" {
				out += "\n" + result.Stderr
			}
			return out, nil
		}
		return "", err
	}
	rows := strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n")
	if result.Truncated && !strings.HasSuffix(result.Stdout, "\n") {
		rows = rows[:len(rows)-1]
	}
	if len(rows) == 0 || rows[0] == "" {
		return "", errors.New("tools: pgrep returned no complete process rows")
	}
	count := min(limit, len(rows))
	out := fmt.Sprintf("Visible processes matching %q (PID, name):\n%s\n%s", t.Runner.Redact(args.Pattern), strings.Join(rows[:count], "\n"), scope)
	if len(rows) > limit || result.Truncated {
		out += "\n[matching process output limited; additional matches may be omitted]"
	}
	if result.Stderr != "" {
		out += "\n" + result.Stderr
	}
	return out, nil
}
