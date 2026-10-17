package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

const processHeader = "    PID USER     %CPU %MEM   RSS COMMAND\n"

func TestTopProcessesSortAndLimit(t *testing.T) {
	for _, tc := range []struct {
		name, raw, sort string
		wantRows        int
	}{
		{"defaults", `{}`, "-rss", 10},
		{"memory", `{"sort_by":"memory","limit":2}`, "-rss", 2},
		{"cpu", `{"sort_by":"cpu","limit":1}`, "-pcpu", 1},
		{"maximum", `{"limit":50}`, "-rss", 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := processHeader + strings.Repeat("     42 sre      150.0  2.5  102400 worker name\n", 12)
			argsFile := fakeCommand(t, "ps", output, "", "0")
			call := Registry(&remote.Runner{})["get_top_processes"]
			if call == nil {
				t.Fatal("process tool is not registered")
			}
			out, err := call(context.Background(), json.RawMessage(tc.raw))
			if err != nil || strings.Count(out, "worker name") != tc.wantRows ||
				!strings.Contains(out, "lifetime average") || !strings.Contains(out, "RSS: KiB") ||
				!strings.Contains(out, "150.0") {
				t.Fatalf("wrong process report: %q, %v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			want := "-eo\npid,user,pcpu,pmem,rss,comm\n--sort=" + tc.sort + "\n--cols=256\n"
			if string(args) != want {
				t.Fatalf("unsafe or incorrect ps arguments: %q, want %q", args, want)
			}
		})
	}
}

func TestTopProcessesRejectsArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "ps", processHeader, "", "0")
	call := Registry(&remote.Runner{})["get_top_processes"]
	if call == nil {
		t.Fatal("process tool is not registered")
	}
	for _, raw := range []string{
		`{"sort_by":"rss"}`, `{"sort_by":"cpu; touch injected"}`,
		`{"limit":0}`, `{"limit":-1}`, `{"limit":51}`, `{"limit":1.5}`,
		`{"limit":"10"}`, `{"command":"kill"}`, `[]`, `null`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("ps ran before argument validation")
	}
}

const topIntervalFixture = `top - 12:00:00 up 1 day,  1 user,  load average: 0.10, 0.20, 0.30
Tasks: 100 total,   1 running,  99 sleeping,   0 stopped,   0 zombie
%Cpu(s):  1.0 us,  0.5 sy,  0.0 ni, 98.0 id,  0.5 wa,  0.0 hi,  0.0 si,  0.0 st
MiB Mem :  16000.0 total,   1000.0 free,   5000.0 used,  10000.0 buff/cache
MiB Swap:   1024.0 total,   1024.0 free,      0.0 used.  11000.0 avail Mem

    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
     10 root      20   0  100000  10000   5000 R  99.0   0.1   0:01.00 first-process
     11 root      20   0  100000  10000   5000 S  50.0   0.1   0:00.50 second-process

top - 12:00:01 up 1 day,  1 user,  load average: 0.10, 0.20, 0.30
Tasks: 100 total,   1 running,  99 sleeping,   0 stopped,   0 zombie
%Cpu(s):  1.0 us,  0.5 sy,  0.0 ni, 98.0 id,  0.5 wa,  0.0 hi,  0.0 si,  0.0 st
MiB Mem :  16000.0 total,   1000.0 free,   5000.0 used,  10000.0 buff/cache
MiB Swap:   1024.0 total,   1024.0 free,      0.0 used.  11000.0 avail Mem

    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND
     20 sre       20   0  100000  10000   5000 R  42.0   0.1   0:01.00 worker-name
     21 sre       20   0  100000  10000   5000 S  10.0   0.1   0:00.10 other-name
`

