package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mini-sre/internal/remote"
)

const containerFormat = "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Status}}\t{{.Ports}}"

func containersCommand(args toolArgs) (remote.Command, int, error) {
	limit := 100
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 500 {
		return remote.Command{}, 0, errors.New("tools: limit must be 1..500")
	}
	flags := []string{"ps"}
	if !args.RunningOnly {
		flags = append(flags, "--all")
	}
	// --last bounds the daemon response to the requested number of containers.
	// It has no effect on selection: docker ps already returns newest first.
	flags = append(flags, "--last", strconv.Itoa(limit))
	if args.RunningOnly {
		flags = append(flags, "--filter", "status=running")
	}
	flags = append(flags, "--format", containerFormat)
	return remote.Command{Program: "docker", Args: flags}, limit, nil
}

func (t *Tools) Containers(ctx context.Context, args toolArgs) (string, error) {
	cmd, limit, err := containersCommand(args)
	if err != nil {
		return "", err
	}
	result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", fmt.Errorf("tools: docker container output was truncated before the requested %d rows; lower limit", limit)
	}
	rows := splitNonemptyLines(result.Stdout)
	scope := "all containers"
	if args.RunningOnly {
		scope = "running containers"
	}
	if len(rows) == 0 {
		if args.RunningOnly {
			return "No running containers found.", nil
		}
		return "No containers found.", nil
	}
	host := args.HostID
	if host == "" {
		host = "local"
	}
	count := min(limit, len(rows))
	var out strings.Builder
	fmt.Fprintf(&out, "Docker containers on %s (%s):\n", host, scope)
	fmt.Fprintf(&out, "ID\tNAME\tIMAGE\tSTATE\tSTATUS\tPORTS\n%s", strings.Join(rows[:count], "\n"))
	if len(rows) > limit {
		fmt.Fprintf(&out, "\n[container list limited to %d rows; additional containers may be omitted]", limit)
	}
	if result.Stderr != "" {
		fmt.Fprintf(&out, "\n%s", strings.TrimSpace(result.Stderr))
	}
	return out.String(), nil
}

func splitNonemptyLines(raw string) []string {
	var rows []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			rows = append(rows, line)
		}
	}
	return rows
}
