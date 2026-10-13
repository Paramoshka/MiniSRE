package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"mini-sre/internal/agent"
)

// Existing delivery tests examine completed replies, after their checkpoint.
func acceptProgress(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg sentMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Text != "⏳ Принял, разбираюсь…" {
		return false
	}
	io.WriteString(w, `{"ok":true,"result":{"message_id":99}}`)
	return true
}

func TestProgressBeforeModelAndFallback(t *testing.T) {
	for _, scenario := range []string{"success", "status failure", "edit failure", "model failure", "not modified"} {
		t.Run(scenario, func(t *testing.T) {
			var methods []string
			var texts []string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				var params struct {
					Text string `json:"text"`
					ID   int64  `json:"message_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
					t.Error(err)
				}
				methods = append(methods, method)
				texts = append(texts, params.Text)
				if method == "editMessageText" && params.ID != 42 {
					t.Error("wrong progress message")
				}
				if (len(methods) == 1 && scenario == "status failure") || (method == "editMessageText" && scenario == "edit failure") {
					io.WriteString(w, `{"ok":false,"error_code":400,"description":"message unavailable"}`)
					return
				}
				if method == "editMessageText" && scenario == "not modified" {
					io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
					return
				}
				io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			}))
			defer server.Close()
			b := stateBot(t, "prompt", func(context.Context, agent.Request) (*agent.Response, error) {
				calls++
				if len(methods) != 1 || texts[0] != "⏳ Принял, разбираюсь…" {
					t.Error("model started before progress")
				}
				if scenario == "model failure" {
					return nil, errors.New("model failed")
				}
				return &agent.Response{Content: "**answer**"}, nil
			})
			b.baseURL = server.URL
			if err := b.process(context.Background(), update{ID: 1, Message: privateMessage(t, 1, "check")}); err != nil {
				t.Fatal(err)
			}
			want := "sendMessage editMessageText"
			if scenario == "status failure" {
				want = "sendMessage sendMessage"
			}
			if scenario == "edit failure" {
				want += " sendMessage"
			}
			if strings.Join(methods, " ") != want || calls != 1 {
				t.Fatalf("methods=%v calls=%d", methods, calls)
			}
			if scenario == "model failure" && !strings.Contains(texts[1], "Could not complete") {
				t.Fatal("status left unfinished")
			}
		})
	}
}

func TestToolMarkupNeverDeliveredOrSaved(t *testing.T) {
	var texts []string
	var modelCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ToolChoice string `json:"tool_choice"`
			Messages   []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if modelCalls.Add(1) == 2 {
			if req.ToolChoice != "none" || len(req.Messages) != 3 || req.Messages[2].Role != "system" {
				t.Errorf("repair request did not disable tools or retain valid history: %+v", req)
			}
			for _, m := range req.Messages {
				if strings.Contains(m.Content, "<｜｜DSML") {
					t.Error("rejected model content reached repair request")
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"broken","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Читаю.\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"read_file\">","tool_calls":null}}]}`)
	}))
	defer model.Close()
	client, err := agent.New(agent.Config{APIKey: "fake-api-key", BaseURL: model.URL})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg sentMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
		}
		texts = append(texts, msg.Text)
		io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer server.Close()
	b := stateBot(t, "prompt", client.Chat)
	b.baseURL = server.URL
	b.statePath = t.TempDir() + "/state.json"
	if err := b.process(context.Background(), update{ID: 1, Message: privateMessage(t, 1, "check")}); err != nil {
		t.Fatal(err)
	}
	if len(texts) != 2 || !strings.Contains(texts[1], "Не удалось сформировать итог диагностики") || !strings.Contains(texts[1], "могли выполниться") {
		t.Fatalf("replies: %v", texts)
	}
	if modelCalls.Load() != 2 {
		t.Fatalf("model calls = %d, want one initial call and one repair", modelCalls.Load())
	}
	for _, text := range texts {
		if strings.Contains(text, "DSML") {
			t.Fatal("markup reached Telegram")
		}
	}
	if len(b.sessions[1].History) != 1 {
		t.Fatal("failed turn saved in session")
	}
	data, err := os.ReadFile(b.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("DSML")) {
		t.Fatal("markup saved to state")
	}
}
