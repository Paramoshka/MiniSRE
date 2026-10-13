package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"mini-sre/internal/agent"
)

func TestHistoryKeepsWholeToolTurns(t *testing.T) {
	s := &Session{MaxHistoryTurns: 2}
	s.RunTool = func(context.Context, agent.ToolCall) (string, error) { return "checked", nil }
	s.Turn = func(_ context.Context, req agent.Request) (*agent.Response, error) {
		if req.Messages[len(req.Messages)-1].Role == agent.RoleUser {
			return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "call", Name: "probe"}}}, nil
		}
		return &agent.Response{Content: "done"}, nil
	}
	for i := range 4 {
		if _, err := s.Ask(context.Background(), fmt.Sprintf("question %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.History) != 9 || s.History[1].Content != "question 2" || s.History[5].Content != "question 3" {
		t.Fatalf("history dropped part of a turn: %+v", s.History)
	}
	for _, start := range []int{1, 5} {
		if len(s.History[start+1].ToolCalls) != 1 || s.History[start+2].ToolCallID != "call" {
			t.Fatal("tool call/result pair was split")
		}
	}
	s.Reset()
	if len(s.History) != 1 || s.History[0].Role != agent.RoleSystem {
		t.Fatal("reset did not clear history")
	}
}

func TestSessionRollbackAndToolErrors(t *testing.T) {
	s := &Session{}
	s.Turn = func(_ context.Context, req agent.Request) (*agent.Response, error) {
		if req.Messages[len(req.Messages)-1].Role == agent.RoleUser {
			return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "call", Name: "probe"}}}, nil
		}
		if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "permission denied") {
			t.Fatal("tool error was hidden")
		}
		return &agent.Response{Content: "check failed"}, nil
	}
	s.RunTool = func(context.Context, agent.ToolCall) (string, error) { return "", errors.New("permission denied") }
	if _, err := s.Ask(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	base := len(s.History)
	s.Turn = func(context.Context, agent.Request) (*agent.Response, error) {
		return nil, errors.New("model unavailable")
	}
	if _, err := s.Ask(context.Background(), "second"); err == nil || len(s.History) != base {
		t.Fatal("failed model request was not rolled back")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Ask(ctx, "cancel"); !errors.Is(err, context.Canceled) || len(s.History) != base {
		t.Fatal("cancelled request was not rolled back")
	}
}

func TestSessionFinalAnswerAfterFiveToolRounds(t *testing.T) {
	for _, finalCallsTool := range []bool{false, true} {
		t.Run(fmt.Sprint(finalCallsTool), func(t *testing.T) {
			turns, executions := 0, 0
			s := &Session{Tools: []agent.Tool{{Name: "probe"}}}
			s.Turn = func(_ context.Context, req agent.Request) (*agent.Response, error) {
				turns++
				if turns <= 5 {
					if len(req.Tools) != 1 || req.DisableToolCalls {
						t.Fatal("tools disabled before the round limit")
					}
					return &agent.Response{ToolCalls: []agent.ToolCall{{ID: fmt.Sprint(turns), Name: "probe"}}}, nil
				}
				if len(req.Tools) != 1 || !req.DisableToolCalls || req.Messages[len(req.Messages)-2].Content != "result 5" || req.Messages[len(req.Messages)-1].Role != agent.RoleSystem {
					t.Fatal("final turn must receive the fifth result with schemas retained and tool calls disabled")
				}
				if finalCallsTool {
					return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "extra", Name: "probe"}}}, nil
				}
				return &agent.Response{Content: "done"}, nil
			}
			s.RunTool = func(context.Context, agent.ToolCall) (string, error) {
				executions++
				return fmt.Sprintf("result %d", executions), nil
			}
			resp, err := s.Ask(context.Background(), "diagnose")
			wantTurns := 6
			if finalCallsTool {
				wantTurns++
			}
			if turns != wantTurns || executions != 5 {
				t.Fatalf("turns=%d executions=%d, want %d and 5", turns, executions, wantTurns)
			}
			if finalCallsTool {
				if !errors.Is(err, ErrResponseFormat) || len(s.History) != 1 {
					t.Fatalf("extra tool round was not rejected and rolled back: %v", err)
				}
			} else if err != nil || resp.Content != "done" || len(s.History) != 13 {
				t.Fatalf("final answer failed: response=%+v err=%v history=%d", resp, err, len(s.History))
			}
		})
	}
}

func TestRestoreRejectsIncompleteOrUnmatchedToolHistory(t *testing.T) {
	user := agent.Message{Role: agent.RoleUser, Content: "question"}
	call := agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "a", Name: "probe"}, {ID: "b", Name: "probe"}}}
	resultA := agent.Message{Role: agent.RoleTool, ToolCallID: "a", Content: "result a"}
	resultB := agent.Message{Role: agent.RoleTool, ToolCallID: "b", Content: "result b"}
	final := agent.Message{Role: agent.RoleAssistant, Content: "answer"}
	s := &Session{SystemPrompt: "current prompt", MaxHistoryTurns: 1}
	valid := []agent.Message{user, call, resultB, resultA, final}
	if err := s.Restore(append([]agent.Message{user, final}, valid...)); err != nil {
		t.Fatal(err)
	}
	if len(s.History) != 6 || s.History[0].Content != "current prompt" || s.History[2].ToolCalls[1].ID != "b" {
		t.Fatal("restore did not keep current prompt and one complete tool turn")
	}
	for _, invalid := range [][]agent.Message{
		{user}, {user, call}, {user, call, resultA, final},
		{user, call, resultA, resultA, final}, {user, call, resultA, resultB},
		{user, final, resultA}, {{Role: agent.RoleSystem}}, {{Role: "unknown"}},
		{user, {Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{Name: "probe"}}}},
		{user, {Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "a"}}}},
		{user, {Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "a", Name: "probe"}, {ID: "a", Name: "probe"}}}},
	} {
		if err := s.Restore(invalid); err == nil || len(s.History) != 6 {
			t.Fatalf("bad history accepted or existing context changed: %+v, %v", invalid, err)
		}
	}
}

func TestLanguageChangesWithoutClearingHistory(t *testing.T) {
	s := &Session{SystemPrompt: "Return JSON with status and summary.", Language: "ru"}
	for _, language := range []string{"ru", "en", "ru"} {
		s.Language = language
		s.Turn = func(_ context.Context, req agent.Request) (*agent.Response, error) {
			want, unwanted := "in Russian", "in English"
			if language == "en" {
				want, unwanted = unwanted, want
			}
			prompt := req.Messages[0].Content
			if !strings.HasPrefix(prompt, s.SystemPrompt) || !strings.Contains(prompt, want) || strings.Contains(prompt, unwanted) || strings.Count(prompt, "Always write") != 1 {
				t.Fatalf("language instruction not updated: %q", prompt)
			}
			return &agent.Response{Content: `{"status":"unknown","summary":"test"}`}, nil
		}
		if _, err := s.Ask(context.Background(), "check"); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.History) != 7 {
		t.Fatal("language change cleared conversation history")
	}
	restored := &Session{Language: "en", SystemPrompt: s.SystemPrompt}
	if err := restored.Restore(s.History[1:]); err != nil || !strings.Contains(restored.History[0].Content, "in English") {
		t.Fatalf("language not applied on restore: %v", err)
	}
}
