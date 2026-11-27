package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

type fakeProgram struct {
	stdout, stderr string
	exit           string
	// body replaces the canned output statements; argument recording stays.
	body string
}

// fakePrograms installs one fake executable per name, each with its own
// canned output, and records each program's last invocation arguments in
// <name>.args inside the returned directory.
func fakePrograms(t *testing.T, programs map[string]fakeProgram) string {
	t.Helper()
	dir := t.TempDir()
	for name, p := range programs {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + filepath.Join(dir, name+".args") + "\"\n"
		if p.body != "" {
			script += p.body
		} else {
			script += "printf '%s' " + fakeQuote(p.stdout) + "\n" +
				"printf '%s' " + fakeQuote(p.stderr) + " >&2\n" +
				"exit " + p.exit + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func fakeQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func fakeArgs(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name+".args"))
	if err != nil {
		t.Fatalf("%s args: %v", name, err)
	}
	return string(raw)
}

func requireNoCommand(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name+".args")); !os.IsNotExist(err) {
		t.Fatalf("%s ran before validation", name)
	}
}

func TestUserSessionsOverview(t *testing.T) {
	wOut := " 12:00:00 up 1 day,  2 users,  load average: 0.10, 0.20, 0.30\n" +
		"USER     TTY      FROM             LOGIN@   IDLE   WHAT\n" +
		"ivan     pts/0    10.0.0.1         11:00    0.00s  w\n"
	dir := fakePrograms(t, map[string]fakeProgram{
		"w":        {stdout: wOut, exit: "0"},
		"loginctl": {stdout: "41 1000 ivan seat0 pts/0\n", exit: "0"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).UserSessions(context.Background(), toolArgs{})
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	for _, want := range []string{
		"Active sessions on local:",
		"w (user, TTY, from, login time, idle, current command):",
		"ivan     pts/0    10.0.0.1",
		"logind sessions (pass session_id for details):",
		"41 1000 ivan seat0 pts/0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sessions output missing %q:\n%s", want, out)
		}
	}
	if got := fakeArgs(t, dir, "w"); got != "-i\n" {
		t.Errorf("w args = %q", got)
	}
	if got := fakeArgs(t, dir, "loginctl"); got != "list-sessions\n--no-legend\n--no-pager\n" {
		t.Errorf("loginctl args = %q", got)
	}
}

func TestUserSessionDetail(t *testing.T) {
	detail := "Session 41:\n  Service: sshd\n  RemoteHost: 10.0.0.1\n  Leader: 1234 (sshd)\n"
	dir := fakePrograms(t, map[string]fakeProgram{
		"loginctl": {stdout: detail, exit: "0"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).UserSessions(context.Background(), toolArgs{SessionID: "41"})
	if err != nil || out != detail {
		t.Fatalf("session detail = %q, %v", out, err)
	}
	if got := fakeArgs(t, dir, "loginctl"); got != "session-status\n41\n--no-pager\n" {
		t.Errorf("loginctl args = %q", got)
	}
	requireNoCommand(t, dir, "w")

	for _, id := range []string{"abc", "41x", "-1", "c", "41;rm", strings.Repeat("9", 17)} {
		if _, err := (&Tools{Runner: &remote.Runner{}}).UserSessions(context.Background(), toolArgs{SessionID: id}); err == nil {
			t.Errorf("invalid session_id accepted: %q", id)
		}
	}
}

func TestUserSessionsLimitations(t *testing.T) {
	fakePrograms(t, map[string]fakeProgram{
		"w":        {stderr: "w: not found\n", exit: "127"},
		"loginctl": {stdout: "41 1000 ivan seat0 pts/0\n", exit: "0"},
	})
	out, err := (&Tools{Runner: &remote.Runner{}}).UserSessions(context.Background(), toolArgs{})
	if err != nil || !strings.Contains(out, "Limitations: w unavailable") || !strings.Contains(out, "41 1000 ivan") {
		t.Fatalf("missing w must be a limitation, got %q, %v", out, err)
	}

	fakePrograms(t, map[string]fakeProgram{
		"w":        {stderr: "w: not found\n", exit: "127"},
		"loginctl": {stderr: "loginctl: not found\n", exit: "127"},
	})
	if _, err := (&Tools{Runner: &remote.Runner{}}).UserSessions(context.Background(), toolArgs{}); err == nil || !strings.Contains(err.Error(), "active sessions unavailable") {
		t.Fatalf("both sources missing must fail, got %v", err)
	}

	fakePrograms(t, map[string]fakeProgram{
		"w":        {stdout: "header\n", exit: "0"},
		"loginctl": {stdout: "", exit: "0"},
	})
	out, err = (&Tools{Runner: &remote.Runner{}}).UserSessions(context.Background(), toolArgs{})
	if err != nil || !strings.Contains(out, "No logind sessions reported.") {
		t.Fatalf("empty logind list = %q, %v", out, err)
	}
}

func TestUserSessionsPipelineStage(t *testing.T) {
	var def Definition
	for _, d := range definitions() {
		if d.Name == "list_user_sessions" {
			def = d
		}
	}
	if def.Pipeline == nil {
		t.Fatal("list_user_sessions must be a pipeline source")
	}
	if _, err := def.Pipeline.Stage(context.Background(), &Tools{}, toolArgs{SessionID: "41"}); err == nil {
		t.Fatal("session_id must not be a pipeline source")
	}
	stage, err := def.Pipeline.Stage(context.Background(), &Tools{}, toolArgs{})
	if err != nil || len(stage.Commands) != 2 || stage.Commands[0].Program != "w" || stage.Commands[1].Program != "loginctl" {
		t.Fatalf("pipeline stage = %+v, %v", stage, err)
	}
}

func TestUserSessionsRegistryValidation(t *testing.T) {
	dir := fakePrograms(t, map[string]fakeProgram{
		"w":        {stdout: "header\n", exit: "0"},
		"loginctl": {stdout: "", exit: "0"},
	})
	call := Registry(&remote.Runner{})["list_user_sessions"]
	for _, raw := range []string{
		`{"session_id":"abc"}`, `{"session_id":41}`, `{"user":"ivan"}`,
		`{"since_minutes":60}`, `null`, `[]`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("invalid arguments accepted: %s", raw)
		}
	}
	requireNoCommand(t, dir, "w")
	requireNoCommand(t, dir, "loginctl")
}
