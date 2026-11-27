package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"mini-sre/internal/remote"
)

var loginUserName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)

// Storage inspected for the sources footer. Absence is reported as a fact:
// a volatile journal (no /var/log/journal) is the Debian 13 default, and
// wtmp.db only exists once wtmpdb is installed and has recorded a login.
// wtmpdb lives at /var/log/wtmp.db on Debian but /var/lib/wtmpdb/wtmp.db
// upstream; rsyslog auth logs are auth.log on Debian-family and secure on
// RHEL-family hosts.
var loginStoragePaths = []string{
	"/var/log/journal",
	"/var/log/wtmp.db",
	"/var/lib/wtmpdb/wtmp.db",
	"/var/log/wtmp",
	"/var/log/audit/audit.log",
	"/var/log/auth.log",
	"/var/log/secure",
}

// sshAuthEventsPattern matches openssh auth_log() messages: "Accepted <method>
// for ...", "Failed <method> for [invalid user] ..." and "Invalid user ...".
const sshAuthEventsPattern = "Accepted|Failed|Invalid user"

func sshAuthEventsCommand(limit, minutes int) remote.Command {
	// OpenSSH >= 9.8 logs auth events as sshd-session, the listener as sshd;
	// matching both identifiers also covers socket-activated sshd@ units.
	return remote.Command{Program: "journalctl", Args: []string{
		"--no-pager", "--output=short-iso",
		"--lines=" + strconv.Itoa(limit),
		"--since=-" + strconv.Itoa(minutes) + "min",
		"-t", "sshd", "-t", "sshd-session",
		"-g", sshAuthEventsPattern,
	}}
}

// loginRecordsCommand builds the variant-independent part of last/wtmpdb last:
// both accept -n, -i, -w and an absolute "YYYY-MM-DD HH:MM:SS" -s timestamp.
func loginRecordsCommand(program string, limit int, since, user string) remote.Command {
	var args []string
	if program == "wtmpdb" {
		args = append(args, "last")
	}
	args = append(args, "-n", strconv.Itoa(limit), "-i", "-w")
	if since != "" {
		args = append(args, "-s", since)
	}
	if user != "" {
		args = append(args, user)
	}
	return remote.Command{Program: program, Args: args}
}

// LoginHistory reports recent login records and SSH authentication events.
// Login records come from wtmpdb (Debian 13+) or util-linux last; when neither
// exists (Ubuntu 25.04+) only journal events are returned. Unavailable sources
// and storage facts are reported in a footer, not as execution errors.
func (t *Tools) LoginHistory(ctx context.Context, args toolArgs) (string, error) {
	limit, minutes := 50, 1440
	if args.Limit != nil {
		limit = *args.Limit
	}
	if args.SinceMinutes != nil {
		minutes = *args.SinceMinutes
	}
	if limit < 1 || limit > 500 {
		return "", errors.New("tools: limit must be 1..500")
	}
	if minutes < 1 || minutes > 43200 {
		return "", errors.New("tools: since_minutes must be 1..43200")
	}
	if args.User != "" && !loginUserName.MatchString(args.User) {
		return "", errors.New("tools: user must be a login name of at most 32 characters, without spaces or a leading dash")
	}
	host := args.HostID
	if host == "" {
		host = "local"
	}

	program := "last"
	if _, err := t.command(ctx, args.HostID, "wtmpdb", "--version"); err == nil {
		program = "wtmpdb"
	}

	// Compute the cutoff on the host: wtmpdb accepts only absolute times, and
	// the host clock and timezone are authoritative for its records.
	since := ""
	if result, err := t.command(ctx, args.HostID, "date", "-d", "-"+strconv.Itoa(minutes)+" minutes", "+%Y-%m-%d %H:%M:%S"); err == nil {
		since = strings.TrimSpace(result.Stdout)
	}

	var sections, notes []string
	recordsOK, journalOK := false, false

	label := "last (/var/log/wtmp)"
	if program == "wtmpdb" {
		// Do not assert a DB path: Debian patches it to /var/log/wtmp.db,
		// upstream defaults to /var/lib/wtmpdb/wtmp.db. The footer shows
		// which storage actually exists.
		label = "wtmpdb last (SQLite)"
	}
	cmd := loginRecordsCommand(program, limit, since, args.User)
	if result, err := t.command(ctx, args.HostID, cmd.Program, cmd.Args...); err == nil {
		recordsOK = true
		out := strings.TrimRight(commandOutput(result), "\n")
		if strings.TrimSpace(out) == "" {
			out = "No login records in the requested interval."
		}
		sections = append(sections, "Login records via "+label+":\n"+out)
	} else {
		notes = append(notes, fmt.Sprintf("login records unavailable (%s): %v", label, err))
	}
	if since == "" {
		notes = append(notes, "host time filter unavailable; login records are bounded by limit only")
	}

	jcmd := sshAuthEventsCommand(limit, minutes)
	if result, err := t.command(ctx, args.HostID, jcmd.Program, jcmd.Args...); err == nil {
		journalOK = true
		out := strings.TrimRight(commandOutput(result), "\n")
		if strings.TrimSpace(out) == "" {
			out = "No SSH authentication events in the requested interval."
		}
		sections = append(sections, "SSH authentication events (sshd, sshd-session):\n"+out)
	} else {
		notes = append(notes, fmt.Sprintf("SSH journal events unavailable: %v", err))
	}

	if !recordsOK && !journalOK {
		return "", errors.New("tools: login history unavailable; " + strings.Join(notes, "; "))
	}

	out := fmt.Sprintf("Login history on %s (last %d minutes, limit %d):\n\n%s",
		host, minutes, limit, strings.Join(sections, "\n\n"))
	out += "\n\n" + t.loginStorageFooter(ctx, args.HostID)
	if len(notes) > 0 {
		out += "\n\nLimitations: " + strings.Join(notes, "; ")
	}
	return out, nil
}

