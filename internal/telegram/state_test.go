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
	"sync/atomic"
	"testing"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

func stateBot(t *testing.T, prompt string, turn func(context.Context, agent.Request) (*agent.Response, error)) *Bot {
	t.Helper()
	b, err := New("test-token", func() []int64 { return []int64{1, 2} }, func() *session.Session {
		return &session.Session{
			SystemPrompt: prompt, Turn: turn,
			RunTool: func(_ context.Context, call agent.ToolCall) (string, error) { return "result of " + call.Name, nil },
		}
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readState(t *testing.T, path string) savedState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state savedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestStateRestoresBoundedToolHistoriesAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "telegram.json")
	var nextOffset atomic.Int64
	var clear atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		// Verify the checkpoint from Telegram's side, before accepting the reply.
		data, err := os.ReadFile(path)
		var state savedState
		if err == nil {
			err = json.Unmarshal(data, &state)
		}
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if state.Offset != nextOffset.Load() || (clear.Load() && len(state.Chats[1]) != 0) {
			t.Errorf("reply preceded its durable checkpoint: offset=%d, want=%d", state.Offset, nextOffset.Load())
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()
	turn := func(_ context.Context, req agent.Request) (*agent.Response, error) {
		if req.Messages[len(req.Messages)-1].Role == agent.RoleUser {
			return &agent.Response{ReasoningContent: "reasoning", ToolCalls: []agent.ToolCall{
				{ID: "load", Name: "load", Arguments: "{}"}, {ID: "disk", Name: "disk", Arguments: "{}"},
			}}, nil
		}
		return &agent.Response{Content: "answer"}, nil
	}
	b := stateBot(t, "old prompt", turn)
	b.baseURL = server.URL
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	process := func(b *Bot, user int64, text string) {
		t.Helper()
		offset := nextOffset.Add(1)
		if err := b.process(context.Background(), update{ID: offset - 1, Message: privateMessage(t, user, text)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 25 {
		process(b, 1, fmt.Sprintf("question %d", i))
	}
	process(b, 2, "other chat")
	state := readState(t, path)
	if len(state.Chats[1]) != 100 || state.Chats[1][0].Content != "question 5" || len(state.Chats[2]) != 5 {
		t.Fatal("did not retain 20 complete tool turns separately per chat")
	}
	for _, history := range state.Chats {
		for _, m := range history {
			if m.Role == agent.RoleSystem {
				t.Fatal("persisted stale system prompt")
			}
		}
	}
	for file, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("wrong state permissions for %s: %v", file, err)
		}
	}
	restarted := stateBot(t, "new prompt", turn)
	restarted.baseURL = server.URL
	if err := restarted.UseState(path); err != nil {
		t.Fatal(err)
	}
	if restarted.offset != nextOffset.Load() || restarted.sessions[1].History[0].Content != "new prompt" ||
		!reflect.DeepEqual(restarted.sessions[1].History[1:], state.Chats[1]) {
		t.Fatal("restart lost messages, reasoning, tool pairs or offset")
	}
	process(restarted, 1, "after restart")
	state = readState(t, path)
	if state.Chats[1][0].Content != "question 6" || state.Chats[1][95].Content != "after restart" {
		t.Fatal("continued history did not trim whole turns")
	}
	clear.Store(true)
	process(restarted, 1, "/clear")
	afterClear := stateBot(t, "current prompt", turn)
	if err := afterClear.UseState(path); err != nil {
		t.Fatal(err)
	}
	if afterClear.sessions[1] != nil || len(afterClear.sessions[2].History) != 6 || afterClear.offset != nextOffset.Load() {
		t.Fatal("clear was not durable or crossed chat boundaries")
	}
}

func TestInvalidStateIsFatalAndNotOverwritten(t *testing.T) {
	tests := []string{
		`{"private-secret":`, `{}`, `null`,
		`{"version":2,"chats":{}}`, `{"version":1,"chats":null}`,
		`{"version":1,"offset":-1,"chats":{}}`, `{"version":1,"offset":1,"chats":{}}`,
		`{"version":1,"chats":{"-1":[]}}`,
		`{"version":1,"chats":{"1":[{"Role":"user","Content":"private-secret"}]}}`,
		`{"version":1,"chats":{"1":[{"Role":"system","Content":"private-secret"}]}}`,
	}
	for i, data := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telegram.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			b := stateBot(t, "prompt", nil)
			var stateErr *stateError
			if err := b.UseState(path); !errors.As(err, &stateErr) || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("invalid state was accepted or leaked content: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != data {
				t.Fatal("invalid state was overwritten")
			}
		})
	}
	b := stateBot(t, "prompt", nil)
	if err := b.UseState(t.TempDir()); err == nil {
		t.Fatal("state read error ignored")
	}
}

