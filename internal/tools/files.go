package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"mini-sre/internal/remote"
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
	components := strings.FieldsFunc(path, func(c rune) bool { return c == '/' })
	if len(components) > 0 && components[0] == "proc" {
		for _, component := range components[1:] {
			switch component {
			case "environ", "cmdline", "mem", "fd", "fdinfo":
				return errors.New("tools: sensitive process files in /proc are not allowed")
			}
		}
	}
	return nil
}

func (t *Tools) checkedPath(ctx context.Context, hostID, path string, directory bool) (string, error) {
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
	if directory {
		if mode&0170000 != 0040000 {
			return "", errors.New("tools: path must refer to a directory")
		}
	} else if mode&0170000 != 0100000 {
		return "", errors.New("tools: path must refer to a regular file")
	}
	return resolved, nil
}

func (t *Tools) checkedFilePath(ctx context.Context, hostID, path string) (string, error) {
	return t.checkedPath(ctx, hostID, path, false)
}

func (t *Tools) checkedDirectoryPath(ctx context.Context, hostID, path string) (string, error) {
	return t.checkedPath(ctx, hostID, path, true)
}

func (t *Tools) readFileCommand(ctx context.Context, args toolArgs) (remote.Command, error) {
	lines, start := 100, 1
	if args.Lines != nil {
		lines = *args.Lines
	}
	if args.StartLine != nil {
		start = *args.StartLine
	}
	if lines < 1 || lines > 500 || start < 1 {
		return remote.Command{}, errors.New("tools: lines must be 1..500 and start_line must be positive")
	}
	if args.Tail && args.StartLine != nil {
		return remote.Command{}, errors.New("tools: start_line cannot be used with tail=true")
	}
	if start > math.MaxInt-(lines-1) {
		return remote.Command{}, errors.New("tools: requested line range is too large")
	}
	path, err := t.checkedFilePath(ctx, args.HostID, args.Path)
	if err != nil {
		return remote.Command{}, err
	}
	program := "sed"
	flags := []string{"--sandbox", "-n", fmt.Sprintf("%d,%dp;%dq", start, start+lines-1, start+lines-1), "--", path}
	if args.Tail {
		program = "tail"
		flags = []string{"-n", strconv.Itoa(lines), "--", path}
	}
	return remote.Command{Program: program, Args: flags}, nil
}

func (t *Tools) ReadFile(ctx context.Context, args toolArgs) (string, error) {
	cmd, err := t.readFileCommand(ctx, args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	if result.Stdout == "" && result.Stderr == "" {
		return "No lines in the requested range.", nil
	}
	return commandOutput(result), nil
}

func (t *Tools) searchFileCommand(ctx context.Context, args toolArgs) (remote.Command, error) {
	if args.Pattern == "" || strings.ContainsRune(args.Pattern, '\x00') {
		return remote.Command{}, errors.New("tools: pattern must be nonempty and must not contain NUL bytes")
	}
	limit := 100
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 500 {
		return remote.Command{}, errors.New("tools: limit must be 1..500")
	}
	path, err := t.checkedFilePath(ctx, args.HostID, args.Path)
	if err != nil {
		return remote.Command{}, err
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
	return remote.Command{Program: "grep", Args: flags}, nil
}

func (t *Tools) SearchFile(ctx context.Context, args toolArgs) (string, error) {
	cmd, err := t.searchFileCommand(ctx, args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		if result.ExitCode == 1 && result.Stdout == "" && result.Stderr == "" && !result.Truncated {
			return "No matching lines.", nil
		}
		return "", err
	}
	return commandOutput(result), nil
}
