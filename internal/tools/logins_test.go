package tools

import (
	"context"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

const testSSHEvent = "2026-11-27T11:00:00+0300 host sshd-session[123]: Accepted publickey for ivan from 10.0.0.1 port 54321 ssh2\n"

// loginHistoryFakes installs deterministic date, journalctl and stat fakes;
// extra overrides or adds programs such as wtmpdb and last.
func loginHistoryFakes(t *testing.T, extra map[string]fakeProgram) string {
	t.Helper()
	programs := map[string]fakeProgram{
		"date":       {stdout: "2026-11-26 12:00:00\n", exit: "0"},
		"journalctl": {stdout: testSSHEvent, exit: "0"},
		"stat": {
			stdout: "/var/log/journal mode=2755 owner=root:systemd-journal\n" +
				"/var/log/wtmp.db mode=644 owner=root:root\n",
			stderr: "stat: cannot statx '/var/log/wtmp': No such file or directory\n",
			exit:   "1",
		},
	}
	for name, p := range extra {
		programs[name] = p
	}
	return fakePrograms(t, programs)
}

func wtmpdbFake(records string) fakeProgram {
	return fakeProgram{body: "case \"$1\" in\n" +
		"--version) printf 'wtmpdb 0.73.0\\n';;\n" +
		"*) printf '%s' " + fakeQuote(records) + ";;\n" +
		"esac\nexit 0\n"}
}

func TestLoginHistoryWtmpdb(t *testing.T) {
	records := "ivan     pts/0    10.0.0.1         Fri Nov 27 11:00   still logged in\n\nwtmpdb begins Fri Nov 20 10:00:00 2026\n"
	dir := loginHistoryFakes(t, map[string]fakeProgram{"wtmpdb": wtmpdbFake(records)})
	out, err := (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{})
	if err != nil {
		t.Fatalf("LoginHistory: %v", err)
	}
	for _, want := range []string{
		"Login history on local (last 1440 minutes, limit 50):",
		"Login records via wtmpdb last (SQLite):",
		"still logged in",
		"SSH authentication events (sshd, sshd-session):",
		"Accepted publickey for ivan",
		"Sources and storage:",
		"/var/log/journal: mode=2755 owner=root:systemd-journal (persistent journal)",
		"/var/log/wtmp.db: mode=644 owner=root:root",
		"/var/lib/wtmpdb/wtmp.db: absent",
		"/var/log/wtmp: absent",
		"/var/log/audit/audit.log: absent (auditd may not be installed)",
		"/var/log/auth.log: absent",
		"/var/log/secure: absent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("login history missing %q:\n%s", want, out)
		}
	}
	if got := fakeArgs(t, dir, "wtmpdb"); got != "last\n-n\n50\n-i\n-w\n-s\n2026-11-26 12:00:00\n" {
		t.Errorf("wtmpdb last args = %q", got)
	}
	if got := fakeArgs(t, dir, "date"); got != "-d\n-1440 minutes\n+%Y-%m-%d %H:%M:%S\n" {
		t.Errorf("date args = %q", got)
	}
	wantJournal := "--no-pager\n--output=short-iso\n--lines=50\n--since=-1440min\n-t\nsshd\n-t\nsshd-session\n-g\nAccepted|Failed|Invalid user\n"
	if got := fakeArgs(t, dir, "journalctl"); got != wantJournal {
		t.Errorf("journalctl args = %q, want %q", got, wantJournal)
	}
	wantStat := "-Lc\n%n mode=%a owner=%U:%G\n/var/log/journal\n/var/log/wtmp.db\n/var/lib/wtmpdb/wtmp.db\n/var/log/wtmp\n/var/log/audit/audit.log\n/var/log/auth.log\n/var/log/secure\n"
	if got := fakeArgs(t, dir, "stat"); got != wantStat {
		t.Errorf("stat args = %q, want %q", got, wantStat)
	}
}

