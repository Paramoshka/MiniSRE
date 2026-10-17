package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

func fakeCommand(t *testing.T, name, stdout, stderr string, exit string) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$MINISRE_TEST_ARGS\"\nprintf '%s' \"$MINISRE_TEST_STDOUT\"\nprintf '%s' \"$MINISRE_TEST_STDERR\" >&2\nexit \"$MINISRE_TEST_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MINISRE_TEST_ARGS", argsFile)
	t.Setenv("MINISRE_TEST_STDOUT", stdout)
	t.Setenv("MINISRE_TEST_STDERR", stderr)
	t.Setenv("MINISRE_TEST_EXIT", exit)
	return argsFile
}

func TestServiceStatus(t *testing.T) {
	tests := []struct {
		backend, command, stdout string
		wantErr                  bool
	}{
		{"systemd", "systemctl", "LoadState=loaded\nActiveState=active\nSubState=running\n", false},
		{"systemd", "systemctl", "LoadState=loaded\nActiveState=failed\nResult=exit-code\nExecMainStatus=1\n", false},
		{"systemd", "systemctl", "LoadState=loaded\nActiveState=inactive\nSubState=dead\n", false},
		{"systemd", "systemctl", "LoadState=not-found\nActiveState=inactive\n", true},
		{"docker", "docker", `{"Status":"exited","ExitCode":2}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.backend+tt.stdout, func(t *testing.T) {
			argsFile := fakeCommand(t, tt.command, tt.stdout, "", "0")
			out, err := (&Tools{Runner: &remote.Runner{}}).ServiceStatus(context.Background(), "local", ServiceTarget{Name: "nginx", Backend: tt.backend})
			if (err != nil) != tt.wantErr {
				t.Fatalf("status=%q err=%v", out, err)
			}
			if !tt.wantErr && out != tt.stdout {
				t.Fatalf("status=%q want=%q", out, tt.stdout)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if tt.backend == "docker" {
				if !strings.Contains(string(args), "--format={{json .State}}\n") || !strings.HasSuffix(string(args), "--\nnginx\n") {
					t.Fatalf("unsafe or wrong Docker inspect: %s", args)
				}
			} else {
				if !strings.HasSuffix(string(args), "--\nnginx.service\n") || strings.Contains(string(args), "--user\n") {
					t.Fatalf("wrong systemd unit or default scope: %s", args)
				}
			}
		})
	}
}

func TestFailedServices(t *testing.T) {
	unitOutput := "nginx.service loaded failed failed nginx\nfoo.mount loaded failed failed /foo\n"
	for _, tc := range []struct {
		name, scope, stdout, want string
	}{
		{"system", "", unitOutput, "Failed systemd units on local (scope=system):"},
		{"user", "user", unitOutput, "Failed systemd units on local (scope=user):"},
		{"empty system", "", "", "No failed systemd units on local in the system scope."},
		{"empty user", "user", "", "No failed systemd units on local in the user scope."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := fakeCommand(t, "systemctl", tc.stdout, "", "0")
			out, err := (&Tools{Runner: &remote.Runner{}}).FailedServices(context.Background(), "local", tc.scope)
			if err != nil {
				t.Fatalf("FailedServices: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("FailedServices = %q, want %q", out, tc.want)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := "--failed\n--no-pager\n--plain\n--no-legend\n"
			if tc.scope == "user" {
				wantArgs = "--user\n" + wantArgs
			}
			if string(args) != wantArgs {
				t.Fatalf("systemctl arguments = %q, want %q", args, wantArgs)
			}
		})
	}
}

func TestFailedServicesRejectsInvalidScopeBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "systemctl", "", "", "0")
	if _, err := (&Tools{Runner: &remote.Runner{}}).FailedServices(context.Background(), "local", "docker"); err == nil {
		t.Fatal("invalid scope accepted")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("systemctl ran before scope validation")
	}
}

func TestFailedServicesCommandError(t *testing.T) {
	fakeCommand(t, "systemctl", "", "Failed to connect to bus\n", "1")
	if _, err := (&Tools{Runner: &remote.Runner{}}).FailedServices(context.Background(), "local", "user"); err == nil {
		t.Fatal("systemctl failure was hidden")
	}
}

func TestServiceLogs(t *testing.T) {
	for _, backend := range []string{"systemd", "docker"} {
		t.Run(backend, func(t *testing.T) {
			program := "journalctl"
			if backend == "docker" {
				program = "docker"
			}
			argsFile := fakeCommand(t, program, "stdout log\n", "stderr log\n", "0")
			call := Registry(&remote.Runner{})["get_service_logs"]
			out, err := call(context.Background(), json.RawMessage(`{"service":"nginx","backend":"`+backend+`"}`))
			if err != nil || out != "stdout log\nstderr log\n" {
				t.Fatalf("logs: %q %v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if backend == "systemd" && (!strings.Contains(string(args), "--output=short-iso\n") || !strings.Contains(string(args), "--lines=100\n") || !strings.Contains(string(args), "--since=-60min\n")) {
				t.Fatalf("wrong journal defaults: %s", args)
			}
			if backend == "docker" && (!strings.Contains(string(args), "--tail=100\n") || !strings.Contains(string(args), "--since=60m\n")) {
				t.Fatalf("wrong Docker defaults: %s", args)
			}
			if strings.Contains(string(args), "--follow") {
				t.Fatal("logs must not follow")
			}
		})
	}
}

func TestJournalFilters(t *testing.T) {
	bootID := strings.Repeat("a", 32)
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"priority and previous boot", `{"service":"nginx","priority":"err","boot":"previous"}`, []string{"--priority=err", "--boot=-1", "--unit=nginx.service"}},
		{"current boot", `{"boot":"current"}`, []string{"--boot=0"}},
		{"explicit boot id", `{"boot":"` + bootID + `"}`, []string{"--boot=" + bootID}},
		{"user priority", `{"scope":"user","priority":"crit"}`, []string{"--priority=crit", "--user"}},
		{"message output", `{"service":"nginx","output":"cat"}`, []string{"--output=cat", "--unit=nginx.service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := fakeCommand(t, "journalctl", "log entry\n", "", "0")
			out, err := Registry(&remote.Runner{})["get_service_logs"](context.Background(), json.RawMessage(tc.raw))
			if err != nil || out != "log entry\n" {
				t.Fatalf("logs: %q %v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(args), want+"\n") {
					t.Fatalf("journal arguments missing %q: %s", want, args)
				}
			}
		})
	}
}

func TestJournalFilterValidation(t *testing.T) {
	for _, raw := range []string{
		`{"priority":"ERROR"}`, `{"priority":"err..warning"}`, `{"priority":"0"}`, `{"priority":"err; id"}`,
		`{"boot":"last"}`, `{"boot":"0"}`, `{"boot":"-1"}`, `{"boot":"` + strings.Repeat("A", 32) + `"}`,
		`{"boot":"` + strings.Repeat("a", 31) + `"}`, `{"boot":"` + strings.Repeat("a", 33) + `"}`,
		`{"output":"json"}`, `{"output":"short"}`,
		`{"backend":"docker","service":"web","priority":"err"}`,
		`{"backend":"docker","service":"web","boot":"current"}`,
		`{"backend":"docker","service":"web","output":"cat"}`,
	} {
		args, err := decodeArgs(json.RawMessage(raw))
		if err != nil {
			continue
		}
		if _, err := serviceLogsCommand(args); err == nil {
			t.Fatalf("invalid journal filter accepted: %s", raw)
		}
	}
}

func TestUserSystemdStatusAndLogs(t *testing.T) {
	for _, tool := range []string{"get_service_status", "get_service_logs"} {
		t.Run(tool, func(t *testing.T) {
			program, output := "systemctl", "LoadState=loaded\nActiveState=active\n"
			if tool == "get_service_logs" {
				program, output = "journalctl", "bot started\n"
			}
			argsFile := fakeCommand(t, program, output, "", "0")
			out, err := Registry(&remote.Runner{})[tool](context.Background(), json.RawMessage(`{"service":"mini-sre","scope":"user"}`))
			if err != nil || out != output {
				t.Fatalf("user service: out=%q err=%v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "--user\n") {
				t.Fatalf("command does not select the user manager/journal: %s", args)
			}
			if tool == "get_service_logs" && !strings.Contains(string(args), "--user-unit=mini-sre.service\n") {
				t.Fatalf("journal does not select the user unit: %s", args)
			}
		})
	}
}

func TestLogErrorsAndValidation(t *testing.T) {
	argsFile := fakeCommand(t, "journalctl", "", "permission denied: secret", "1")
	r := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
	call := Registry(r)["get_service_logs"]
	for _, raw := range []string{
		`{"lines":0}`, `{"lines":501}`, `{"since_minutes":-1}`, `{"backend":"docker"}`,
		`{"backend":"openrc"}`, `{"service":"-all"}`, `{"service":"nginx*"}`, `{"service":"a; touch file"}`,
		`{"lines":"secret"}`, `{"command":"rm"}`, `[]`, `null`, `{} {}`, `{bad`,
		`{"scope":"all"}`, `{"scope":"--user"}`, `{"backend":"docker","service":"web","scope":"user"}`,
		`{"backend":"docker","service":"web","scope":"system"}`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("a command ran before validation")
	}
	_, err := call(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("permission error or redaction: %v", err)
	}
}

func TestSystemdJournalScope(t *testing.T) {
	for _, scope := range []string{"system", "user"} {
		t.Run(scope, func(t *testing.T) {
			argsFile := fakeCommand(t, "journalctl", "log entry\n", "", "0")
			out, err := Registry(&remote.Runner{})["get_service_logs"](context.Background(), json.RawMessage(`{"scope":"`+scope+`"}`))
			if err != nil || out != "log entry\n" {
				t.Fatalf("general journal: out=%q err=%v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(args), "--user\n") != (scope == "user") || strings.Contains(string(args), "--unit=") || strings.Contains(string(args), "--user-unit=") {
				t.Fatalf("wrong journal scope or unexpected unit filter: %s", args)
			}
		})
	}
}

func TestEmptyJournalAndDiskArguments(t *testing.T) {
	t.Run("truncated logs", func(t *testing.T) {
		fakeCommand(t, "journalctl", strings.Repeat("x", 70000), "", "0")
		out, err := Registry(&remote.Runner{})["get_service_logs"](context.Background(), nil)
		if err != nil || !strings.Contains(out, "[output truncated at 64 KiB]") {
			t.Fatalf("truncation marker missing: len=%d err=%v", len(out), err)
		}
	})
	t.Run("empty journal", func(t *testing.T) {
		argsFile := fakeCommand(t, "journalctl", "", "", "0")
		out, err := Registry(&remote.Runner{})["get_service_logs"](context.Background(), nil)
		if err != nil || !strings.Contains(out, "No log entries") {
			t.Fatalf("empty logs: %q %v", out, err)
		}
		args, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(args), "--unit") {
			t.Fatal("general journal was restricted to a service")
		}
	})
	t.Run("literal path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "space ' $(touch injected)")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		out, err := (&Tools{Runner: &remote.Runner{}}).DiskUsage(context.Background(), "local", path)
		if err != nil || !strings.HasPrefix(out, path+": total=") {
			t.Fatalf("disk path: %q %v", out, err)
		}
	})
	t.Run("malformed disk result", func(t *testing.T) {
		fakeCommand(t, "df", "Size Used Avail\n1024 invalid 512\n", "", "0")
		if _, err := (&Tools{Runner: &remote.Runner{}}).DiskUsage(context.Background(), "local", "/"); err == nil {
			t.Fatal("malformed disk result accepted")
		}
	})
}
