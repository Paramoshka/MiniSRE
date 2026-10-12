package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
