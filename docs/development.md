# Development

[Back to README](../README.md)

Build checks, package layout and model-client notes.

## Checks

```bash
gofmt -l .
go build ./...
go vet ./...
go test -race ./...
```

Layout:

- `internal/agent` - DeepSeek client: Chat, ChatStream (iterator), tool calls, its own
  types, the SDK does not leak outside;
- `internal/cli` - REPL, one-shot, streaming console output;
- `internal/session` - shared history and agent loop (up to 5 tool call rounds, then a final answer without tools);
- `internal/tools` - probes and their JSON schemas for the model;
  `load.go`, `disk.go`, `service.go` contain the individual probes;
- `internal/config`, `internal/remote` - YAML hosts and local/SSH command execution;
- `internal/telegram` - Bot API polling, authorization and chat sessions;
- `cmd/main.go` - flags and wiring.

## Notes

- DeepSeek context caching is server-side and automatic, there is no API setting; only
  `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` are observable.
- Temperature has no effect with `-thinking`: the model enables thinking by default,
  so the agent explicitly disables thinking unless requested. See
  [model settings](configuration.md#model-settings-in-env).
