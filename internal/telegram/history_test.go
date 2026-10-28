package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mini-sre/internal/agent"
)

// Exercise polling and delivery with a local Telegram adapter. A second poll
// ends the run with a nonretryable error instead of waiting on real services.
func pollHistoryUpdates(t *testing.T, b *Bot, updates []update, onReply func(sentMessage)) ([]sentMessage, error) {
	t.Helper()
	var mu sync.Mutex
	var replies []sentMessage
	delivered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		switch r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] {
		case "getWebhookInfo":
			io.WriteString(w, `{"ok":true,"result":{"url":""}}`)
		case "getUpdates":
			if delivered {
				w.WriteHeader(http.StatusConflict)
				io.WriteString(w, `{"ok":false,"error_code":409,"description":"stop history test"}`)
				return
			}
			delivered = true
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
		case "sendMessage", "editMessageText":
			var reply sentMessage
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Error(err)
			}
			if onReply != nil {
				onReply(reply)
			}
			mu.Lock()
			replies = append(replies, reply)
			mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":{}}`)
		default:
			t.Errorf("unexpected Telegram operation: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	b.baseURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	return append([]sentMessage(nil), replies...), err
}

func requireHistoryPollStopped(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "stop history test") {
		t.Fatalf("unexpected polling result: %v", err)
	}
}

func TestTelegramRetainsCompleteHistoryAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	var lastRequest []agent.Message
	turn := func(_ context.Context, req agent.Request) (*agent.Response, error) {
		lastRequest = append([]agent.Message(nil), req.Messages...)
		if req.Messages[len(req.Messages)-1].Role == agent.RoleUser {
			return &agent.Response{ReasoningContent: "reasoning", ToolCalls: []agent.ToolCall{
				{ID: "probe", Name: "load", Arguments: "{}"},
			}}, nil
		}
		return &agent.Response{Content: "answer"}, nil
	}
	b := stateBot(t, "old prompt", turn)
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	var updates []update
	for i := range 25 {
		updates = append(updates, update{ID: int64(i), Message: privateMessage(t, 1, fmt.Sprintf("question %d", i))})
	}
	replies, err := pollHistoryUpdates(t, b, updates, nil)
	requireHistoryPollStopped(t, err)
	state := readState(t, path)
	if len(replies) != 25 || state.Offset != 25 || len(state.Chats[1]) != 100 || state.Chats[1][0].Content != "question 0" {
		t.Fatalf("completed history was truncated: replies=%d offset=%d messages=%d first=%q", len(replies), state.Offset, len(state.Chats[1]), state.Chats[1][0].Content)
	}
	restarted := stateBot(t, "new prompt", turn)
	if err := restarted.UseState(path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readState(t, path).Chats, state.Chats) {
		t.Fatal("restart changed full history, reasoning or tool pairs")
	}
	_, err = pollHistoryUpdates(t, restarted, []update{{ID: 25, Message: privateMessage(t, 1, "after restart")}}, nil)
	requireHistoryPollStopped(t, err)
	if len(lastRequest) != 104 || !strings.HasPrefix(lastRequest[0].Content, "new prompt") || lastRequest[1].Content != "question 0" || lastRequest[101].Content != "after restart" {
		t.Fatal("model did not receive the full conversation after restart")
	}
	if len(readState(t, path).Chats[1]) != 104 {
		t.Fatal("continued full history was not saved")
	}
}

