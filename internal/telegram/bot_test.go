package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

func TestRequestTimingLogs(t *testing.T) {
	var logs bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()
	turns := 0
	bot, err := New("secret-token", func() []int64 { return []int64{1} }, func() *session.Session {
		return &session.Session{
			Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
				turns++
				if req.Messages[len(req.Messages)-1].Role == agent.RoleUser {
					return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "probe", Name: "get_load_average", Arguments: "private-arguments"}}}, nil
				}
				return &agent.Response{Content: "private-answer", FinishReason: "stop"}, nil
			},
			RunTool: func(context.Context, agent.ToolCall) (string, error) {
				return "private-result", nil
			},
		}
	}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	bot.baseURL = server.URL
	if err := bot.UseState(t.TempDir() + "/state.json"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		logs.Reset()
		if err := bot.process(context.Background(), update{ID: id, Message: privateMessage(t, 1, "private-input")}); err != nil {
			t.Fatal(err)
		}
		output := logs.String()
		if !strings.Contains(output, `round=2 disable_tool_calls=false finish_reason="stop" tool_calls=0`) || !strings.Contains(output, `tool_calls=1`) {
			t.Fatalf("model metadata missing: %s", output)
		}
		for _, stage := range []struct {
			name  string
			count int
		}{{"model", 2}, {"tool name=\"get_load_average\"", 1}, {"state", 1}, {"send", 1}} {
			prefix := fmt.Sprintf("telegram: update=%d stage=%s duration=", id, stage.name)
			if strings.Count(output, prefix) != stage.count {
				t.Fatalf("missing or duplicate stage timings: %s", output)
			}
		}
		if strings.Contains(output, "private-") || strings.Contains(output, "secret-token") || strings.Contains(output, "failed=true") {
			t.Fatalf("unexpected log contents: %s", output)
		}
	}
	if turns != 4 {
		t.Fatalf("model calls = %d, want 4", turns)
	}
}