func TestPollingWriteFailureIsFatalBeforeReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	b := stateBot(t, "prompt", func(context.Context, agent.Request) (*agent.Response, error) {
		return &agent.Response{Content: "answer"}, nil
	})
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			io.WriteString(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			io.WriteString(w, `{"ok":true,"result":[{"update_id":8,"message":{"text":"question","from":{"id":1},"chat":{"id":1,"type":"private"}}}]}`)
		default:
			t.Error("sent a reply despite checkpoint failure")
			io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	defer server.Close()
	b.baseURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var stateErr *stateError
	if err := b.Run(ctx); !errors.As(err, &stateErr) || b.offset != 0 {
		t.Fatalf("write failure was not fatal or acknowledged update: %v offset=%d", err, b.offset)
	}
	if state := readState(t, path+".previous"); len(state.Chats) != 0 || state.Offset != 0 {
		t.Fatal("failed write changed previous checkpoint")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".telegram-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatal("failed write leaked temporary files")
	}
}

func TestPollingRestoresOffsetAndDoesNotRepeatModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	var mu sync.Mutex
	var offsets []int64
	sent, modelCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			io.WriteString(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var params struct {
				Offset int64 `json:"offset"`
			}
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				t.Error(err)
			}
			offsets = append(offsets, params.Offset)
			if len(offsets)%2 == 0 {
				w.WriteHeader(409)
				io.WriteString(w, `{"ok":false,"error_code":409,"description":"stop test"}`)
				return
			}
			if params.Offset == 0 {
				io.WriteString(w, `{"ok":true,"result":[{"update_id":40,"message":{"text":"first","from":{"id":1},"chat":{"id":1,"type":"private"}}}]}`)
			} else {
				io.WriteString(w, `{"ok":true,"result":[{"update_id":40,"message":{"text":"duplicate","from":{"id":1},"chat":{"id":1,"type":"private"}}},{"update_id":41,"message":{"text":"second","from":{"id":1},"chat":{"id":1,"type":"private"}}}]}`)
			}
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
			sent++
			io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	defer server.Close()
	turn := func(_ context.Context, req agent.Request) (*agent.Response, error) {
		modelCalls++
		if modelCalls == 2 && (len(req.Messages) != 4 || req.Messages[1].Content != "first") {
			t.Error("next model request did not use restored context")
		}
		return &agent.Response{Content: "answer"}, nil
	}
	for range 2 {
		b := stateBot(t, "prompt", turn)
		b.baseURL = server.URL
		if err := b.UseState(path); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := b.Run(ctx)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "409") {
			t.Fatalf("polling: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(offsets) != "[0 41 41 42]" || modelCalls != 2 || sent != 2 {
		t.Fatalf("repeated an update across restart: offsets=%v model=%d sent=%d", offsets, modelCalls, sent)
	}
}

func TestModelFailureAndCancellationKeepCompletedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()
	turn := func(context.Context, agent.Request) (*agent.Response, error) {
		return &agent.Response{Content: "answer"}, nil
	}
	b := stateBot(t, "prompt", turn)
	b.baseURL = server.URL
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	if err := b.process(context.Background(), update{ID: 1, Message: privateMessage(t, 1, "completed")}); err != nil {
		t.Fatal(err)
	}
	completed := readState(t, path).Chats
	b.sessions[1].Turn = func(context.Context, agent.Request) (*agent.Response, error) {
		return nil, errors.New("model unavailable")
	}
	if err := b.process(context.Background(), update{ID: 2, Message: privateMessage(t, 1, "failed")}); err != nil {
		t.Fatal(err)
	}
	state := readState(t, path)
	if state.Offset != 3 || !reflect.DeepEqual(completed, state.Chats) {
		t.Fatal("model failure corrupted completed context")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.sessions[1].Turn = func(context.Context, agent.Request) (*agent.Response, error) { cancel(); return nil, ctx.Err() }
	if err := b.process(ctx, update{ID: 3, Message: privateMessage(t, 1, "cancelled")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) || b.offset != 3 {
		t.Fatal("cancelled request was checkpointed")
	}
}

func TestStateOffsetExpiresWithoutDiscardingContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	state := savedState{
		Version: 1, Offset: 999, OffsetSavedAt: time.Now().Add(-25 * time.Hour),
		Chats: map[int64][]agent.Message{1: {{Role: agent.RoleUser, Content: "old question"}, {Role: agent.RoleAssistant, Content: "old answer"}}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	b := stateBot(t, "prompt", nil)
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	if b.offset != 0 || len(b.sessions[1].History) != 3 || readState(t, path).Offset != 0 {
		t.Fatal("expired offset retained or history discarded")
	}
}

func TestDefaultStatePathAndUnwritableStartup(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	path, err := DefaultStatePath()
	if err != nil || path != filepath.Join(base, "mini-sre", "telegram.json") {
		t.Fatalf("XDG state path: %q, %v", path, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", base)
	path, err = DefaultStatePath()
	if err != nil || path != filepath.Join(base, ".local", "state", "mini-sre", "telegram.json") {
		t.Fatalf("fallback state path: %q, %v", path, err)
	}
	blocked := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	b := stateBot(t, "prompt", nil)
	if err := b.UseState(filepath.Join(blocked, "telegram.json")); err == nil {
		t.Fatal("unwritable startup state accepted")
	}
}