// loginStorageFooter reports presence, mode and owner of the login-audit
// storage. stat exits 1 when some operands fail; only "No such file or
// directory" proves absence, while EACCES (e.g. Debian's 0750 root:adm
// /var/log/audit) means the status is unknown. Absent paths are reported as
// facts, not accusations.
func (t *Tools) loginStorageFooter(ctx context.Context, hostID string) string {
	// -L dereferences symlinks: Debian's /var/lib/wtmpdb/wtmp.db links to
	// /var/log/wtmp.db, and the target's mode governs readability.
	statArgs := append([]string{"-Lc", "%n mode=%a owner=%U:%G"}, loginStoragePaths...)
	result, err := t.Runner.Run(ctx, hostID, "stat", statArgs...)
	present := make(map[string]string)
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			present[fields[0]] = strings.Join(fields[1:], " ")
		}
	}
	// GNU stat (LC_ALL=C) reports each failed operand on stderr as
	// "stat: cannot stat[x] '<path>': <reason>". Match the quoted path
	// exactly so /var/log/wtmp does not match /var/log/wtmp.db lines.
	failed := make(map[string]string)
	for _, line := range strings.Split(result.Stderr, "\n") {
		for _, path := range loginStoragePaths {
			marker := "'" + path + "': "
			idx := strings.Index(line, marker)
			if idx < 0 {
				continue
			}
			if reason := strings.TrimSpace(line[idx+len(marker):]); reason != "No such file or directory" {
				failed[path] = reason
			}
		}
	}
	var lines []string
	for _, path := range loginStoragePaths {
		info, ok := present[path]
		switch {
		case ok && path == "/var/log/journal":
			lines = append(lines, path+": "+info+" (persistent journal)")
		case ok:
			lines = append(lines, path+": "+info)
		case failed[path] != "":
			lines = append(lines, path+": status unknown ("+failed[path]+"; a check limitation, not evidence of absence)")
		case path == "/var/log/journal":
			lines = append(lines, path+": absent (volatile journal; history is lost at reboot; absence alone is not evidence of tampering)")
		case path == "/var/log/audit/audit.log":
			lines = append(lines, path+": absent (auditd may not be installed)")
		default:
			lines = append(lines, path+": absent")
		}
	}
	// stat exits 1 when some operands are missing or unreadable; that is
	// expected data, not an execution failure. Any other failure means the
	// inspection itself broke.
	if err != nil && result.ExitCode != 1 {
		lines = append(lines, fmt.Sprintf("storage inspection unavailable: %v", err))
	}
	return "Sources and storage:\n" + strings.Join(lines, "\n")
}
