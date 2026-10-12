# mini-sre

![mini-sre — on-call robot mascot](assets/readme-cover.png)

An SRE assistant powered by DeepSeek (OpenAI-compatible API). Ask questions in
CLI chat or Telegram; the agent checks the local machine or configured SSH hosts
and explains what it found.

## What it can check

- Load, disk space and processes using the most memory or CPU.
- Systemd services, Docker containers and their recent logs.
- Config files: selected lines, the end of a file or matching text.
- Network interfaces, routes, policy rules, neighbors, sockets and ping.
- DNS with dig/nslookup; HTTP with curl, including bounded downloads.
- Filter diagnostic output and save it in a private directory under `/tmp`.
- Create, inspect and clean up bounded FRR/Alpine containerlab labs on explicitly enabled hosts.

The CLI supports streaming answers and shows token/cache usage. Telegram keeps
chat history across restarts and supports scheduled checks with incident alerts.

## Quick start

You need Go 1.25+, Linux and a DeepSeek API key. The agent uses installed command-line
utilities; see [command dependencies](docs/tools.md#command-dependencies) for each
probe. HTTP checks need curl 8.4 or newer.

```bash
export DEEPSEEK_API_KEY=sk-...

go run ./cmd "what is the load average and how much space is on /?"
go run ./cmd  # interactive chat
```

To build a binary:

```bash
go build -o mini-sre ./cmd
./mini-sre
```

The API key can also go in a local `.env` file. Real environment variables take
precedence. The default model is `deepseek-flash`, with thinking disabled.
[Configuration](docs/configuration.md) covers flags and model settings.

In interactive chat, `/usage` shows the last answer's token/cache statistics,
`/clear` resets history, and `/exit` or `/quit` exits.

## Check SSH hosts

Copy [hosts.example.yaml](hosts.example.yaml), edit the hosts and credentials,
and provide a verified server key in `known_hosts`:

```bash
cp hosts.example.yaml hosts.yaml
chmod 600 hosts.yaml
go run ./cmd -config hosts.yaml "show nginx errors on web-1"
```

`local` always means the machine running the agent. Other host IDs come from the
YAML file. Hosts support a private key or password; unknown or changed SSH host
keys are rejected. [SSH configuration](docs/configuration.md#hosts-and-ssh) explains
credentials, paths and live config reloads.

## Use Telegram

Create a bot and put your numeric Telegram user ID in
`telegram.allowed_user_ids` in `hosts.yaml`. Then:

```bash
export TELEGRAM_BOT_TOKEN='your-bot-token'
go run ./cmd -config hosts.yaml -telegram
```

The bot accepts personal text messages and forwarded text from allowed users.
`/clear` resets its chat history. To schedule a check:

```text
/task add 5m 1h Check nginx on web-1; report a problem if the service is not active.
```

This checks every five minutes and reports every hour, with immediate incident
notifications. See [Telegram setup](docs/telegram.md) for proxy and systemd options,
and [scheduled checks](docs/scheduled-checks.md) for task commands and limits.

## Access and limits

Diagnostic commands run as the local or SSH user, without `sudo`. Optional lab
operations use a separately installed, restricted `mini-sre-lab` helper through
`sudo`, with host-side TTL cleanup. See [containerlab setup](docs/containerlab.md).
The agent has a fixed toolset; it cannot execute arbitrary shell commands. Downloads and
saved output write under `/tmp`, with a 10 MiB limit per file. Diagnostic commands
have a 15-second timeout and bounded output.

Logs and config files can contain secrets and are sent to the model. Configured
SSH passwords and known runtime credentials (Telegram, DeepSeek and proxies) are
automatically redacted; saved files contain raw data. Give the
agent access only to hosts and files you want it to inspect. Tool results are
observations; successful ping or HTTP transport alone does not prove service health.

## Documentation

- [Configuration](docs/configuration.md) — flags, `.env`, SSH and live reloads.
- [Diagnostic tools](docs/tools.md) — arguments, defaults, dependencies and pipelines.
- [Telegram](docs/telegram.md) — setup, proxy, history, delivery and systemd.
- [Scheduled checks](docs/scheduled-checks.md) — tasks, notifications and persistence.
- [Containerlab](docs/containerlab.md) — helper installation, lab descriptions, limits and TTL.
- [Development](docs/development.md) — checks and package layout.

## Development

```bash
gofmt -l .
go build ./...
go vet ./...
go test -race ./...
```
