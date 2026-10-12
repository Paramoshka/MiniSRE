package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestSensitiveProcPathsRejectedBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "readlink", "", "", "0")
	tools := &Tools{Runner: &remote.Runner{}}
	for _, path := range []string{
		"/proc/1/environ", "/proc/self/environ", "/proc/thread-self/environ",
		"/proc/123/cmdline", "/proc/123/mem", "/proc/123/fd/3", "/proc/123/fdinfo/3",
		"/proc/123/task/456/environ", "/proc/self/task/456/cmdline", "/proc/thread-self/fd/3",
		"//proc//123//environ", "/proc/123/root/proc/456/environ",
	} {
		for _, host := range []string{"local", "unconfigured-ssh"} {
			assertProcRejected(t, tools, host, path)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("command ran before path validation")
	}
}

func assertProcRejected(t *testing.T, tools *Tools, host, path string) {
	t.Helper()
	args := toolArgs{HostID: host, Path: path, Pattern: "test"}
	if _, err := tools.ReadFile(context.Background(), args); err == nil || !strings.Contains(err.Error(), "sensitive process files") {
		t.Fatalf("read accepted %s: %v", path, err)
	}
	if _, err := tools.SearchFile(context.Background(), args); err == nil || !strings.Contains(err.Error(), "sensitive process files") {
		t.Fatalf("search accepted %s: %v", path, err)
	}
	for _, name := range []string{"read_file", "search_file"} {
		if _, err := tools.sourceStage(context.Background(), name, args); err == nil || !strings.Contains(err.Error(), "sensitive process files") {
			t.Fatalf("pipeline %s accepted %s: %v", name, path, err)
		}
	}
}

func TestProcAliasesAndOrdinaryDiagnostics(t *testing.T) {
	runner := fileSSHRunner(t)
	tools := &Tools{Runner: runner}
	for _, target := range []string{"/proc/1/environ", "/proc/1/cmdline", "/proc/1/mem"} {
		link := filepath.Join(t.TempDir(), "process-file")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"local", "web"} {
			assertProcRejected(t, tools, host, link)
		}
	}
	for _, host := range []string{"local", "web"} {
		for _, path := range []string{"/proc/meminfo", "/proc/loadavg", fileFixture(t, "environ", "ordinary config\n")} {
			if _, err := tools.ReadFile(context.Background(), toolArgs{HostID: host, Path: path}); err != nil {
				t.Fatalf("diagnostic %s rejected on %s: %v", path, host, err)
			}
		}
	}
}
