package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func validateFilePath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsRune(path, '\x00') {
		return errors.New("tools: path must be absolute and must not contain NUL bytes")
	}
	for _, component := range strings.Split(path, "/") {
		if strings.HasPrefix(component, ".") {
			return errors.New("tools: hidden path components, . and .. are not allowed")
		}
	}
	return nil
}

func (t *Tools) checkedFilePath(ctx context.Context, hostID, path string) (string, error) {
	if err := validateFilePath(path); err != nil {
		return "", err
	}
	result, err := t.command(ctx, hostID, "readlink", "--canonicalize-existing", "--zero", "--", path)
	if err != nil {
		return "", err
	}
	if result.Truncated || !strings.HasSuffix(result.Stdout, "\x00") {
		return "", errors.New("tools: readlink returned an incomplete path")
	}
	resolved := strings.TrimSuffix(result.Stdout, "\x00")
	if err := validateFilePath(resolved); err != nil {
		return "", err
	}
	result, err = t.command(ctx, hostID, "stat", "--format=%f", "--", resolved)
	if err != nil {
		return "", err
	}
	mode, err := strconv.ParseUint(strings.TrimSpace(result.Stdout), 16, 32)
	if err != nil || result.Truncated {
		return "", errors.New("tools: stat returned an invalid file mode")
	}
	// GNU stat's hexadecimal mode includes the POSIX file type bits.
	if mode&0170000 != 0100000 {
		return "", errors.New("tools: path must refer to a regular file")
	}
	return resolved, nil
}

func (t *Tools) ReadFile(ctx context.Context, args toolArgs) (string, error) {
	lines, start := 100, 1
	if args.Lines != nil {
		lines = *args.Lines
	}
	if args.StartLine != nil {
		start = *args.StartLine
	}
	if lines < 1 || lines > 500 || start < 1 {
		return "", errors.New("tools: lines must be 1..500 and start_line must be positive")
	}
	if args.Tail && args.StartLine != nil {
		return "", errors.New("tools: start_line cannot be used with tail=true")
	}
	if start > math.MaxInt-(lines-1) {
		return "", errors.New("tools: requested line range is too large")
	}
	path, err := t.checkedFilePath(ctx, args.HostID, args.Path)
	if err != nil {
		return "", err
	}
	program := "sed"
	flags := []string{"--sandbox", "-n", fmt.Sprintf("%d,%dp;%dq", start, start+lines-1, start+lines-1), "--", path}
	if args.Tail {
		program = "tail"
		flags = []string{"-n", strconv.Itoa(lines), "--", path}
	}
	result, err := t.command(ctx, args.HostID, program, flags...)
	if err != nil {
		return "", err
	}
	if result.Stdout == "" && result.Stderr == "" {
		return "No lines in the requested range.", nil
	}
	return commandOutput(result), nil
}

func (t *Tools) SearchFile(ctx context.Context, args toolArgs) (string, error) {
	if args.Pattern == "" || strings.ContainsRune(args.Pattern, '\x00') {
		return "", errors.New("tools: pattern must be nonempty and must not contain NUL bytes")
	}
	limit := 100
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 500 {
		return "", errors.New("tools: limit must be 1..500")
	}
	path, err := t.checkedFilePath(ctx, args.HostID, args.Path)
	if err != nil {
		return "", err
	}
	mode := "-F"
	if args.Regex {
		mode = "-E"
	}
	flags := []string{mode, "-n", "--color=never", "-m", strconv.Itoa(limit)}
	if args.IgnoreCase {
		flags = append(flags, "-i")
	}
	flags = append(flags, "-e", args.Pattern, "--", path)
	result, err := t.command(ctx, args.HostID, "grep", flags...)
	if err != nil {
		if result.ExitCode == 1 && result.Stdout == "" && result.Stderr == "" && !result.Truncated {
			return "No matching lines.", nil
		}
		return "", err
	}
	return commandOutput(result), nil
}
