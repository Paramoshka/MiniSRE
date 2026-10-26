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

const diskIOSample = "Device r/s rkB/s r_await w/s wkB/s w_await aqu-sz %util\nsda 4.00 16.00 1.50 2.00 8.00 3.00 0.10 20.00\n\n"

func TestDiskIOInstalledUtility(t *testing.T) {
	if _, err := exec.LookPath("iostat"); err != nil {
		t.Skip("sysstat iostat is not installed")
	}
	t.Setenv("S_COLORS", "always")
	out, err := Registry(&remote.Runner{})["get_disk_io"](context.Background(), nil)
	if err != nil || !strings.Contains(out, "r/s") || strings.Contains(out, "\x1b[") {
		t.Fatalf("installed iostat = %q, %v", out, err)
	}
}

func TestDiskIOLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	raw := "Linux 6.8 (host) 10/26/26 _x86_64_ (2 CPU)\n\n" + strings.Repeat(diskIOSample, 3)
	argsFile := fakeCommand(t, "iostat", raw, "", "0")
	t.Setenv("S_COLORS", "always")
	command := filepath.Join(filepath.Dir(argsFile), "iostat")
	script, err := os.ReadFile(command)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "#!/bin/sh\n", "#!/bin/sh\n[ \"$S_COLORS\" = never ] || exit 9\n", 1))
	if err := os.WriteFile(command, script, 0700); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"local", "web"} {
		out, err := Registry(runner)["get_disk_io"](context.Background(), json.RawMessage(`{"host_id":"`+host+`"}`))
		if err != nil || !strings.HasSuffix(out, raw) || !strings.Contains(out, "since-boot report omitted") {
			t.Fatalf("%s = %q, %v", host, out, err)
		}
		flags, err := os.ReadFile(argsFile)
		if err != nil || string(flags) != "-d\n-x\n-k\n-y\n1\n3\n" {
			t.Fatalf("arguments = %q, %v", flags, err)
		}
	}
}

func TestDiskIOReportsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, raw, exit string
		wantErr         bool
	}{
		{"normal", strings.Repeat(diskIOSample, 3), "0", false},
		{"older headers", strings.Repeat("Device: r/s w/s rkB/s wkB/s await avgqu-sz %util\nsda 0 0 0 0 0 0 0\n", 3), "0", false},
		{"empty", "", "0", true},
		{"missing sample", strings.Repeat(diskIOSample, 2), "0", true},
		{"extra sample", strings.Repeat(diskIOSample, 4), "0", true},
		{"unsupported columns", strings.ReplaceAll(strings.Repeat(diskIOSample, 3), "r_await", "unknown"), "0", true},
		{"incomplete row", strings.Repeat(diskIOSample, 3) + "sdb 4\n", "0", true},
		{"NaN", strings.ReplaceAll(strings.Repeat(diskIOSample, 3), "1.50", "NaN"), "0", true},
		{"no devices", strings.Repeat("Device r/s rkB/s r_await w/s wkB/s w_await aqu-sz %util\n", 3), "0", true},
		{"truncated", strings.Repeat(diskIOSample, 1000), "0", true},
		{"command failed", "", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "iostat", tc.raw, "secret warning\n", tc.exit)
			runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
			out, err := Registry(runner)["get_disk_io"](context.Background(), nil)
			if (err != nil) != tc.wantErr || strings.Contains(out, "secret") || (err != nil && strings.Contains(err.Error(), "secret")) {
				t.Fatalf("report = %q, %v", out, err)
			}
		})
	}
}

func TestDiskIOValidationAndCancellation(t *testing.T) {
	argsFile := fakeCommand(t, "iostat", "", "", "0")
	for _, raw := range []string{`{"count":5}`, `{"host_id":123}`, `null`} {
		if _, err := Registry(&remote.Runner{})["get_disk_io"](context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("iostat ran before validation")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "iostat"), []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Registry(&remote.Runner{})["get_disk_io"](ctx, nil); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("cancellation = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Registry(&remote.Runner{})["get_disk_io"](context.Background(), nil); err == nil {
		t.Fatal("missing iostat accepted")
	}
}
