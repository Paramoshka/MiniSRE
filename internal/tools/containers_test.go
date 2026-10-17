package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

const containerRows = "abc123\tweb-1\tnginx:latest\trunning\tUp 2 hours (healthy)\t0.0.0.0:80->80/tcp\n" +
	"def456\tdb-1\tpostgres:16\texited\tExited (1) 3 hours ago\t5432/tcp\n"

func TestListContainers(t *testing.T) {
	call := Registry(&remote.Runner{})["list_containers"]
	for _, tc := range []struct {
		name, raw, wantArgs, want string
	}{
		{
			name:     "all containers",
			raw:      `{}`,
			wantArgs: "ps\n--all\n--last\n100\n--format\n" + containerFormat + "\n",
			want:     "Docker containers on local (all containers):",
		},
		{
			name:     "running only",
			raw:      `{"running_only":true}`,
			wantArgs: "ps\n--last\n100\n--filter\nstatus=running\n--format\n" + containerFormat + "\n",
			want:     "Docker containers on local (running containers):",
		},
		{
			name:     "limit",
			raw:      `{"limit":1}`,
			wantArgs: "ps\n--all\n--last\n1\n--format\n" + containerFormat + "\n",
			want:     "additional containers may be omitted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := fakeCommand(t, "docker", containerRows, "", "0")
			out, err := call(context.Background(), json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("list_containers: %v", err)
			}
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "web-1") {
				t.Fatalf("output = %q, want %q", out, tc.want)
			}
			if tc.name == "limit" && strings.Contains(out, "db-1") {
				t.Fatalf("row limit not applied: %q", out)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(args) != tc.wantArgs {
				t.Fatalf("docker arguments = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}

func TestListContainersEmptyAndErrors(t *testing.T) {
	call := Registry(&remote.Runner{})["list_containers"]
	fakeCommand(t, "docker", "", "", "0")
	out, err := call(context.Background(), json.RawMessage(`{}`))
	if err != nil || out != "No containers found." {
		t.Fatalf("empty list = %q, %v", out, err)
	}
	out, err = call(context.Background(), json.RawMessage(`{"running_only":true}`))
	if err != nil || out != "No running containers found." {
		t.Fatalf("empty running list = %q, %v", out, err)
	}

	fakeCommand(t, "docker", "", "permission denied while connecting to the daemon\n", "1")
	if _, err := call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Docker failure was hidden")
	}

	fakeCommand(t, "docker", strings.Repeat("x", 70000), "", "0")
	if _, err := call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("truncated Docker output was accepted")
	}
}

func TestListContainersRejectsArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "docker", "", "", "0")
	call := Registry(&remote.Runner{})["list_containers"]
	for _, raw := range []string{
		`{"limit":0}`, `{"limit":501}`, `{"limit":1.5}`, `{"limit":"1"}`,
		`{"running":true}`, `null`, `[]`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("docker ran before argument validation")
	}
}
