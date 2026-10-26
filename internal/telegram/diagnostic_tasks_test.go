package telegram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/agent"
	"mini-sre/internal/config"
	"mini-sre/internal/remote"
	"mini-sre/internal/session"
	"mini-sre/internal/tools"
)

func TestTaskIncompleteDirectoryUsageCannotBeHealthy(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nprintf '4096\\t%s/seen\\000' \"$MINISRE_TASK_ROOT\"\nprintf 'permission denied task-secret' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "du"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("MINISRE_TASK_ROOT", root)
	raw, _ := json.Marshal(map[string]any{"path": root})
	registry := tools.Registry(&remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "task-secret"}}}})
	b, _ := testTasks(t)
	b.newSession = func() *session.Session {
		return &session.Session{
			Tools: []agent.Tool{{Name: "get_disk_usage"}, {Name: "get_directory_usage"}},
			Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
				if len(req.Messages) == 2 {
					return &agent.Response{ToolCalls: []agent.ToolCall{
						{ID: "disk", Name: "get_disk_usage"},
						{ID: "dirs", Name: "get_directory_usage", Arguments: string(raw)},
					}}, nil
				}
				found := false
				for _, message := range req.Messages {
					if message.Role != agent.RoleTool || message.ToolCallID != "dirs" {
						continue
					}
					found = true
					if !strings.HasPrefix(message.Content, "error:") || !strings.Contains(message.Content, "complete=false") ||
						!strings.Contains(message.Content, "seen") || !strings.Contains(message.Content, "[redacted]") || strings.Contains(message.Content, "task-secret") {
						t.Fatalf("partial observations were lost or exposed credentials: %q", message.Content)
					}
				}
				if !found {
					t.Fatal("model did not receive directory result")
				}
				// Even after another successful probe, an incorrect model verdict
				// cannot turn incomplete evidence into a healthy scheduled check.
				return &agent.Response{Content: `{"status":"ok","summary":"healthy"}`}, nil
			},
			RunTool: func(ctx context.Context, call agent.ToolCall) (string, error) {
				if call.Name == "get_disk_usage" {
					return "available=1 GiB", nil
				}
				return registry[call.Name](ctx, json.RawMessage(call.Arguments))
			},
		}
	}
	result := b.checkTask(context.Background(), scheduledTask{OwnerID: 1, Prompt: "Check disk space"})
	if result.Status != "unknown" {
		t.Fatalf("incomplete check became healthy: %+v", result)
	}
}
