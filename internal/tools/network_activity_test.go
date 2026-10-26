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
	"time"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

func activityLink(index int, name string, value uint64) string {
	counters := fmt.Sprintf(`{"bytes":%d,"packets":%d,"errors":%d,"dropped":%d}`, value, value, value, value)
	return fmt.Sprintf(`{"ifindex":%d,"ifname":%q,"stats64":{"rx":%s,"tx":%s}}`, index, name, counters, counters)
}

func activitySamples(first, second string) string {
	return "100.00\n" + first + "\n102.00\n" + second + "\n"
}

func TestNetworkActivityInstalledUtility(t *testing.T) {
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 ip is not installed")
	}
	out, err := Registry(&remote.Runner{})["get_network_activity"](context.Background(), json.RawMessage(`{"interface":"lo"}`))
	if err != nil || !strings.Contains(out, "lo (ifindex=") || !strings.Contains(out, "RX bytes/s=") {
		t.Fatalf("installed ip = %q, %v", out, err)
	}
}

func TestNetworkActivityReports(t *testing.T) {
	first := "[" + activityLink(2, "eth0", 100) + "]"
	second := "[" + activityLink(2, "eth0", 120) + "]"
	for _, tc := range []struct {
		name, raw, want string
		wantErr         bool
	}{
		{"growth", activitySamples(first, second), "RX bytes/s=10.0 packets=20 errors=20 drops=20", false},
		{"zero", activitySamples(first, first), "RX bytes/s=0.0 packets=0 errors=0 drops=0", false},
		{"stats fallback", strings.ReplaceAll(activitySamples(first, second), "stats64", "stats"), "RX bytes/s=10.0", false},
		{"large integers", activitySamples("["+activityLink(2, "eth0", 9007199254740992)+"]", "["+activityLink(2, "eth0", 9007199254740993)+"]"), "RX bytes/s=0.5 packets=1", false},
		{"reset", activitySamples(second, first), "counter reset", true},
		{"disappeared", activitySamples(first, "[]"), "interface changed", true},
		{"new interface", activitySamples("[]", second), "interface changed", true},
		{"recreated", activitySamples(first, "["+activityLink(3, "eth0", 120)+"]"), "interface changed", true},
		{"missing counter", activitySamples(first, strings.ReplaceAll(second, `"dropped":120`, `"other":120`)), "counters missing", true},
		{"no interfaces", activitySamples("[]", "[]"), "No interface measurements", true},
		{"zero elapsed", strings.Replace(activitySamples(first, second), "102.00", "100.00", 1), "invalid network sample interval", true},
		{"backwards time", strings.Replace(activitySamples(first, second), "102.00", "99.00", 1), "invalid network sample interval", true},
		{"null time", strings.Replace(activitySamples(first, second), "100.00", "null", 1), "invalid network sample timestamp", true},
		{"bad second snapshot", activitySamples(first, "["), "incomplete or unsupported", true},
		{"duplicate identity", activitySamples(first, "["+activityLink(2, "eth0", 120)+","+activityLink(2, "eth1", 120)+"]"), "duplicate", true},
		{"trailing data", activitySamples(first, second) + "[]", "trailing data", true},
		{"truncated", activitySamples(first, second) + strings.Repeat(" ", 65536), "truncated", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "timeout", tc.raw, "", "0")
			out, err := Registry(&remote.Runner{})["get_network_activity"](context.Background(), nil)
			text := out
			if err != nil {
				text = err.Error()
			}
			if (err != nil) != tc.wantErr || !strings.Contains(text, tc.want) {
				t.Fatalf("report = %q, %v", out, err)
			}
		})
	}
}

func TestNetworkActivityLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			dir := t.TempDir()
			argsFile, state := filepath.Join(dir, "args"), filepath.Join(dir, "state")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$MINISRE_NET_ARGS\"\nif [ -e \"$MINISRE_NET_STATE\" ]; then\nprintf '%s\\n' \"$MINISRE_NET_SECOND\"\nelse\n: > \"$MINISRE_NET_STATE\"\nprintf '%s\\n' \"$MINISRE_NET_FIRST\"\nfi\n"
			if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("MINISRE_NET_ARGS", argsFile)
			t.Setenv("MINISRE_NET_STATE", state)
			t.Setenv("MINISRE_NET_FIRST", "["+activityLink(2, "eth0", 100)+"]")
			t.Setenv("MINISRE_NET_SECOND", "["+activityLink(2, "eth0", 120)+"]")
			out, err := Registry(runner)["get_network_activity"](context.Background(), json.RawMessage(`{"host_id":"`+host+`","interface":"eth0"}`))
			if err != nil || !strings.Contains(out, "packets=20 errors=20 drops=20") || !strings.Contains(out, "probe-host uptime") {
				t.Fatalf("sampling = %q, %v", out, err)
			}
			flags, err := os.ReadFile(argsFile)
			if err != nil || string(flags) != strings.Repeat("-j\n-s\nlink\nshow\ndev\neth0\n", 2) {
				t.Fatalf("ip arguments = %q, %v", flags, err)
			}
		})
	}
}

func TestNetworkActivityValidationAndFailure(t *testing.T) {
	argsFile := fakeCommand(t, "timeout", "", "secret missing ip", "127")
	registry := Registry(&remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}})
	for _, raw := range []string{`{"interface":"eth0; echo"}`, `{"interface":"-x"}`, `{"interface":1}`, `{"duration":3}`, `null`} {
		if _, err := registry["get_network_activity"](context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("sampling ran before validation")
	}
	if _, err := registry["get_network_activity"](context.Background(), nil); err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("command error = %v", err)
	}
}

func TestNetworkActivityCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := time.AfterFunc(50*time.Millisecond, cancel)
	defer stop.Stop()
	if _, err := Registry(&remote.Runner{})["get_network_activity"](ctx, nil); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestNetworkActivitySecondSnapshotFailure(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ -e \"$MINISRE_NET_STATE\" ]; then\nprintf 'second snapshot failed' >&2\nexit 1\nfi\n: > \"$MINISRE_NET_STATE\"\nprintf '%s\\n' \"$MINISRE_NET_FIRST\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MINISRE_NET_STATE", filepath.Join(dir, "state"))
	t.Setenv("MINISRE_NET_FIRST", "["+activityLink(2, "eth0", 100)+"]")
	out, err := Registry(&remote.Runner{})["get_network_activity"](context.Background(), nil)
	if out != "" || err == nil || !strings.Contains(err.Error(), "second snapshot failed") || strings.Contains(err.Error(), "RX bytes/s=") {
		t.Fatalf("failed second snapshot produced measurements: %q, %v", out, err)
	}
}

func TestNewDiagnosticsNotPipelineSources(t *testing.T) {
	for _, name := range []string{"get_directory_usage", "get_disk_io", "get_network_activity"} {
		if _, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), json.RawMessage(`{"source":{"tool":"`+name+`"}}`)); err == nil {
			t.Fatalf("%s accepted as a pipeline source", name)
		}
		for _, spec := range Specs() {
			if spec.Name == name && spec.Mutating {
				t.Fatalf("%s unexpectedly mutating", name)
			}
		}
	}
}
