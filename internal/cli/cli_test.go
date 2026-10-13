package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

const streamBody = `data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{"reasoning_content":"think "},"finish_reason":null}]}

data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}

data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3}}

data: [DONE]

`

const completionBody = `{
  "id": "chat-2",
  "object": "chat.completion",
  "created": 1,
  "model": "deepseek-flash",
  "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "pong"}}],
  "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4, "prompt_cache_hit_tokens": 0, "prompt_cache_miss_tokens": 3}
}`

const toolCallStream = `data: {"id":"chat-t","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_load_average","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"chat-t","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

type messageJSON struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type recorder struct {
	mu       sync.Mutex
	requests [][]messageJSON
}

func (r *recorder) add(messages []messageJSON) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, messages)
}

func (r *recorder) snapshot() [][]messageJSON {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]messageJSON(nil), r.requests...)
}

func testClient(t *testing.T, rec *recorder, body string, contentType string) *agent.AgentClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []messageJSON `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.add(req.Messages)
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	client, err := agent.New(agent.Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return client
}

func TestAskStreamsAnswer(t *testing.T) {
	var rec recorder
	client := testClient(t, &rec, streamBody, "text/event-stream")

	var out, errOut bytes.Buffer
	app := &App{Client: client, Out: &out, Err: &errOut, Stream: true, Reasoning: true}

	if err := app.Ask(context.Background(), "hi"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if out.String() != "hello world\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "hello world\n")
	}
	if !strings.Contains(errOut.String(), "think ") {
		t.Errorf("stderr = %q, want reasoning text", errOut.String())
	}
	if !strings.Contains(errOut.String(), "cache hit=7 miss=3") {
		t.Errorf("stderr = %q, want cache stats", errOut.String())
	}

	if len(app.session.History) != 3 {
		t.Fatalf("history = %d messages, want 3", len(app.session.History))
	}
	if app.session.History[0].Role != agent.RoleSystem || app.session.History[0].Content != DefaultSystemPrompt {
		t.Errorf("history[0] = %+v, want system %q", app.session.History[0], DefaultSystemPrompt)
	}
	if app.session.History[1].Role != agent.RoleUser || app.session.History[1].Content != "hi" {
		t.Errorf("history[1] = %+v, want user/hi", app.session.History[1])
	}
	if app.session.History[2].Role != agent.RoleAssistant || app.session.History[2].Content != "hello world" {
		t.Errorf("history[2] = %+v, want assistant/hello world", app.session.History[2])
	}

	reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if len(reqs[0]) != 2 || reqs[0][0].Role != "system" || reqs[0][1].Content != "hi" {
		t.Errorf("request messages = %+v, want system + hi", reqs[0])
	}
}

func TestAskNonStreaming(t *testing.T) {
	var rec recorder
	client := testClient(t, &rec, completionBody, "application/json")

	var out, errOut bytes.Buffer
	app := &App{Client: client, Out: &out, Err: &errOut, Stream: false}

	if err := app.Ask(context.Background(), "ping"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if out.String() != "pong\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "pong\n")
	}
}

func TestAskRollsBackHistoryOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	client, err := agent.New(agent.Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	var out, errOut bytes.Buffer
	app := &App{Client: client, Out: &out, Err: &errOut, Stream: true}

	if err := app.Ask(context.Background(), "hi"); err == nil {
		t.Fatal("Ask() = nil error, want error")
	}
	if len(app.session.History) != 1 || app.session.History[0].Role != agent.RoleSystem {
		t.Errorf("history = %+v, want only system message", app.session.History)
	}
}

