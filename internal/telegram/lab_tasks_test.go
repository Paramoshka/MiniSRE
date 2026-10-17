package telegram

import (
	"context"
	"testing"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

func TestScheduledChecksCannotMutateLabs(t *testing.T) {
	for _, name := range []string{"lab_deploy", "lab_destroy"} {
		b, _ := testTasks(t)
		calls, runs := 0, 0
		b.newSession = func() *session.Session {
			return &session.Session{
				Tools: []agent.Tool{
					{Name: "lab_deploy", Mutating: true}, {Name: "lab_destroy", Mutating: true},
					{Name: "lab_inspect"}, {Name: "lab_run"},
				},
				Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
					calls++
					for _, tool := range req.Tools {
						if tool.Name == "lab_deploy" || tool.Name == "lab_destroy" {
							t.Fatal("scheduled model sees mutating tools")
						}
					}
					if calls == 1 {
						return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "blocked", Name: name}}}, nil
					}
					return &agent.Response{Content: `{"status":"ok","summary":"success"}`}, nil
				},
				RunTool: func(context.Context, agent.ToolCall) (string, error) { runs++; return "unexpected", nil },
			}
		}
		result := b.checkTask(context.Background(), scheduledTask{OwnerID: 1, Prompt: "create a lab"})
		if result.Status != "unknown" || runs != 0 {
			t.Fatalf("mutation reached executor: %+v runs=%d", result, runs)
		}
	}
}
