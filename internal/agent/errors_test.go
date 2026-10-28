package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestContextLengthError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		mode := "chat"
		if stream {
			mode = "stream"
		}
		for _, tt := range []struct {
			name    string
			status  int
			code    string
			message string
			want    bool
		}{
			{"structured code", 400, "context_length_exceeded", "Input is too long", true},
			{"provider message", 400, "invalid_request_error", "This model's maximum context length is 131072 tokens. However, you requested 140000 tokens.", true},
			{"unprocessable context", 422, "context_length_exceeded", "Input is too long", true},
			{"invalid request", 400, "invalid_request_error", "Invalid tool schema", false},
			{"output budget", 400, "invalid_request_error", "max_tokens exceeds the maximum output length", false},
			{"unrelated context", 400, "invalid_request_error", "Invalid context parameter", false},
			{"authentication", 401, "context_length_exceeded", "Invalid key", false},
		} {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				client := newTestClient(t, Config{MaxRetries: intPtr(0)}, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.status)
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
						"type": "invalid_request_error", "code": tt.code, "message": tt.message,
					}})
				})
				req := Request{Messages: []Message{{Role: RoleUser, Content: "question"}}}
				var err error
				if stream {
					var response *Stream
					response, err = client.ChatStream(context.Background(), req)
					if err == nil {
						for _, chunkErr := range response.Chunks() {
							if chunkErr != nil {
								err = chunkErr
							}
						}
					}
				} else {
					_, err = client.Chat(context.Background(), req)
				}
				if err == nil || errors.Is(err, ErrContextLengthExceeded) != tt.want {
					t.Fatalf("overflow=%t, want %t: %v", errors.Is(err, ErrContextLengthExceeded), tt.want, err)
				}
				var apiErr *openai.Error
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.status || apiErr.Message != tt.message {
					t.Fatalf("lost original provider error: %v", err)
				}
			})
		}
	}
}
