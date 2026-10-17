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

## CI and releases

[CI and release](../.github/workflows/ci.yml) runs these checks on pushes to
`main`, pull requests and version tags. It uses the Go version from `go.mod`.
After the checks pass, it builds Linux `amd64` and `arm64` binaries with CGO
disabled and uploads a `mini-sre-linux` workflow artifact (retained for 14 days).
Each archive contains `mini-sre`, the example user service and `hosts.example.yaml`.
`checksums.txt` contains SHA256 hashes for both archives.

To publish a release, push a stable version tag on the commit you want to ship:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Use a new `vMAJOR.MINOR.PATCH` tag for each release. Once checks and both builds
pass, the workflow verifies the artifacts, uploads them to a draft GitHub Release,
then publishes it as the latest release. If publication fails, inspect
the run and any draft release before retrying; the workflow does not overwrite
existing releases. GitHub Actions must be enabled for the repository.

Published assets are available without a Go installation or an Actions login:

```text
https://github.com/Paramoshka/mini-sre/releases/latest/download/mini-sre_linux_amd64.tar.gz
https://github.com/Paramoshka/mini-sre/releases/latest/download/mini-sre_linux_arm64.tar.gz
https://github.com/Paramoshka/mini-sre/releases/latest/download/checksums.txt
```

The `latest` URLs become available after the first successful release. The
archives use stable filenames so the installer can select one by architecture.

## Package layout

- `internal/agent` - DeepSeek client: Chat, ChatStream (iterator), tool calls, its own
  types, the SDK does not leak outside;
- `internal/cli` - REPL, one-shot, streaming console output;
- `internal/session` - shared history and agent loop (up to 5 tool call rounds, then a final answer without tools);
- `internal/tools` - probes and their JSON schemas for the model;
  `load.go`, `disk.go`, `service.go` contain the individual probes;
- `internal/config`, `internal/remote` - YAML hosts and local/SSH command execution;
- `internal/telegram` - Bot API polling, authorization and chat sessions;
- `cmd/main.go` - flags and wiring.
- `internal/lab`, `cmd/mini-sre-lab` - restricted host-side containerlab execution and durable TTL registry.

Lab unit tests simulate Docker/containerlab lifecycle and failures; they do not
create live lab resources. Follow [the test-host checklist](containerlab.md#verification-on-a-test-host)
to validate the installed containerlab version and image digests before enabling a shared host.

## Notes

- DeepSeek context caching is server-side and automatic, there is no API setting; only
  `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` are observable.
- Temperature has no effect with `-thinking`: the model enables thinking by default,
  so the agent explicitly disables thinking unless requested. See
  [model settings](configuration.md#model-settings-in-env).