func TestTelegramContextOverflowPreservesHistory(t *testing.T) {
	for _, language := range []string{"ru", "en"} {
		t.Run(language, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telegram.json")
			failedCalls := 0
			turn := func(_ context.Context, req agent.Request) (*agent.Response, error) {
				last := req.Messages[len(req.Messages)-1]
				if last.Role == agent.RoleTool {
					failedCalls++
					return nil, fmt.Errorf("model turn: %w", agent.ErrContextLengthExceeded)
				}
				if last.Content == "too much" {
					failedCalls++
					return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "probe", Name: "load", Arguments: "{}"}}}, nil
				}
				if last.Content == "continue" && (len(req.Messages) != 4 || req.Messages[1].Content != "completed") {
					t.Error("failed turn replaced or polluted completed context")
				}
				return &agent.Response{Content: "answer"}, nil
			}
			b := stateBot(t, "prompt", turn)
			b.SetLanguage(func() string { return language })
			if err := b.UseState(path); err != nil {
				t.Fatal(err)
			}
			_, err := pollHistoryUpdates(t, b, []update{{ID: 0, Message: privateMessage(t, 1, "completed")}}, nil)
			requireHistoryPollStopped(t, err)
			before := readState(t, path)
			replies, err := pollHistoryUpdates(t, b, []update{{ID: 1, Message: privateMessage(t, 1, "too much")}}, nil)
			requireHistoryPollStopped(t, err)
			if len(replies) != 1 || !strings.Contains(replies[0].Text, "/clear") {
				t.Fatalf("missing overflow guidance: %+v", replies)
			}
			want := "Context"
			if language == "ru" {
				want = "Контекст"
			}
			if !strings.Contains(replies[0].Text, want) || !reflect.DeepEqual(readState(t, path).Chats, before.Chats) || readState(t, path).Offset != 2 || failedCalls != 2 {
				t.Fatal("overflow lost history, repeated tools or omitted localized explanation")
			}
			_, err = pollHistoryUpdates(t, b, []update{{ID: 2, Message: privateMessage(t, 1, "continue")}}, nil)
			requireHistoryPollStopped(t, err)
		})
	}
}

func TestTelegramOtherErrorsAndTruncationAreNotContextOverflow(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			b := stateBot(t, "prompt", func(context.Context, agent.Request) (*agent.Response, error) {
				if truncated {
					return &agent.Response{Content: "partial answer", FinishReason: "length"}, nil
				}
				return nil, errors.New("invalid tool schema")
			})
			replies, err := pollHistoryUpdates(t, b, []update{{ID: 0, Message: privateMessage(t, 1, "request")}}, nil)
			requireHistoryPollStopped(t, err)
			if len(replies) != 1 || strings.Contains(replies[0].Text, "/clear") || (truncated && replies[0].Text != "partial answer") {
				t.Fatalf("unrelated error/truncation was treated as context overflow: %+v", replies)
			}
		})
	}
}

func TestTelegramClearBacksUpAndResetsOnlyCurrentChat(t *testing.T) {
	for _, language := range []string{"ru", "en"} {
		t.Run(language, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telegram.json")
			turn := func(context.Context, agent.Request) (*agent.Response, error) {
				return &agent.Response{Content: "answer"}, nil
			}
			b := stateBot(t, "prompt", turn)
			b.SetLanguage(func() string { return language })
			if err := b.UseState(path); err != nil {
				t.Fatal(err)
			}
			_, err := pollHistoryUpdates(t, b, []update{
				{ID: 0, Message: privateMessage(t, 1, "old conversation")},
				{ID: 1, Message: privateMessage(t, 2, "other chat")},
			}, nil)
			requireHistoryPollStopped(t, err)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			replies, err := pollHistoryUpdates(t, b, []update{{ID: 2, Message: privateMessage(t, 1, "/clear")}}, func(sentMessage) {
				// The confirmation must follow both durable writes.
				backup, readErr := os.ReadFile(path + ".bak")
				if readErr != nil || string(backup) != string(before) {
					t.Error("confirmation preceded the backup of the previous state")
				}
				active := readState(t, path)
				if active.Offset != 3 || len(active.Chats[1]) != 0 || len(active.Chats[2]) != 2 {
					t.Error("confirmation preceded the cleared checkpoint or affected another chat")
				}
			})
			requireHistoryPollStopped(t, err)
			want := "manually"
			if language == "ru" {
				want = "вручную"
			}
			if len(replies) != 1 || !strings.Contains(replies[0].Text, want) || !strings.Contains(replies[0].Text, "Telegram") {
				t.Fatalf("missing localized manual-clear guidance: %+v", replies)
			}
			info, err := os.Stat(path + ".bak")
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("backup is missing or not owner-only")
			}
			freshCalls := 0
			restarted := stateBot(t, "prompt", func(_ context.Context, req agent.Request) (*agent.Response, error) {
				freshCalls++
				if len(req.Messages) != 2 || req.Messages[1].Content != "new conversation" {
					t.Error("cleared history was restored into the next request")
				}
				return &agent.Response{Content: "fresh answer"}, nil
			})
			if err := restarted.UseState(path); err != nil {
				t.Fatal(err)
			}
			_, err = pollHistoryUpdates(t, restarted, []update{{ID: 3, Message: privateMessage(t, 1, "new conversation")}}, nil)
			requireHistoryPollStopped(t, err)
			beforeSecondClear, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = pollHistoryUpdates(t, restarted, []update{{ID: 4, Message: privateMessage(t, 1, "/clear")}}, nil)
			requireHistoryPollStopped(t, err)
			backup, err := os.ReadFile(path + ".bak")
			if err != nil || string(backup) != string(beforeSecondClear) || freshCalls != 1 {
				t.Fatal("second clear did not replace the backup with the latest pre-clear state")
			}
			files, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(files) != 2 {
				t.Fatal("clear accumulated backups or leaked temporary files")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			withoutActive := stateBot(t, "prompt", turn)
			if err := withoutActive.UseState(path); err != nil {
				t.Fatal(err)
			}
			if len(readState(t, path).Chats) != 0 || readState(t, path).Offset != 0 {
				t.Fatal("missing active state automatically restored the backup")
			}
		})
	}
}

