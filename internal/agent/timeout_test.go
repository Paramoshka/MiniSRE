package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestTimeoutConfig(t *testing.T) {
	for _, test := range []struct {
		name      string
		timeout   time.Duration
		want      time.Duration
		wantError bool
	}{
		{name: "default", want: 180 * time.Second},
		{name: "explicit", timeout: 90 * time.Second, want: 90 * time.Second},
		{name: "negative", timeout: -time.Second, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := New(Config{APIKey: "test-key", Timeout: test.timeout})
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %t", err, test.wantError)
			}
			if !test.wantError && client.cfg.Timeout != test.want {
				t.Fatalf("timeout = %s, want %s", client.cfg.Timeout, test.want)
			}
		})
	}
}

func TestChatTimeout(t *testing.T) {
	for _, test := range []struct {
		name           string
		flushHeaders   bool
		parentDeadline bool
	}{
		{name: "before headers"},
		{name: "during body", flushHeaders: true},
		{name: "parent deadline", parentDeadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			timeout := 100 * time.Millisecond
			if test.parentDeadline {
				timeout = 5 * time.Second
			}
			var cap capture
			client := newTestClient(t, Config{Timeout: timeout}, func(w http.ResponseWriter, r *http.Request) {
				if _, err := cap.record(r); err != nil {
					t.Error(err)
					return
				}
				if test.flushHeaders {
					w.Header().Set("Content-Type", "application/json")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					t.Error("request was not cancelled")
					io.WriteString(w, completionJSON)
				}
			})
			ctx := context.Background()
			if test.parentDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			_, err := client.Chat(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "ping"}}})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want deadline exceeded", err)
			}
			if got := len(cap.snapshot().requests); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
			if test.parentDeadline && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("parent context error = %v", ctx.Err())
			}
		})
	}
}

func TestChatCompletesWithinTimeout(t *testing.T) {
	client := newTestClient(t, Config{Timeout: 5 * time.Second}, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(20 * time.Millisecond):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, completionJSON)
		case <-r.Context().Done():
		}
	})
	resp, err := client.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "ping"}}})
	if err != nil || resp.Content != "pong" {
		t.Fatalf("response = %+v, error = %v", resp, err)
	}
}
