package tools

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

func fileFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadFile(t *testing.T) {
	path := fileFixture(t, "config", "first\nsecond\nthird\nfourth")
	for _, tc := range []struct{ name, raw, want string }{
		{"defaults", `{}`, "first\nsecond\nthird\nfourth"},
		{"range", `{"start_line":2,"lines":2}`, "second\nthird\n"},
		{"tail", `{"tail":true,"lines":2}`, "third\nfourth"},
		{"past EOF", `{"start_line":10}`, "No lines in the requested range."},
		{"last line", `{"start_line":4,"lines":500}`, "fourth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := decodeArgs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			args.Path = path
			out, err := (&Tools{Runner: &remote.Runner{}}).ReadFile(context.Background(), args)
			if err != nil || out != tc.want {
				t.Fatalf("ReadFile = %q, %v; want %q", out, err, tc.want)
			}
		})
	}
	path = fileFixture(t, "empty", "")
	out, err := (&Tools{Runner: &remote.Runner{}}).ReadFile(context.Background(), toolArgs{Path: path})
	if err != nil || out != "No lines in the requested range." {
		t.Fatalf("empty file = %q, %v", out, err)
	}
}

func TestSearchFile(t *testing.T) {
	path := fileFixture(t, "config", "listen 80\nLISTEN 443\ntimeout=10\nlisten.*\n-e\n")
	for _, tc := range []struct{ name, raw, want string }{
		{"literal", `{"pattern":"listen.*"}`, "4:listen.*\n"},
		{"regex", `{"pattern":"^listen [0-9]+$","regex":true}`, "1:listen 80\n"},
		{"ignore case", `{"pattern":"listen","ignore_case":true,"limit":2}`, "1:listen 80\n2:LISTEN 443\n"},
		{"limit", `{"pattern":"listen","limit":1}`, "1:listen 80\n"},
		{"leading dash", `{"pattern":"-e"}`, "5:-e\n"},
		{"no matches", `{"pattern":"missing"}`, "No matching lines."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := decodeArgs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			args.Path = path
			out, err := (&Tools{Runner: &remote.Runner{}}).SearchFile(context.Background(), args)
			if err != nil || out != tc.want {
				t.Fatalf("SearchFile = %q, %v; want %q", out, err, tc.want)
			}
		})
	}
	if _, err := (&Tools{Runner: &remote.Runner{}}).SearchFile(context.Background(), toolArgs{Path: path, Pattern: "[", Regex: true}); err == nil {
		t.Fatal("invalid regex accepted")
	}
}

func TestFileToolsRejectArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "readlink", "", "", "0")
	tools := &Tools{Runner: &remote.Runner{}}
	for _, raw := range []string{
		`{"lines":0}`, `{"lines":501}`, `{"start_line":0}`,
		`{"tail":true,"start_line":1}`, `{"lines":2,"start_line":` + strconv.Itoa(math.MaxInt) + `}`,
		`{"lines":1.5}`, `{"tail":"true"}`, `{"command":"cat"}`,
	} {
		args, err := decodeArgs(json.RawMessage(raw))
		if err == nil {
			args.Path = "/etc/config"
			_, err = tools.ReadFile(context.Background(), args)
		}
		if err == nil {
			t.Fatalf("invalid read arguments accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		`{}`, `{"pattern":"\u0000"}`, `{"pattern":"x","limit":0}`,
		`{"pattern":"x","limit":501}`, `{"pattern":"x","regex":"true"}`,
	} {
		args, err := decodeArgs(json.RawMessage(raw))
		if err == nil {
			args.Path = "/etc/config"
			_, err = tools.SearchFile(context.Background(), args)
		}
		if err == nil {
			t.Fatalf("invalid search arguments accepted: %s", raw)
		}
	}
	for _, path := range []string{"", "relative", "-", "/tmp/a\x00b", "/home/user/.ssh/config", "/opt/.env", "/tmp/.hidden/file", "/tmp/../etc/config", "/tmp/./config"} {
		if _, err := tools.ReadFile(context.Background(), toolArgs{Path: path}); err == nil {
			t.Fatalf("unsafe read path accepted: %q", path)
		}
		if _, err := tools.SearchFile(context.Background(), toolArgs{Path: path, Pattern: "x"}); err == nil {
			t.Fatalf("unsafe search path accepted: %q", path)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("readlink ran before argument validation")
	}
}

func TestFileToolsCheckResolvedPathAndType(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(dir, ".ssh", "config")
	if err := os.WriteFile(hidden, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config")
	if err := os.Symlink(hidden, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "keys")
	if err := os.Symlink(filepath.Dir(hidden), dirLink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	tools := &Tools{Runner: &remote.Runner{}}
	for _, path := range []string{link, filepath.Join(dirLink, "config"), dir, fifo, "/dev/null", filepath.Join(dir, "missing")} {
		if _, err := tools.ReadFile(context.Background(), toolArgs{Path: path}); err == nil {
			t.Fatalf("unsafe path read: %q", path)
		}
		if _, err := tools.SearchFile(context.Background(), toolArgs{Path: path, Pattern: "secret"}); err == nil {
			t.Fatalf("unsafe path searched: %q", path)
		}
	}
	allowed := fileFixture(t, "normal", "visible\n")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(allowed, link); err != nil {
		t.Fatal(err)
	}
	out, err := tools.ReadFile(context.Background(), toolArgs{Path: link})
	if err != nil || out != "visible\n" {
		t.Fatalf("allowed symlink = %q, %v", out, err)
	}
}

func TestFileToolsLiteralArgumentsAndTruncation(t *testing.T) {
	content := "a ' $HOME; $(printf injected)\n"
	path := fileFixture(t, "config ' $HOME; $(printf injected)\nnewline", content)
	tools := &Tools{Runner: &remote.Runner{}}
	out, err := tools.ReadFile(context.Background(), toolArgs{Path: path})
	if err != nil || out != content {
		t.Fatalf("literal filename = %q, %v", out, err)
	}
	out, err = tools.SearchFile(context.Background(), toolArgs{Path: path, Pattern: strings.TrimSuffix(content, "\n")})
	if err != nil || out != "1:"+content {
		t.Fatalf("literal pattern = %q, %v", out, err)
	}
	path = fileFixture(t, "large", strings.Repeat("x", 70000)+"\n")
	for _, tail := range []bool{false, true} {
		out, err = tools.ReadFile(context.Background(), toolArgs{Path: path, Tail: tail})
		if err != nil || !strings.Contains(out, "[output truncated at 64 KiB]") {
			t.Fatalf("truncated read = %d bytes, %v", len(out), err)
		}
	}
	out, err = tools.SearchFile(context.Background(), toolArgs{Path: path, Pattern: "x"})
	if err != nil || !strings.Contains(out, "[output truncated at 64 KiB]") {
		t.Fatalf("truncated search = %d bytes, %v", len(out), err)
	}
}

func TestFileToolsDefaultLimits(t *testing.T) {
	path := fileFixture(t, "config", strings.Repeat("setting=value\n", 120))
	tools := &Tools{Runner: &remote.Runner{}}
	for _, tail := range []bool{false, true} {
		out, err := tools.ReadFile(context.Background(), toolArgs{Path: path, Tail: tail})
		if err != nil || strings.Count(out, "setting=value") != 100 {
			t.Fatalf("default read limit: %d lines, %v", strings.Count(out, "setting=value"), err)
		}
	}
	out, err := tools.SearchFile(context.Background(), toolArgs{Path: path, Pattern: "setting"})
	if err != nil || strings.Count(out, "setting=value") != 100 {
		t.Fatalf("default search limit: %d matches, %v", strings.Count(out, "setting=value"), err)
	}
}

func TestFileToolsCommandErrors(t *testing.T) {
	path := fileFixture(t, "config", "secret setting\n")
	runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
	for _, program := range []string{"readlink", "stat", "sed", "tail", "grep"} {
		t.Run(program, func(t *testing.T) {
			fakeCommand(t, program, "", "permission denied: secret", "1")
			name := "read_file"
			args := map[string]any{"path": path, "tail": program == "tail"}
			if program == "grep" {
				name = "search_file"
				args = map[string]any{"path": path, "pattern": "setting"}
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Registry(runner)[name](context.Background(), raw)
			if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("command failure ignored or leaked password: %v", err)
			}
		})
	}
	if _, err := (&Tools{Runner: runner}).ReadFile(context.Background(), toolArgs{HostID: "missing", Path: path}); err == nil {
		t.Fatal("unknown host accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Tools{Runner: runner}).SearchFile(ctx, toolArgs{Path: path, Pattern: "x"}); err == nil {
		t.Fatal("cancelled search succeeded")
	}
}

func TestFileRegistryRedactsContent(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fake-api-secret")
	path := fileFixture(t, "config", "secret setting fake-api-secret\n")
	runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
	for _, name := range []string{"read_file", "search_file"} {
		args := map[string]any{"path": path}
		if name == "search_file" {
			args["pattern"] = "setting"
		}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		out, err := Registry(runner)[name](context.Background(), raw)
		if err != nil || strings.Contains(out, "secret") || !strings.Contains(out, "[redacted] setting [redacted]") {
			t.Fatalf("%s leaked password: %q, %v", name, out, err)
		}
	}
}

func TestFileToolsRejectMalformedPathChecks(t *testing.T) {
	path := fileFixture(t, "config", "x\n")
	for _, tc := range []struct{ program, stdout string }{
		{"readlink", path}, {"readlink", path + "\x00extra\x00"},
		{"readlink", strings.Repeat("x", 70000)}, {"stat", "invalid"},
	} {
		t.Run(tc.program+strconv.Itoa(len(tc.stdout)), func(t *testing.T) {
			if strings.ContainsRune(tc.stdout, '\x00') {
				argsFile := fakeCommand(t, tc.program, "", "", "0")
				if err := os.WriteFile(filepath.Join(filepath.Dir(argsFile), tc.program), []byte("#!/bin/sh\nprintf '/tmp/config\\000extra\\000'\n"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				fakeCommand(t, tc.program, tc.stdout, "", "0")
			}
			if _, err := (&Tools{Runner: &remote.Runner{}}).ReadFile(context.Background(), toolArgs{Path: path}); err == nil {
				t.Fatal("malformed path check accepted")
			}
		})
	}
}