func TestTelegramClearStorageFailureDoesNotConfirmOrForget(t *testing.T) {
	for _, blocked := range []string{"backup", "active state"} {
		t.Run(blocked, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telegram.json")
			b := stateBot(t, "prompt", func(_ context.Context, req agent.Request) (*agent.Response, error) {
				if req.Messages[len(req.Messages)-1].Content == "continue" && (len(req.Messages) != 4 || req.Messages[1].Content != "keep this") {
					t.Error("failed clear discarded the in-memory history")
				}
				return &agent.Response{Content: "answer"}, nil
			})
			if err := b.UseState(path); err != nil {
				t.Fatal(err)
			}
			_, err := pollHistoryUpdates(t, b, []update{{ID: 0, Message: privateMessage(t, 1, "keep this")}}, nil)
			requireHistoryPollStopped(t, err)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			obstruction := path + ".bak"
			if blocked == "active state" {
				obstruction = path
				if err := os.Rename(path, path+".previous"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(obstruction, 0700); err != nil {
				t.Fatal(err)
			}
			replies, err := pollHistoryUpdates(t, b, []update{{ID: 1, Message: privateMessage(t, 1, "/clear")}}, nil)
			var stateErr *stateError
			if !errors.As(err, &stateErr) || len(replies) != 0 {
				t.Fatalf("storage failure was ignored or confirmed: replies=%v err=%v", replies, err)
			}
			checkpoint := path
			if blocked == "active state" {
				checkpoint = path + ".previous"
			}
			after, err := os.ReadFile(checkpoint)
			if err != nil || string(after) != string(before) {
				t.Fatal("failed clear changed the previous checkpoint")
			}
			leaks, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".telegram-*.tmp"))
			if err != nil || len(leaks) != 0 {
				t.Fatal("failed clear leaked conversation data in temporary files")
			}
			if err := os.Remove(obstruction); err != nil {
				t.Fatal(err)
			}
			if blocked == "active state" {
				if err := os.Rename(checkpoint, path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = pollHistoryUpdates(t, b, []update{{ID: 1, Message: privateMessage(t, 1, "continue")}}, nil)
			requireHistoryPollStopped(t, err)
		})
	}
}

func TestTelegramIgnoredClearDoesNotResetOrBackUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	b := stateBot(t, "prompt", func(context.Context, agent.Request) (*agent.Response, error) {
		return &agent.Response{Content: "answer"}, nil
	})
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	_, err := pollHistoryUpdates(t, b, []update{{ID: 0, Message: privateMessage(t, 1, "keep this")}}, nil)
	requireHistoryPollStopped(t, err)
	before := readState(t, path).Chats
	group := privateMessage(t, 1, "/clear")
	group.Chat.Type = "group"
	replies, err := pollHistoryUpdates(t, b, []update{
		{ID: 1, Message: privateMessage(t, 999, "/clear")},
		{ID: 2, Message: group},
	}, nil)
	requireHistoryPollStopped(t, err)
	if len(replies) != 0 || !reflect.DeepEqual(readState(t, path).Chats, before) {
		t.Fatal("ignored clear affected the saved conversation")
	}
	if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored clear created a backup: %v", err)
	}
}
