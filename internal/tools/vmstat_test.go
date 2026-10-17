package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

const vmstatHeaders = "procs --------memory-------- ---swap-- -----io---- -system-- ------cpu-----\n" +
	" r b swpd free buff cache si so bi bo in cs us sy id wa st gu\n"
const vmstatSample = " 1 0 0 100 20 30 0 0 0 8 40 60 10 5 85 0 0 0\n"

func TestVMStatLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			raw := vmstatHeaders + strings.Repeat(vmstatSample, 3)
			argsFile := fakeCommand(t, "vmstat", raw, "", "0")
			registry := Registry(runner)
			out, err := registry["get_vmstat"](context.Background(), json.RawMessage(`{"host_id":"`+host+`"}`))
			if err != nil || !strings.HasSuffix(out, raw) || !strings.Contains(out, "3 samples at 1-second intervals") || !strings.Contains(out, "KiB") {
				t.Fatalf("sample = %q, %v", out, err)
			}
			out, err = registry["run_pipeline"](context.Background(), json.RawMessage(`{"host_id":"`+host+`","source":{"tool":"get_vmstat"},"filters":[{"type":"tail","lines":1}]}`))
			if err != nil || !strings.Contains(out, vmstatSample) || !strings.Contains(out, "stage[0]=get_vmstat exit_code=0") || strings.Contains(out, "since-boot report omitted") {
				t.Fatalf("pipeline = %q, %v", out, err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil || string(args) != "-w\n-n\n-y\n-S\nK\n1\n3\n" {
				t.Fatalf("vmstat arguments = %q, %v", args, err)
			}
		})
	}
}

func TestVMStatRejectsArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "vmstat", "", "", "0")
	call := Registry(&remote.Runner{})["get_vmstat"]
	for _, raw := range []string{`{"count":5}`, `{"path":"/tmp"}`, `{"flags":"-m"}`, `{"host_id":123}`, `null`, `[]`, `{bad`} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("vmstat ran before argument validation")
	}
}

func TestVMStatErrorsAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, exit string
		wantErr                    bool
	}{
		{"empty", "", "", "0", true},
		{"headers only", vmstatHeaders, "", "0", true},
		{"incomplete sample", vmstatHeaders + vmstatSample, "", "0", true},
		{"too many samples", vmstatHeaders + strings.Repeat(vmstatSample, 4), "", "0", true},
		{"truncated", strings.Repeat("secret\n", 11000), "", "0", true},
		{"command failure", "", "vmstat: permission denied secret", "1", true},
		{"redacted output", "secret " + vmstatHeaders + strings.Repeat(vmstatSample, 3), "", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "vmstat", tc.stdout, tc.stderr, tc.exit)
			runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
			out, err := Registry(runner)["get_vmstat"](context.Background(), nil)
			if (err != nil) != tc.wantErr || strings.Contains(out, "secret") || (err != nil && strings.Contains(err.Error(), "secret")) {
				t.Fatalf("output/error = %q, %v", out, err)
			}
			if !tc.wantErr && !strings.Contains(out, "[redacted]") {
				t.Fatal("known credential was not redacted")
			}
		})
	}
}

func TestVMStatMissingAndCancelled(t *testing.T) {
	t.Run("missing command", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := (&Tools{Runner: &remote.Runner{}}).VMStat(context.Background(), "local"); err == nil {
			t.Fatal("missing vmstat was accepted")
		}
	})
	t.Run("cancelled sampling", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "vmstat"), []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := (&Tools{Runner: &remote.Runner{}}).VMStat(ctx, "local"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancellation = %v", err)
		}
	})
}

func TestVMStatLocally(t *testing.T) {
	if _, err := exec.LookPath("vmstat"); err != nil {
		t.Skip(err)
	}
	out, err := Registry(&remote.Runner{})["get_vmstat"](context.Background(), nil)
	if err != nil || !strings.Contains(out, "swpd") || !strings.Contains(out, "since-boot report omitted") {
		t.Fatalf("local sample = %q, %v", out, err)
	}
}