func TestTopProcessesCPUInterval(t *testing.T) {
	argsFile := fakeCommand(t, "top", topIntervalFixture, "", "0")
	out, err := Registry(&remote.Runner{})["get_top_processes"](context.Background(), json.RawMessage(`{"sort_by":"cpu_interval","limit":2}`))
	if err != nil || !strings.Contains(out, "1-second interval") || !strings.Contains(out, "worker-name") ||
		!strings.Contains(out, "other-name") || strings.Contains(out, "first-process") || strings.Contains(out, "second-process") {
		t.Fatalf("interval process report = %q, %v", out, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "-b\n-n\n2\n-d\n1\n-o\n+%CPU\n-w\n256\n-i\n"
	if string(args) != want {
		t.Fatalf("top arguments = %q, want %q", args, want)
	}
}

func TestTopProcessesCPUIntervalErrors(t *testing.T) {
	call := Registry(&remote.Runner{})["get_top_processes"]
	fakeCommand(t, "top", strings.Split(topIntervalFixture, "\ntop - 12:00:01")[0]+"\n", "", "0")
	if _, err := call(context.Background(), json.RawMessage(`{"sort_by":"cpu_interval"}`)); err == nil {
		t.Fatal("one top sample was accepted")
	}
	fakeCommand(t, "top", strings.Repeat("x", 70000), "", "0")
	if _, err := call(context.Background(), json.RawMessage(`{"sort_by":"cpu_interval"}`)); err == nil {
		t.Fatal("truncated top output was accepted")
	}
	noRows := "    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND\n\n" +
		"    PID USER      PR  NI    VIRT    RES    SHR S  %CPU  %MEM     TIME+ COMMAND\n"
	fakeCommand(t, "top", noRows, "", "0")
	if _, err := call(context.Background(), json.RawMessage(`{"sort_by":"cpu_interval"}`)); err == nil {
		t.Fatal("top sample without rows was accepted")
	}
	fakeCommand(t, "top", "", "permission denied\n", "1")
	if _, err := call(context.Background(), json.RawMessage(`{"sort_by":"cpu_interval"}`)); err == nil {
		t.Fatal("top command failure was hidden")
	}
}

func TestPipelineRejectsCPUInterval(t *testing.T) {
	argsFile := fakeCommand(t, "top", "", "", "0")
	raw := json.RawMessage(`{"source":{"tool":"get_top_processes","args":{"sort_by":"cpu_interval"}}}`)
	if _, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw); err == nil {
		t.Fatal("cpu_interval was accepted as a pipeline source")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("top ran before the pipeline source was rejected")
	}
}

func TestTopProcessesErrorsAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, exit string
		wantErr                    bool
	}{
		{"command error", "", "ps permission denied: secret", "1", true},
		{"empty output", "", "", "0", true},
		{"no rows", processHeader, "", "0", true},
		{"truncated after top rows", processHeader + strings.Repeat("     42 sre      1.0  0.1  1024 secret-worker\n", 2000), "", "0", false},
		{"truncated first row", processHeader + strings.Repeat("x", 70000), "", "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "ps", tc.stdout, tc.stderr, tc.exit)
			runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
			call := Registry(runner)["get_top_processes"]
			if call == nil {
				t.Fatal("process tool is not registered")
			}
			out, err := call(context.Background(), nil)
			if (err != nil) != tc.wantErr || strings.Contains(out, "secret") || (err != nil && strings.Contains(err.Error(), "secret")) {
				t.Fatalf("report or error leaked password: %q, %v", out, err)
			}
			if !tc.wantErr && strings.Count(out, "[redacted]-worker") != 10 {
				t.Fatal("truncated ps output lost the requested complete top rows")
			}
		})
	}
}

func TestTopProcessesLocally(t *testing.T) {
	call := Registry(&remote.Runner{})["get_top_processes"]
	if call == nil {
		t.Fatal("process tool is not registered")
	}
	sortModes := []string{"memory", "cpu"}
	if _, err := exec.LookPath("top"); err == nil {
		sortModes = append(sortModes, "cpu_interval")
	}
	for _, sortBy := range sortModes {
		out, err := call(context.Background(), json.RawMessage(`{"sort_by":"`+sortBy+`","limit":3}`))
		if err != nil || !strings.Contains(out, "COMMAND") || !strings.Contains(out, "PID") {
			t.Fatalf("real local process report (%s): %q, %v", sortBy, out, err)
		}
		if sortBy == "cpu_interval" && !strings.Contains(out, "1-second interval") {
			t.Fatalf("real interval report: %q", out)
		}
	}
	if _, err := call(context.Background(), json.RawMessage(`{"host_id":"missing"}`)); err == nil {
		t.Fatal("unknown SSH host accepted")
	}
}

func TestSearchProcessesLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			var rows strings.Builder
			for pid := 1; pid <= 60; pid++ {
				fmt.Fprintf(&rows, "%d worker\n", pid)
			}
			argsFile := fakeCommand(t, "pgrep", rows.String(), "", "0")
			raw, err := json.Marshal(map[string]any{"host_id": host, "pattern": "-worker'.[x]$"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := Registry(runner)["search_processes"](context.Background(), raw)
			if err != nil || !strings.Contains(out, "60 worker") || strings.Count(out, " worker\n") != 60 {
				t.Fatalf("search was limited by resource ranking: %q, %v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil || string(args) != "-i\n-l\n--\n-worker'\\.\\[x\\]\\$\n" {
				t.Fatalf("literal pattern was not safely passed to pgrep: %q, %v", args, err)
			}
		})
	}
}

func TestSearchProcessesResults(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, exit, raw, want string
		wantErr                               bool
	}{
		{"limit", "1 worker\n2 worker\n", "", "0", `{"pattern":"worker","limit":1}`, "additional matches may be omitted", false},
		{"default limit", strings.Repeat("1 worker\n", 101), "", "0", `{"pattern":"worker"}`, "additional matches may be omitted", false},
		{"no matches", "", "", "1", `{"pattern":"worker"}`, "No visible process names matched", false},
		{"no matches with warning", "", "name may be truncated\n", "1", `{"pattern":"worker"}`, "name may be truncated", false},
		{"command error", "", "permission denied ssh-test-password", "2", `{"pattern":"worker"}`, "permission denied [redacted]", true},
		{"empty successful output", "", "", "0", `{"pattern":"worker"}`, "no complete process rows", true},
		{"truncated matches", strings.Repeat("1 worker\n", 8000), "", "0", `{"pattern":"worker","limit":500}`, "additional matches may be omitted", false},
		{"truncated first row", strings.Repeat("x", 70000), "", "0", `{"pattern":"worker"}`, "no complete process rows", true},
		{"redacted result", "1 ssh-test-password\n", "", "0", `{"pattern":"worker"}`, "1 [redacted]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "pgrep", tc.stdout, tc.stderr, tc.exit)
			runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "ssh-test-password"}}}}
			out, err := Registry(runner)["search_processes"](context.Background(), json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("result=%q err=%v", out, err)
			}
			if err != nil {
				out = err.Error()
			}
			if !strings.Contains(out, tc.want) || strings.Contains(out, "ssh-test-password") {
				t.Fatalf("unexpected result: %q", out)
			}
		})
	}
}

func TestSearchProcessesRejectsArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "pgrep", "", "", "0")
	call := Registry(&remote.Runner{})["search_processes"]
	for _, raw := range []string{
		`{}`, `{"pattern":""}`, `{"pattern":"x\u0000"}`, `{"pattern":"x\n"}`, `{"pattern":"x\r"}`,
		`{"pattern":"` + strings.Repeat("x", 129) + `"}`, `{"pattern":"` + strings.Repeat("я", 65) + `"}`,
		`{"pattern":"x","limit":0}`, `{"pattern":"x","limit":501}`, `{"pattern":"x","limit":1.5}`,
		`{"pattern":"x","command":"kill"}`, `null`, `[]`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("pgrep ran before validation")
	}
}

func TestSearchProcessesRealLiteralMatch(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is not available")
	}
	data, err := os.ReadFile(sleepPath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sre.test[x]")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	out, err := Registry(&remote.Runner{})["search_processes"](context.Background(), json.RawMessage(`{"pattern":"SRE.test["}`))
	if err != nil || !strings.Contains(out, fmt.Sprintf("%d sre.test[x]", cmd.Process.Pid)) {
		t.Fatalf("real literal, case-insensitive lookup failed: %q, %v", out, err)
	}
}
