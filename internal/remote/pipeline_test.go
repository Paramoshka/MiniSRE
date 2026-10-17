package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pipelineStage(program string, args ...string) Stage {
	return Stage{Commands: []Command{{Program: program, Args: args}}}
}

func TestPipelineStreamsAndStatuses(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			r := &Runner{}
			if host == "web" {
				r = testSSH(t, false)
			}
			plan := Pipeline{Stdout: "inline", Stderr: "inline", Stages: []Stage{
				pipelineStage("sh", "-c", "printf 'first\\nlast\\n'; printf 'problem\\n' >&2; exit 7"),
				pipelineStage("grep", "-F", "-e", "last"),
			}}
			result, err := r.RunPipeline(context.Background(), host, plan)
			if err != nil || !result.Complete || result.Stdout != "last\n" || result.Stderr != "problem\n" || result.Codes[0] != 7 || result.Codes[1] != 0 {
				t.Fatalf("pipeline = %+v, %v", result, err)
			}
			plan.Stderr = "merge"
			plan.Stages[1] = pipelineStage("grep", "-F", "-e", "problem")
			result, err = r.RunPipeline(context.Background(), host, plan)
			if err != nil || result.Stdout != "problem\n" || result.Stderr != "" {
				t.Fatalf("merge = %+v, %v", result, err)
			}
			plan.Stdout, plan.Stderr = "discard", "discard"
			result, err = r.RunPipeline(context.Background(), host, plan)
			if err != nil || result.Stdout != "" || result.Stderr != "" || result.Codes[0] != 7 {
				t.Fatalf("discard = %+v, %v", result, err)
			}
		})
	}
}

func TestPipelineDrainsAndFiltersBeforePreview(t *testing.T) {
	filter := pipelineStage("head", "-n", "1")
	filter.Drain = true
	result, err := (&Runner{}).RunPipeline(context.Background(), "local", Pipeline{Stdout: "inline", Stderr: "inline", Stages: []Stage{
		pipelineStage("sh", "-c", "yes line | head -n 100000; printf 'needle\\n'"), filter,
	}})
	if err != nil || result.Stdout != "line\n" || result.Codes[0] != 0 || result.Codes[1] != 0 {
		t.Fatalf("drain = %+v, %v", result, err)
	}
	result, err = (&Runner{}).RunPipeline(context.Background(), "local", Pipeline{Stdout: "inline", Stderr: "inline", Stages: []Stage{
		pipelineStage("sh", "-c", "head -c 100000 /dev/zero; printf '\\nneedle\\n'"), pipelineStage("grep", "-a", "-F", "-e", "needle"),
	}})
	if err != nil || result.Stdout != "needle\n" {
		t.Fatalf("late match = %+v, %v", result, err)
	}
}

func TestPipelineArtifactsAndLimits(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			r := &Runner{}
			if host == "web" {
				r = testSSH(t, false)
			}
			plan := Pipeline{Stdout: "file", Stderr: "file", Filename: "quote'$(){{FILENAME}}.txt", Stages: []Stage{pipelineStage("sh", "-c", "printf '%s' '{{PIPELINE}}'; printf diagnostic >&2; exit 3")}}
			result, err := r.RunPipeline(context.Background(), host, plan)
			if err != nil || result.Codes[0] != 3 {
				t.Fatalf("file = %+v, %v", result, err)
			}
			if filepath.Base(result.StdoutPath) != plan.Filename {
				t.Fatalf("filename changed: %q", result.StdoutPath)
			}
			dir := filepath.Dir(filepath.Dir(result.StdoutPath))
			t.Cleanup(func() { os.RemoveAll(dir) })
			for _, item := range []struct{ path, want string }{{result.StdoutPath, "{{PIPELINE}}"}, {result.StderrPath, "diagnostic"}} {
				data, err := os.ReadFile(item.path)
				if err != nil || string(data) != item.want {
					t.Fatalf("artifact = %q, %v", data, err)
				}
				info, err := os.Stat(item.path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("permissions = %v, %v", info, err)
				}
			}
			info, err := os.Stat(dir)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("directory = %v, %v", info, err)
			}
			plan.Stages = []Stage{pipelineStage("head", "-c", strconv.Itoa(ArtifactLimit+1000), "/dev/zero")}
			result, err = r.RunPipeline(context.Background(), host, plan)
			if err != nil || !result.Limited || result.StdoutBytes != ArtifactLimit {
				t.Fatalf("size limit = %+v, %v", result, err)
			}
			t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(result.StdoutPath))) })
		})
	}
}

func TestPipelineCancellationStopsChildren(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			runner := &Runner{}
			if host == "web" {
				runner = testSSH(t, false)
			}
			pidFile := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := runner.RunPipeline(ctx, host, Pipeline{Stdout: "discard", Stderr: "discard", Stages: []Stage{
				pipelineStage("sh", "-c", "echo $$ > \"$1\"; exec sleep 30", "test", pidFile),
			}})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancel = %v", err)
			}
			assertPipelineChildStopped(t, pidFile)
		})
	}
}

func assertPipelineChildStopped(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		state, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		t.Fatalf("child %d survived cancellation: %s", pid, state)
	}
}

func TestPipelineManualCancellationStopsChildren(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			runner := &Runner{}
			if host == "web" {
				runner = testSSH(t, false)
			}
			pidFile := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := time.AfterFunc(300*time.Millisecond, cancel)
			defer timer.Stop()
			_, err := runner.RunPipeline(ctx, host, Pipeline{Stdout: "discard", Stderr: "discard", Stages: []Stage{
				pipelineStage("sh", "-c", "echo $$ > \"$1\"; exec sleep 30", "test", pidFile),
			}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel = %v", err)
			}
			assertPipelineChildStopped(t, pidFile)
		})
	}
}

func TestPipelineWatchdogStopsChildren(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			runner := &Runner{}
			if host == "web" {
				runner = testSSH(t, false)
			}
			pidFile := filepath.Join(t.TempDir(), "pid")
			script, err := pipelineScript(Pipeline{Stdout: "discard", Stderr: "discard", Stages: []Stage{
				pipelineStage("sh", "-c", "echo $$ > \"$1\"; exec sleep 30", "test", pidFile),
			}})
			if err != nil {
				t.Fatal(err)
			}
			// With no parent cancellation, only the host-side watchdog can stop the worker.
			result, err := runner.run(context.Background(), host, true, "timeout", "--foreground",
				"--signal=TERM", "--kill-after=1s", "0.3", "bash", "--noprofile", "--norc", "-p", "-c", script)
			if err == nil || result.ExitCode != 124 {
				t.Fatalf("watchdog: exit=%d err=%v", result.ExitCode, err)
			}
			assertPipelineChildStopped(t, pidFile)
		})
	}
}

func TestPipelineRejectsMalformedPlans(t *testing.T) {
	for _, plan := range []Pipeline{
		{}, {Stages: []Stage{{}}}, {Stages: []Stage{pipelineStage("printf", "%s", "ok")}, Stdout: "merge"},
		{Stages: []Stage{pipelineStage("printf", "%s", "ok")}, Filename: "../outside"},
		{Stages: []Stage{pipelineStage("printf", "%s", "a\x00b")}},
	} {
		if _, err := pipelineScript(plan); err == nil {
			t.Fatalf("accepted %+v", plan)
		}
	}
}