func TestAskRunsTools(t *testing.T) {
	var mu sync.Mutex
	var requests [][]messageJSON
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []messageJSON `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, req.Messages)
		requestCount++
		n := requestCount
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, toolCallStream)
			return
		}
		io.WriteString(w, streamBody)
	}))
	t.Cleanup(server.Close)

	client, err := agent.New(agent.Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	var out, errOut bytes.Buffer
	var calls []agent.ToolCall
	app := &App{
		Client: client,
		Out:    &out,
		Err:    &errOut,
		Stream: true,
		Tools:  []agent.Tool{{Name: "get_load_average", Description: "load"}},
		RunTool: func(_ context.Context, call agent.ToolCall) (string, error) {
			calls = append(calls, call)
			return "load average: 0.10 0.05 0.01 (running 1/450)", nil
		},
	}

	if err := app.Ask(context.Background(), "load?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if len(calls) != 1 || calls[0].Name != "get_load_average" || calls[0].Arguments != "{}" {
		t.Errorf("tool calls = %+v, want single get_load_average", calls)
	}
	if !strings.Contains(errOut.String(), "→ get_load_average({})") {
		t.Errorf("stderr = %q, want tool call line", errOut.String())
	}
	if !strings.Contains(out.String(), "hello world") {
		t.Errorf("stdout = %q, want final answer", out.String())
	}

	if len(app.session.History) != 5 {
		t.Fatalf("history = %d messages, want 5", len(app.session.History))
	}
	if app.session.History[2].Role != agent.RoleAssistant || len(app.session.History[2].ToolCalls) != 1 {
		t.Errorf("history[2] = %+v, want assistant with tool call", app.session.History[2])
	}
	if app.session.History[3].Role != agent.RoleTool || app.session.History[3].ToolCallID != "call_1" {
		t.Errorf("history[3] = %+v, want tool result for call_1", app.session.History[3])
	}
	if app.session.History[4].Role != agent.RoleAssistant || app.session.History[4].Content != "hello world" {
		t.Errorf("history[4] = %+v, want final assistant answer", app.session.History[4])
	}

	mu.Lock()
	defer mu.Unlock()
	if requestCount != 2 {
		t.Fatalf("requests = %d, want 2", requestCount)
	}
	second := requests[1]
	if len(second) != 4 {
		t.Fatalf("second request messages = %d, want 4", len(second))
	}
	if len(second[2].ToolCalls) != 1 || second[2].ToolCalls[0].ID != "call_1" {
		t.Errorf("second request assistant = %+v, want tool call", second[2])
	}
	if second[3].Role != "tool" || second[3].ToolCallID != "call_1" || second[3].Content == "" {
		t.Errorf("second request tool message = %+v, want result", second[3])
	}
}

func TestAskToolRoundsExceeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, toolCallStream)
	}))
	t.Cleanup(server.Close)

	client, err := agent.New(agent.Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	var out, errOut bytes.Buffer
	app := &App{
		Client: client,
		Out:    &out,
		Err:    &errOut,
		Stream: true,
		RunTool: func(_ context.Context, call agent.ToolCall) (string, error) {
			return "ok", nil
		},
	}

	err = app.Ask(context.Background(), "loop")
	if !errors.Is(err, session.ErrResponseFormat) {
		t.Fatalf("Ask error = %v, want response format error", err)
	}
	if len(app.session.History) != 1 || app.session.History[0].Role != agent.RoleSystem {
		t.Errorf("history = %+v, want rolled back to system message", app.session.History)
	}
}

func TestRunCommands(t *testing.T) {
	var rec recorder
	client := testClient(t, &rec, streamBody, "text/event-stream")

	var out, errOut bytes.Buffer
	app := &App{
		Client: client,
		In:     strings.NewReader("hi\n/usage\n/exit\n"),
		Out:    &out,
		Err:    &errOut,
		Stream: true,
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Count(out.String(), prompt); got != 3 {
		t.Errorf("prompts = %d, want 3", got)
	}
	if !strings.Contains(out.String(), "hello world") {
		t.Errorf("stdout = %q, want answer", out.String())
	}
	if !strings.Contains(out.String(), "tokens: prompt=10 completion=5 total=15, cache: hit=7 miss=3") {
		t.Errorf("stdout = %q, want /usage output", out.String())
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestRunClear(t *testing.T) {
	var rec recorder
	client := testClient(t, &rec, streamBody, "text/event-stream")

	var out, errOut bytes.Buffer
	app := &App{
		Client: client,
		In:     strings.NewReader("hi\n/clear\n/exit\n"),
		Out:    &out,
		Err:    &errOut,
		Stream: true,
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(app.session.History) != 1 || app.session.History[0].Role != agent.RoleSystem {
		t.Errorf("history = %+v, want only system message after /clear", app.session.History)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var rec recorder
	client := testClient(t, &rec, streamBody, "text/event-stream")

	var out, errOut bytes.Buffer
	app := &App{
		Client: client,
		In:     strings.NewReader("/nope\n/exit\n"),
		Out:    &out,
		Err:    &errOut,
		Stream: true,
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(errOut.String(), "unknown command: /nope") {
		t.Errorf("stderr = %q, want unknown command error", errOut.String())
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("requests = %d, want 0", got)
	}
}

type blockedInput struct {
	io.ReadCloser
	started  chan struct{}
	finished chan struct{}
}

func (r *blockedInput) Read(p []byte) (int, error) {
	close(r.started)
	defer close(r.finished)
	return r.ReadCloser.Read(p)
}

func TestRunCancelsBlockedInput(t *testing.T) {
	reader, writer := io.Pipe()
	input := &blockedInput{ReadCloser: reader, started: make(chan struct{}), finished: make(chan struct{})}
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := &App{In: input, Out: io.Discard, Err: io.Discard}
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	select {
	case <-input.started:
	case <-time.After(time.Second):
		t.Fatal("Run did not start reading input")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run stayed blocked on input after cancellation")
	}
	select {
	case <-input.finished:
	case <-time.After(time.Second):
		t.Fatal("input read was not released after cancellation")
	}
}

func TestRunReportsInputError(t *testing.T) {
	input, writer := io.Pipe()
	defer input.Close()
	want := errors.New("input failed")
	if err := writer.CloseWithError(want); err != nil {
		t.Fatal(err)
	}
	app := &App{In: input, Out: io.Discard, Err: io.Discard}
	if err := app.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
}
