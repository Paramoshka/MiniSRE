package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

func TestErrorLanguageChangesBetweenRequests(t *testing.T) {
	for _, tc := range []struct {
		name, input, content, ru, en string
		err                          error
	}{
		{"model error", "check", "", "Не удалось выполнить запрос", "Could not complete the request", errors.New("private model error")},
		{"timeout", "check", "", "Модель не ответила", "The model did not respond", context.DeadlineExceeded},
		{"format error", "check", "<|DSML| invoke>", "Не удалось сформировать итог", "Could not produce the diagnostic summary", nil},
		{"empty answer", "check", "", "Модель вернула пустой ответ", "The model returned an empty response", nil},
		{"unknown command", "/unknown", "", "Неизвестная команда", "Unknown command", nil},
		{"tasks disabled", "/task list", "", "Плановые задачи не включены", "Scheduled tasks are not enabled", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			language := "ru"
			b := stateBot(t, "prompt", func(_ context.Context, req agent.Request) (*agent.Response, error) {
				want := "in Russian"
				if language == "en" {
					want = "in English"
				}
				if !strings.Contains(req.Messages[0].Content, want) {
					t.Fatal("model and error response languages disagree")
				}
				return &agent.Response{Content: tc.content}, tc.err
			})
			b.SetLanguage(func() string { return language })
			for _, next := range []string{"ru", "en", "ru"} {
				language = next
				text, err := b.handle(context.Background(), privateMessage(t, 1, tc.input), 1, func() {})
				want := tc.ru
				if next == "en" {
					want = tc.en
				}
				if err != nil || !strings.Contains(text, want) || strings.Contains(text, "private model error") {
					t.Fatalf("language=%s text=%q err=%v", next, text, err)
				}
			}
		})
	}
}

func TestRequestKeepsLanguageDuringReload(t *testing.T) {
	language := "en"
	b := stateBot(t, "prompt", func(_ context.Context, req agent.Request) (*agent.Response, error) {
		if !strings.Contains(req.Messages[0].Content, "in English") {
			t.Fatal("wrong initial model language")
		}
		language = "ru"
		return nil, errors.New("model failed")
	})
	b.SetLanguage(func() string { return language })
	text, err := b.handle(context.Background(), privateMessage(t, 1, "проверь"), 1, func() {})
	if err != nil || !strings.Contains(text, "Could not complete the request") {
		t.Fatalf("reload changed an in-flight reply's language: %q, %v", text, err)
	}
}

func TestScheduledCheckAndCommandErrorLanguage(t *testing.T) {
	for _, language := range []string{"ru", "en"} {
		t.Run(language, func(t *testing.T) {
			b, scheduler := testTasks(t)
			b.SetLanguage(func() string { return language })
			b.newSession = func() *session.Session {
				return &session.Session{Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
					want := "in Russian"
					if language == "en" {
						want = "in English"
					}
					if !strings.Contains(req.Messages[0].Content, want) || !strings.Contains(req.Messages[0].Content, "Return exactly one JSON object") {
						t.Fatal("scheduled check lost response language or JSON contract")
					}
					return nil, errors.New("model unavailable")
				}}
			}
			result := b.checkTask(context.Background(), scheduledTask{OwnerID: 1, Prompt: "check"})
			wantSummary := "Не удалось выполнить проверку"
			if language == "en" {
				wantSummary = "Could not run the check"
			}
			if result.Status != "unknown" || !strings.Contains(result.Summary, wantSummary) {
				t.Fatalf("wrong scheduled error: %+v", result)
			}
			for _, tc := range []struct{ input, ru, en string }{
				{"/task show missing", "Задача не найдена", "Task not found"},
				{"/task add 30s 1h check", "Интервал проверки", "The check interval"},
				{"/task add 1h 5m check", "Интервал отчёта", "The report interval"},
			} {
				reply := scheduler.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: tc.input})
				want := tc.ru
				if language == "en" {
					want = tc.en
				}
				if reply.err != nil || !strings.Contains(reply.text, want) {
					t.Fatalf("wrong command error: %+v", reply)
				}
			}
		})
	}
}
