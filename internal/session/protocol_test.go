package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
			turns := 0
			s := &Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
				turns++
				return &agent.Response{Content: tc.content, FinishReason: "stop"}, nil
			}}
			s.RunTool = func(context.Context, agent.ToolCall) (string, error) {
				t.Fatal("text markup must never execute a tool")
				return "", nil
			}
			resp, err := s.Ask(context.Background(), "test")
			if tc.reject {
				if !errors.Is(err, ErrResponseFormat) || resp != nil || len(s.History) != 1 || turns != 2 {
					t.Fatalf("protocol failure was not rolled back: resp=%+v err=%v history=%d", resp, err, len(s.History))
				}
			} else if err != nil || resp.Content != tc.content {
				t.Fatalf("valid explanation rejected: %v", err)
			}
		})
	}
}

func TestFinalAnswerRepairPreservesObservations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toolRounds int
		broken     agent.Response
	}{
		{"markup with pending call", 1, agent.Response{Content: "<|DSML| invoke>", ToolCalls: []agent.ToolCall{{ID: "rejected", Name: "probe"}}}},
		{"markup at round limit", 5, agent.Response{Content: "<|DSML| invoke>"}},
		{"structured call at round limit", 5, agent.Response{ToolCalls: []agent.ToolCall{{ID: "rejected", Name: "probe"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns, executions := 0, 0
			s := &Session{Tools: []agent.Tool{{Name: "probe"}}}
			previous := []agent.Message{{Role: agent.RoleUser, Content: "previous"}, {Role: agent.RoleAssistant, Content: "answer"}}
			if err := s.Restore(previous); err != nil {
				t.Fatal(err)
			}
			s.Turn = func(_ context.Context, req agent.Request) (*agent.Response, error) {
				turns++
				if turns <= tc.toolRounds {
					return &agent.Response{ToolCalls: []agent.ToolCall{{ID: fmt.Sprint(turns), Name: "probe"}}}, nil
				}
				if turns == tc.toolRounds+1 {
					return &tc.broken, nil
				}
				if !req.DisableToolCalls || len(req.Tools) != 1 || req.Messages[len(req.Messages)-1].Role != agent.RoleSystem {
					t.Fatal("repair must retain schemas and explicitly request a final answer without tools")
				}
				if !reflect.DeepEqual(req.Messages[:len(req.Messages)-1], s.History) {
					t.Fatal("repair must use only the last valid history")
				}
				if req.Messages[len(req.Messages)-2].Content != "error: service not found" {
					t.Fatal("repair lost diagnostic error evidence")
				}
				return &agent.Response{Content: "Unit not found; service existence is not established."}, nil
			}
			s.RunTool = func(_ context.Context, call agent.ToolCall) (string, error) {
				if call.ID == "rejected" {
					t.Fatal("rejected tool call must never execute")
				}
				executions++
				return "", errors.New("service not found")
			}
			resp, err := s.Ask(context.Background(), "check")
			if err != nil || resp == nil || turns != tc.toolRounds+2 || executions != tc.toolRounds {
				t.Fatalf("resp=%+v err=%v turns=%d executions=%d", resp, err, turns, executions)
			}
			if !reflect.DeepEqual(s.History[1:3], previous) {
				t.Fatal("previous history changed")
			}
			restored := &Session{}
			if err := restored.Restore(s.History[1:]); err != nil {
				t.Fatalf("repaired history is incomplete or contains a temporary instruction: %v", err)
			}
		})
	}
}

func TestFinalAnswerRepairFailureRollsBack(t *testing.T) {
	modelErr := errors.New("model unavailable")
	for _, tc := range []struct {
		name string
		resp *agent.Response
		err  error
	}{
		{"markup", &agent.Response{Content: "<|DSML| invoke>"}, nil},
		{"tool call", &agent.Response{Content: "Trying again", ToolCalls: []agent.ToolCall{{ID: "rejected", Name: "probe"}}}, nil},
		{"empty", &agent.Response{Content: " \n"}, nil},
		{"nil response", nil, nil},
		{"model error", nil, modelErr},
		{"model timeout", nil, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := 0
			s := &Session{}
			s.Reset()
			before := append([]agent.Message(nil), s.History...)
			s.Turn = func(context.Context, agent.Request) (*agent.Response, error) {
				turns++
				if turns == 1 {
					return &agent.Response{Content: "<|DSML| invoke>"}, nil
				}
				return tc.resp, tc.err
			}
			s.RunTool = func(context.Context, agent.ToolCall) (string, error) {
				t.Fatal("repair must not run tools")
				return "", nil
			}
			resp, err := s.Ask(context.Background(), "check")
			wantErr := tc.err
			if wantErr == nil {
				wantErr = ErrResponseFormat
			}
			if resp != nil || !errors.Is(err, wantErr) || turns != 2 || !reflect.DeepEqual(s.History, before) {
				t.Fatalf("resp=%+v err=%v turns=%d history=%+v", resp, err, turns, s.History)
			}
		})
	}
}

func TestFinalAnswerRepairCancellation(t *testing.T) {
	for _, cancelTurn := range []int{1, 2} {
		t.Run(fmt.Sprint(cancelTurn), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			turns := 0
			s := &Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
				turns++
				if turns == cancelTurn {
					cancel()
				}
				return &agent.Response{Content: "<|DSML| invoke>"}, nil
			}}
			if _, err := s.Ask(ctx, "check"); !errors.Is(err, context.Canceled) || turns != cancelTurn || len(s.History) != 1 {
				t.Fatalf("err=%v turns=%d history=%+v", err, turns, s.History)
			}
		})
	}
}
