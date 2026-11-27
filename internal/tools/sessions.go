package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"mini-sre/internal/remote"
)

// logind session IDs are numeric for login sessions and cN for seat sessions.
var logindSessionID = regexp.MustCompile(`^(?:[0-9]+|c[0-9]+)$`)

func sessionsCommands() []remote.Command {
	return []remote.Command{
		// -i shows IP addresses on non-systemd hosts; systemd builds ignore it
		// and report the remote host as registered by the login program.
		{Program: "w", Args: []string{"-i"}},
		{Program: "loginctl", Args: []string{"list-sessions", "--no-legend", "--no-pager"}},
	}
}

func sessionStatusCommand(id string) remote.Command {
	return remote.Command{Program: "loginctl", Args: []string{"session-status", id, "--no-pager"}}
}

// UserSessions reports active login sessions, or one session's details when
// session_id is set. Unavailable sources are reported as limitations.
func (t *Tools) UserSessions(ctx context.Context, args toolArgs) (string, error) {
	host := args.HostID
	if host == "" {
		host = "local"
	}
	if args.SessionID != "" {
		if len(args.SessionID) > 16 || !logindSessionID.MatchString(args.SessionID) {
			return "", errors.New("tools: session_id must be a logind session ID such as 41 or c1")
		}
		cmd := sessionStatusCommand(args.SessionID)
		result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
		if err != nil {
			return "", err
		}
		return commandOutput(result), nil
	}

	var parts, failures []string
	for _, cmd := range sessionsCommands() {
		result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s unavailable: %v", cmd.Program, err))
			continue
		}
		out := strings.TrimRight(commandOutput(result), "\n")
		switch cmd.Program {
		case "w":
			parts = append(parts, "w (user, TTY, from, login time, idle, current command):\n"+out)
		case "loginctl":
			if strings.TrimSpace(out) == "" {
				out = "No logind sessions reported."
			}
			parts = append(parts, "logind sessions (pass session_id for details):\n"+out)
		}
	}
	if len(parts) == 0 {
		return "", errors.New("tools: active sessions unavailable; " + strings.Join(failures, "; "))
	}
	out := fmt.Sprintf("Active sessions on %s:\n%s", host, strings.Join(parts, "\n\n"))
	if len(failures) > 0 {
		out += "\n\nLimitations: " + strings.Join(failures, "; ")
	}
	return out, nil
}