func TestToolErrorLogs(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", `fake-api-secret"quoted`)
	for _, toolErr := range []error{nil, errors.New("command on Germay: permission denied\nfake-api-secret\"quoted secret-token")} {
		t.Run(fmt.Sprint(toolErr != nil), func(t *testing.T) {
			var logs bytes.Buffer
			bot, err := New("secret-token", func() []int64 { return []int64{1} }, func() *session.Session {
				return &session.Session{
					Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
						if req.Messages[len(req.Messages)-1].Role == agent.RoleUser {
							return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "probe", Name: "get_service_status", Arguments: "private-arguments"}}}, nil
						}
						result := req.Messages[len(req.Messages)-1].Content
						if toolErr != nil {
							if result != "error: "+toolErr.Error() {
								t.Fatal("tool error was not passed to the model")
							}
						} else if result != "private-result" {
							t.Fatal("tool result was changed")
						}
						return &agent.Response{Content: "private-answer"}, nil
					},
					RunTool: func(context.Context, agent.ToolCall) (string, error) {
						return "private-result", toolErr
					},
				}
			}, &logs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bot.handle(context.Background(), privateMessage(t, 1, "private-input"), 42, func() {}); err != nil {
				t.Fatal(err)
			}
			output := logs.String()
			if !strings.Contains(output, `telegram: update=42 stage=tool name="get_service_status" duration=`) {
				t.Fatalf("tool metadata missing: %s", output)
			}
			if toolErr != nil {
				if !strings.Contains(output, `failed=true error="command on Germay: permission denied\n[redacted] [redacted]"`) {
					t.Fatalf("escaped and redacted tool error missing: %s", output)
				}
			} else if !strings.Contains(output, "failed=false") || strings.Contains(output, " error=") {
				t.Fatalf("unexpected error for successful tool: %s", output)
			}
			if strings.Contains(output, "private-") || strings.Contains(output, "fake-api-secret") || strings.Contains(output, "secret-token") {
				t.Fatalf("private content leaked: %s", output)
			}
		})
	}
}

type sentMessage struct {
	ChatID    int64           `json:"chat_id"`
	Text      string          `json:"text"`
	Entities  []messageEntity `json:"entities"`
	ParseMode string          `json:"parse_mode"`
}

func privateMessage(t *testing.T, user int64, text string) *message {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"text": text, "from": map[string]any{"id": user}, "chat": map[string]any{"id": user, "type": "private"},
		"forward_origin": map[string]any{"type": "user", "sender_user": map[string]any{"id": 999}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestAccessAndChatHistory(t *testing.T) {
	var mu sync.Mutex
	var sent []sentMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		var msg sentMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sent = append(sent, msg)
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()
	var histories [][]agent.Message
	factory := func() *session.Session {
		return &session.Session{Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
			histories = append(histories, append([]agent.Message(nil), req.Messages...))
			return &agent.Response{Content: "answer"}, nil
		}}
	}
	bot, err := New("secret-token", func() []int64 { return []int64{1, 2} }, factory, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	bot.baseURL = server.URL
	group := privateMessage(t, 1, "group request")
	group.Chat.Type = "group"
	for _, m := range []*message{nil, privateMessage(t, 999, "unauthorized"), group, privateMessage(t, 1, " ")} {
		if err := bot.process(context.Background(), update{ID: bot.offset, Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	if len(histories) != 0 || len(bot.sessions) != 0 {
		t.Fatal("unauthorized request reached the model")
	}
	for _, m := range []*message{
		privateMessage(t, 1, "/start"), privateMessage(t, 1, "forwarded problem"),
		privateMessage(t, 1, "second question"), privateMessage(t, 2, "other chat"),
		privateMessage(t, 1, "/clear"), privateMessage(t, 1, "fresh question"),
	} {
		if err := bot.process(context.Background(), update{ID: bot.offset, Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	if len(histories) != 4 || len(histories[0]) != 2 || len(histories[1]) != 4 || len(histories[2]) != 2 || len(histories[3]) != 2 {
		t.Fatalf("history crossed chats or reset failed: %+v", histories)
	}
	if histories[0][1].Content != "forwarded problem" || bot.sessions[1].MaxHistoryTurns != 20 {
		t.Fatal("forwarded text or history limit incorrect")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 6 || sent[3].ChatID != 2 {
		t.Fatalf("wrong replies: %+v", sent)
	}
}

func TestPollingOffsetRetriesAndDuplicates(t *testing.T) {
	var mu sync.Mutex
	var offsets []int64
	polls, sent, modelCalls := 0, 0, 0
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
			polls++
			switch polls {
			case 1:
				w.WriteHeader(503)
				io.WriteString(w, `{"ok":false,"error_code":503,"description":"temporary"}`)
			case 2:
				io.WriteString(w, `{"ok":true,"result":[{"update_id":3,"message":{"text":"unauthorized","from":{"id":999},"chat":{"id":999,"type":"private"}}},{"update_id":2,"message":{"text":"check","from":{"id":1},"chat":{"id":1,"type":"private"}}},{"update_id":2,"message":{"text":"duplicate","from":{"id":1},"chat":{"id":1,"type":"private"}}}]}`)
			case 3:
				io.WriteString(w, `{"ok":true,"result":[{"update_id":2,"message":{"text":"old","from":{"id":1},"chat":{"id":1,"type":"private"}}},{"update_id":4,"message":{"text":"next","from":{"id":1},"chat":{"id":1,"type":"private"}}}]}`)
			default:
				w.WriteHeader(409)
				io.WriteString(w, `{"ok":false,"error_code":409,"description":"other poller"}`)
			}
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
			sent++
			io.WriteString(w, `{"ok":true,"result":{}}`)
		default:
			t.Errorf("unexpected API method: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot, err := New("secret-token", func() []int64 { return []int64{1} }, func() *session.Session {
		return &session.Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
			modelCalls++
			return &agent.Response{Content: "done"}, nil
		}}
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	bot.baseURL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = bot.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("polling conflict was not fatal: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(offsets) != "[0 0 4 5]" || sent != 2 || modelCalls != 2 || bot.offset != 5 {
		t.Fatalf("offsets=%v sent=%d modelCalls=%d final=%d", offsets, sent, modelCalls, bot.offset)
	}
}

func TestAccessReloadPreservesHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()
	users := []int64{1}
	calls := 0
	bot, err := New("token", func() []int64 { return users }, func() *session.Session {
		return &session.Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
			calls++
			return &agent.Response{Content: "answer"}, nil
		}}
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	bot.baseURL = server.URL
	for _, step := range []struct {
		users []int64
		user  int64
	}{{[]int64{1}, 1}, {[]int64{2}, 1}, {[]int64{2}, 2}, {[]int64{1, 2}, 1}} {
		users = step.users
		if err := bot.process(context.Background(), update{ID: bot.offset, Message: privateMessage(t, step.user, "question")}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 || len(bot.sessions[1].History) != 5 || len(bot.sessions[2].History) != 3 {
		t.Fatal("access reload failed or discarded chat history")
	}
}

func TestWebhookAndShutdown(t *testing.T) {
	for _, webhook := range []bool{true, false} {
		t.Run(fmt.Sprintf("webhook=%v", webhook), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if acceptProgress(t, w, r) {
					return
				}
				if strings.HasSuffix(r.URL.Path, "/getWebhookInfo") {
					url := ""
					if webhook {
						url = "https://example.test/hook"
					}
					fmt.Fprintf(w, `{"ok":true,"result":{"url":%q}}`, url)
					return
				}
				if webhook {
					t.Error("bot changed or polled a configured webhook")
				}
				io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}))
			defer server.Close()
			bot, err := New("secret-token", func() []int64 { return []int64{1} }, nil, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			bot.baseURL = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err = bot.Run(ctx)
			if webhook && (err == nil || !strings.Contains(err.Error(), "webhook")) {
				t.Fatalf("webhook not rejected: %v", err)
			}
			if !webhook && err != nil {
				t.Fatalf("shutdown failed: %v", err)
			}
		})
	}
}

func TestReplySplitting(t *testing.T) {
	text := strings.Repeat("Hello🙂", 1800)
	parts := splitText(text)
	if len(parts) < 2 || strings.Join(parts, "") != text {
		t.Fatal("split lost text")
	}
	for _, part := range parts {
		if !utf8.ValidString(part) || len(utf16.Encode([]rune(part))) > 4000 || part == "" {
			t.Fatal("split broke Unicode or size limit")
		}
	}
}

func TestSendFormattedReply(t *testing.T) {
	var mu sync.Mutex
	var sent []sentMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if acceptProgress(t, w, r) {
			return
		}
		var msg sentMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sent = append(sent, msg)
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()
	bot, err := New("test-token", func() []int64 { return []int64{1} }, func() *session.Session {
		return &session.Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
			return &agent.Response{Content: "**SSH**\n```bash\necho '<tag> & 🙂'\n```\n" + strings.Repeat("`df -h`\n", 800)}, nil
		}}
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	bot.baseURL = server.URL
	if err := bot.process(context.Background(), update{ID: bot.offset, Message: privateMessage(t, 1, "How can I check disk space?")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var text strings.Builder
	code, inline := 0, 0
	for _, msg := range sent {
		if msg.ChatID != 1 || msg.ParseMode != "" || len(msg.Entities) > 100 || len(utf16.Encode([]rune(msg.Text))) > 4000 {
			t.Fatalf("wrong recipient, parse mode or size: %+v", msg)
		}
		text.WriteString(msg.Text)
		for _, entity := range msg.Entities {
			content := entityText(t, messagePart{Text: msg.Text}, entity)
			switch entity.Type {
			case "pre":
				code++
				if content != "echo '<tag> & 🙂'\n" || entity.Language != "bash" {
					t.Fatal("sent code was changed or language lost")
				}
			case "code":
				inline++
				if content != "df -h" {
					t.Fatal("sent inline command was changed")
				}
			}
		}
	}
	if len(sent) < 2 || code != 1 || inline != 800 || text.String() != "SSH\necho '<tag> & 🙂'\n"+strings.Repeat("df -h\n", 800) {
		t.Fatal("actual sendMessage payload lost formatting or content")
	}
}

func TestLogRedactsRuntimeSecrets(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fake-api-secret")
	t.Setenv("TELEGRAM_BOT_TOKEN", "fake-env-token")
	t.Setenv("TELEGRAM_HTTP_PROXY", "http://user:fake-proxy-secret@proxy.test:8080")
	var logs bytes.Buffer
	b := &Bot{token: "fake-bot-token", errOut: &logs}
	b.log(errors.New("fake-api-secret fake-env-token fake-bot-token http://user:fake-proxy-secret@proxy.test:8080 fake-proxy-secret"))
	if strings.Contains(logs.String(), "fake-") || strings.Count(logs.String(), "[redacted]") != 5 {
		t.Fatalf("credentials leaked: %s", logs.String())
	}
}