func TestLoginHistoryClassicLast(t *testing.T) {
	dir := loginHistoryFakes(t, map[string]fakeProgram{
		"wtmpdb": {stderr: "wtmpdb: not found\n", exit: "127"},
		"last":   {stdout: "ivan     pts/0    10.0.0.1    Fri Nov 27 11:00   still logged in\n", exit: "0"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{})
	if err != nil || !strings.Contains(out, "Login records via last (/var/log/wtmp):") {
		t.Fatalf("classic last = %q, %v", out, err)
	}
	if got := fakeArgs(t, dir, "last"); got != "-n\n50\n-i\n-w\n-s\n2026-11-26 12:00:00\n" {
		t.Errorf("last args = %q", got)
	}

	// Empty journal output is a diagnostic result, not an error.
	loginHistoryFakes(t, map[string]fakeProgram{
		"wtmpdb":     {exit: "127"},
		"last":       {stdout: "records\n", exit: "0"},
		"journalctl": {stdout: "", exit: "0"},
	})
	out, err = (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{})
	if err != nil || !strings.Contains(out, "No SSH authentication events in the requested interval.") {
		t.Fatalf("empty journal = %q, %v", out, err)
	}
}

func TestLoginHistoryJournalOnly(t *testing.T) {
	// Ubuntu 25.04+: neither wtmpdb nor last exists.
	loginHistoryFakes(t, map[string]fakeProgram{
		"wtmpdb": {exit: "127"},
		"last":   {stderr: "last: not found\n", exit: "127"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{})
	if err != nil {
		t.Fatalf("journal-only history must succeed: %v", err)
	}
	if strings.Contains(out, "Login records via") {
		t.Fatalf("absent login records reported as a source:\n%s", out)
	}
	if !strings.Contains(out, "Accepted publickey for ivan") || !strings.Contains(out, "Limitations:") || !strings.Contains(out, "login records unavailable") {
		t.Fatalf("journal-only output = %q", out)
	}
}

func TestLoginHistoryAllUnavailable(t *testing.T) {
	loginHistoryFakes(t, map[string]fakeProgram{
		"wtmpdb":     {exit: "127"},
		"last":       {exit: "127"},
		"journalctl": {stderr: "No journal files were found.\n", exit: "1"},
	})
	if _, err := (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{}); err == nil || !strings.Contains(err.Error(), "login history unavailable") {
		t.Fatalf("all sources unavailable must fail, got %v", err)
	}
}

func TestLoginHistoryValidation(t *testing.T) {
	dir := loginHistoryFakes(t, map[string]fakeProgram{
		"wtmpdb": {exit: "127"},
		"last":   {stdout: "records\n", exit: "0"},
	})
	tools := &Tools{Runner: &remote.Runner{}}
	zero, overLimit, overWindow := 0, 501, 43201
	for _, args := range []toolArgs{
		{Limit: &zero}, {Limit: &overLimit},
		{SinceMinutes: &zero}, {SinceMinutes: &overWindow},
		{User: "-rf"}, {User: "a b"}, {User: "ivan;rm"}, {User: strings.Repeat("a", 33)},
	} {
		if _, err := tools.LoginHistory(context.Background(), args); err == nil {
			t.Errorf("invalid arguments accepted: %+v", args)
		}
	}
	for _, name := range []string{"date", "last", "wtmpdb", "journalctl", "stat"} {
		requireNoCommand(t, dir, name)
	}
}

func TestLoginHistoryUserAndWindow(t *testing.T) {
	limit, minutes := 10, 60
	dir := loginHistoryFakes(t, map[string]fakeProgram{
		"wtmpdb": {exit: "127"},
		"last":   {stdout: "", exit: "0"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{User: "ivan", Limit: &limit, SinceMinutes: &minutes})
	if err != nil {
		t.Fatalf("LoginHistory: %v", err)
	}
	if !strings.Contains(out, "(last 60 minutes, limit 10)") || !strings.Contains(out, "No login records in the requested interval.") {
		t.Fatalf("window output = %q", out)
	}
	if got := fakeArgs(t, dir, "last"); got != "-n\n10\n-i\n-w\n-s\n2026-11-26 12:00:00\nivan\n" {
		t.Errorf("last args = %q", got)
	}
	if got := fakeArgs(t, dir, "date"); got != "-d\n-60 minutes\n+%Y-%m-%d %H:%M:%S\n" {
		t.Errorf("date args = %q", got)
	}
	if got := fakeArgs(t, dir, "journalctl"); !strings.Contains(got, "--lines=10\n--since=-60min\n") {
		t.Errorf("journalctl args = %q", got)
	}
}

func TestLoginHistoryDateUnavailable(t *testing.T) {
	dir := loginHistoryFakes(t, map[string]fakeProgram{
		"date":   {exit: "127"},
		"wtmpdb": {exit: "127"},
		"last":   {stdout: "records\n", exit: "0"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).LoginHistory(context.Background(), toolArgs{})
	if err != nil || !strings.Contains(out, "host time filter unavailable") {
		t.Fatalf("missing date must be a limitation, got %q, %v", out, err)
	}
	if got := fakeArgs(t, dir, "last"); got != "-n\n50\n-i\n-w\n" {
		t.Errorf("last args without time filter = %q", got)
	}
}

func TestLoginStorageFooter(t *testing.T) {
	tools := &Tools{Runner: &remote.Runner{}}

	// All paths absent: stat exit 1 is expected data, not an execution failure.
	fakePrograms(t, map[string]fakeProgram{
		"stat": {stderr: "stat: cannot statx '/var/log/journal': No such file or directory\n", exit: "1"},
	})
	footer := tools.loginStorageFooter(context.Background(), "")
	if !strings.Contains(footer, "/var/log/journal: absent (volatile journal") ||
		!strings.Contains(footer, "/var/log/wtmp.db: absent") ||
		strings.Contains(footer, "storage inspection unavailable") {
		t.Fatalf("all-absent footer = %q", footer)
	}

	// stat itself missing.
	fakePrograms(t, map[string]fakeProgram{"stat": {exit: "127"}})
	footer = tools.loginStorageFooter(context.Background(), "")
	if !strings.Contains(footer, "storage inspection unavailable") {
		t.Fatalf("missing stat footer = %q", footer)
	}

	// All paths present.
	fakePrograms(t, map[string]fakeProgram{
		"stat": {stdout: "/var/log/journal mode=2755 owner=root:systemd-journal\n" +
			"/var/log/wtmp.db mode=644 owner=root:root\n" +
			"/var/log/wtmp mode=644 owner=root:utmp\n" +
			"/var/log/audit/audit.log mode=640 owner=root:adm\n" +
			"/var/log/auth.log mode=640 owner=root:adm\n" +
			"/var/log/secure mode=600 owner=root:root\n", exit: "0"},
	})
	footer = tools.loginStorageFooter(context.Background(), "")
	for _, want := range []string{
		"/var/log/journal: mode=2755 owner=root:systemd-journal (persistent journal)",
		"/var/log/wtmp: mode=644 owner=root:utmp",
		"/var/log/audit/audit.log: mode=640 owner=root:adm",
		"/var/log/secure: mode=600 owner=root:root",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("present footer missing %q:\n%s", want, footer)
		}
	}

	// EACCES is not absence: Debian's /var/log/audit is 0750 root:adm, so an
	// unprivileged stat fails with Permission denied. A wtmp.db failure must
	// not mark the /var/log/wtmp prefix as unreadable.
	fakePrograms(t, map[string]fakeProgram{
		"stat": {
			stdout: "/var/log/journal mode=2755 owner=root:systemd-journal\n",
			stderr: "stat: cannot statx '/var/log/wtmp.db': Permission denied\n" +
				"stat: cannot statx '/var/log/audit/audit.log': Permission denied\n" +
				"stat: cannot statx '/var/log/wtmp': No such file or directory\n",
			exit: "1",
		},
	})
	footer = tools.loginStorageFooter(context.Background(), "")
	for _, want := range []string{
		"/var/log/wtmp.db: status unknown (Permission denied; a check limitation, not evidence of absence)",
		"/var/log/audit/audit.log: status unknown (Permission denied; a check limitation, not evidence of absence)",
		"/var/log/wtmp: absent",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("EACCES footer missing %q:\n%s", want, footer)
		}
	}
	if strings.Contains(footer, "/var/log/wtmp: status unknown") {
		t.Fatalf("wtmp.db failure leaked into the wtmp prefix:\n%s", footer)
	}
}
