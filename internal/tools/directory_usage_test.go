package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

func fakeDirectoryUsage(t *testing.T, raw, exit string) string {
	t.Helper()
	dir := t.TempDir()
	data, args := filepath.Join(dir, "data"), filepath.Join(dir, "args")
	if err := os.WriteFile(data, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$MINISRE_DU_ARGS\"\ncat \"$MINISRE_DU_DATA\"\nprintf '%s' \"$MINISRE_DU_ERROR\" >&2\nexit \"$MINISRE_DU_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "du"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MINISRE_DU_DATA", data)
	t.Setenv("MINISRE_DU_ARGS", args)
	t.Setenv("MINISRE_DU_ERROR", "")
	t.Setenv("MINISRE_DU_EXIT", exit)
	return args
}

func TestDirectoryUsageLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	root := t.TempDir()
	fake := "10\t" + root + "/small\x0020\t" + root + "/large ' $(echo x)\nname\x0035\t" + root + "\x00"
	argsFile := fakeDirectoryUsage(t, fake, "0")
	for _, host := range []string{"local", "web"} {
		raw, _ := json.Marshal(map[string]any{"host_id": host, "path": root, "limit": 1})
		out, err := Registry(runner)["get_directory_usage"](context.Background(), raw)
		if err != nil || !strings.Contains(out, "complete=true") || !strings.Contains(out, "35 bytes") ||
			!strings.Contains(out, "large ' $(echo x)\\x0aname") || strings.Contains(out, "\tsmall") || !strings.Contains(out, "showing 1 of 2") {
			t.Fatalf("%s: %q, %v", host, out, err)
		}
		flags, err := os.ReadFile(argsFile)
		if err != nil || string(flags) != "--null\n--block-size=1\n--max-depth=1\n--one-file-system\n--no-dereference\n--exclude=.*\n--\n"+root+"\n" {
			t.Fatalf("du arguments = %q, %v", flags, err)
		}
	}
}

func TestDirectoryUsageIncomplete(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, raw, exit, want string
	}{
		{"permission", "10\t" + root + "/seen\x0015\t" + root + "\x00", "1", "seen"},
		{"timeout", "10\t" + root + "/seen\x00", "124", "seen"},
		{"unfinished record", "10\t" + root + "/seen\x00999\t" + root + "/unfinished", "0", "unfinished du record"},
		{"missing total", "10\t" + root + "/seen\x00", "0", "did not return the directory total"},
		{"hidden output", "10\t" + root + "/.secret\x00", "0", "invalid du record"},
		{"unexpected depth", "10\t" + root + "/one/two\x00", "0", "outside the requested depth"},
		{"malformed", "oops\x00", "0", "invalid du record"},
		{"truncated", strings.Repeat("10\t"+root+"/seen\x00", 4000), "0", "truncated"},
		{"no output", "", "1", "No subdirectory records"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeDirectoryUsage(t, tc.raw, tc.exit)
			t.Setenv("MINISRE_DU_ERROR", "permission denied credential")
			runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "credential"}}}}
			raw, _ := json.Marshal(map[string]any{"path": root})
			out, err := Registry(runner)["get_directory_usage"](context.Background(), raw)
			if err == nil || out != "" || !strings.Contains(err.Error(), "complete=false") || !strings.Contains(err.Error(), tc.want) ||
				strings.Contains(err.Error(), "credential") || (tc.name != "truncated" && !strings.Contains(err.Error(), "[redacted]")) {
				t.Fatalf("partial report = %q, %v", out, err)
			}
		})
	}
}

func TestDirectoryUsageFilesystem(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"small", "large", ".hidden", "large/.nested"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "large/data"), []byte(strings.Repeat("x", 8192)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "large"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"path": root})
	out, err := Registry(&remote.Runner{})["get_directory_usage"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "complete=true") || strings.Contains(out, ".hidden") || strings.Contains(out, ".nested") || strings.Contains(out, "\tlink") ||
		!strings.Contains(out, "\tlarge\n") || !strings.Contains(out, "\tsmall\n") {
		t.Fatalf("real du = %q, %v", out, err)
	}
	for _, file := range []string{".hidden/data", "large/.nested/data"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(strings.Repeat("x", 8192)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := Registry(&remote.Runner{})["get_directory_usage"](context.Background(), raw)
	if err != nil || after != out {
		t.Fatalf("hidden descendants affected totals: before=%q after=%q, %v", out, after, err)
	}
}

func TestDirectoryUsageValidation(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, ".hidden")
	if err := os.Mkdir(hidden, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(hidden, link); err != nil {
		t.Fatal(err)
	}
	argsFile := fakeDirectoryUsage(t, "", "0")
	for _, args := range []map[string]any{
		{}, {"path": "relative"}, {"path": hidden}, {"path": link},
		{"path": root, "limit": 0}, {"path": root, "limit": 51}, {"path": root, "count": 1},
	} {
		raw, _ := json.Marshal(args)
		if _, err := Registry(&remote.Runner{})["get_directory_usage"](context.Background(), raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("du ran before validation")
	}
}

func TestDirectoryUsageWatchdogAndCancellation(t *testing.T) {
	root := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"path": root})
	timeoutPath, err := exec.LookPath("timeout")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "du"), []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	// Exercise the real watchdog with a shorter duration, retaining the probe's
	// positional arguments. No test waits for the production 15-second budget.
	script := "#!/bin/sh\n[ \"$1 $2 $3\" = '--signal=TERM --kill-after=1s 15s' ] || exit 90\nshift 3\nexec \"$MINISRE_REAL_TIMEOUT\" --kill-after=0.1s 0.1s \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "timeout"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINISRE_REAL_TIMEOUT", timeoutPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	_, err = Registry(&remote.Runner{})["get_directory_usage"](context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "124") || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("watchdog = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := time.AfterFunc(30*time.Millisecond, cancel)
	defer stop.Stop()
	if _, err := Registry(&remote.Runner{})["get_directory_usage"](ctx, raw); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation = %v", err)
	}
}
