package session

import (
	"context"
	"testing"

	"mini-sre/internal/agent"
)

func TestToolMarkupResponse(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		reject        bool
	}{
		{"reported", "Читаю.\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"read_file\">", true},
		{"single bars", "<｜DSML｜tool_calls>\n<｜DSML｜ invoke name=\"read_file\">", true},
		{"ascii", "<|DSML| invoke name=\"read_file\">", true},
		{"plain", "Лаборатория удалена.", false},
		{"fenced", "Пример:\n```xml\n<｜｜DSML｜｜ calls>\n```", false},
		{"tilde fence", "~~~~xml\n<|DSML| invoke>\n~~~~", false},
		{"quoted", "> <｜｜DSML｜｜ calls>\n> <｜｜DSML｜｜ invoke>", false},
		{"inline", "Разметка `<｜｜DSML｜｜ invoke>` ошибочна.", false},
		{"double inline", "Пример ``<|DSML| invoke `x`>``.", false},
		{"after example", "```\nexample\n```\n<|DSML| invoke>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
				return &agent.Response{Content: tc.content, FinishReason: "stop"}, nil
			}}
			s.RunTool = func(context.Context, agent.ToolCall) (string, error) {
				t.Fatal("text markup must never execute a tool")
				return "", nil
			}
			resp, err := s.Ask(context.Background(), "test")
			if tc.reject {
				if err == nil || resp != nil || len(s.History) != 1 {
					t.Fatalf("protocol failure was not rolled back: resp=%+v err=%v history=%d", resp, err, len(s.History))
				}
			} else if err != nil || resp.Content != tc.content {
				t.Fatalf("valid explanation rejected: %v", err)
			}
		})
	}
}
