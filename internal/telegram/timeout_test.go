package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"mini-sre/internal/agent"
)

func TestModelTimeoutAfterToolRounds(t *testing.T) {
	for _, modelError := range []error{
		fmt.Errorf("agent: chat: %w", context.DeadlineExceeded),
		fmt.Errorf("agent: chat: %w", &net.DNSError{IsTimeout: true}),
	} {
		t.Run(modelError.Error(), func(t *testing.T) {
			var methods, texts []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var msg sentMessage
				if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
					t.Error(err)
				}
				methods = append(methods, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
				texts = append(texts, msg.Text)
				io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			}))
			defer server.Close()
			turns, executions := 0, 0
			b := stateBot(t, "prompt", func(context.Context, agent.Request) (*agent.Response, error) {
				turns++
				if turns <= 2 {
					return &agent.Response{ToolCalls: []agent.ToolCall{{ID: fmt.Sprint(turns), Name: "lab_inspect"}}}, nil
				}
				return nil, modelError
			})
			b.baseURL = server.URL
			if err := b.UseState(t.TempDir() + "/state.json"); err != nil {
				t.Fatal(err)
			}
			s := b.newSession()
			s.Language = "ru"
			previous := []agent.Message{{Role: agent.RoleUser, Content: "previous"}, {Role: agent.RoleAssistant, Content: "answer"}}
			if err := s.Restore(previous); err != nil {
				t.Fatal(err)
			}
			before := append([]agent.Message(nil), s.History...)
			s.RunTool = func(context.Context, agent.ToolCall) (string, error) {
				executions++
				return "checked", nil
			}
			b.sessions[1] = s
			if err := b.process(context.Background(), update{ID: 10, Message: privateMessage(t, 1, "check")}); err != nil {
				t.Fatal(err)
			}
			if turns != 3 || executions != 2 {
				t.Fatalf("turns = %d, tools = %d", turns, executions)
			}
			if strings.Join(methods, " ") != "sendMessage editMessageText" || len(texts) != 2 ||
				!strings.Contains(texts[1], "Модель не ответила за отведённое время") ||
				!strings.Contains(texts[1], "Действия, выполненные до ошибки, могли сохраниться") {
				t.Fatalf("methods = %v, replies = %v", methods, texts)
			}
			if !reflect.DeepEqual(s.History, before) {
				t.Fatal("previous history changed")
			}
			state := readState(t, b.statePath)
			if b.offset != 11 || state.Offset != 11 || !reflect.DeepEqual(state.Chats[1], previous) {
				t.Fatalf("wrong checkpoint: %+v", state)
			}
		})
	}
}

func TestParentCancellationTakesPriorityOverModelTimeout(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			defer cancel()
			b := stateBot(t, "prompt", func(context.Context, agent.Request) (*agent.Response, error) {
				if deadline {
					<-ctx.Done()
				} else {
					cancel()
				}
				return nil, fmt.Errorf("agent: chat: %w", context.DeadlineExceeded)
			})
			reply, err := b.handle(ctx, privateMessage(t, 1, "check"), 1, func() {})
			text := reply.text
			if text != "" || !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
				t.Fatalf("text = %q, error = %v, context error = %v", text, err, ctx.Err())
			}
		})
	}
}
