# MiniSRE

![MiniSRE — on-call robot mascot](assets/readme-cover.png)

An SRE assistant powered by DeepSeek (OpenAI-compatible API). Ask questions in
CLI chat or Telegram; the agent checks the local machine or configured SSH hosts
and explains what it found.

## What it can check

- Load, disk space and processes using the most memory or CPU.
- Current CPU, memory, swap and I/O activity with a short vmstat sample.
- Memory, swap, PSI pressure stalls and OOM/reclaim counters.
- Systemd services, failed units, Docker containers and their recent logs.
- Config files: selected lines, the end of a file or matching text.
- Network interfaces, routes, policy rules, neighbors, sockets and ping.
- DNS with dig/nslookup; HTTP with curl, including bounded downloads.
- Filter diagnostic output and save it in a private directory under `/tmp`.
- Create, inspect and clean up bounded FRR/Alpine containerlab labs on explicitly enabled hosts.

The CLI supports streaming answers and shows token/cache usage. Telegram keeps
chat history across restarts and supports scheduled checks with incident alerts.

## Quick start

### Install the Telegram bot

On Linux with systemd (`x86_64` or `aarch64`), run as your regular user from a
login/SSH session, **without sudo**:

```bash
curl -fsSL https://raw.githubusercontent.com/Paramoshka/MiniSRE/main/scripts/install.sh | bash
```

Requires Bash, curl, tar and sha256sum; Go is not needed. The installer downloads
the latest GitHub Release, verifies its SHA256 checksum, and asks for your
DeepSeek API key, Telegram bot token and numeric Telegram user ID through the
terminal. Tokens are hidden while typing. It installs the bot in `~/mini-sre`,
saves credentials and configuration with owner-only permissions, then enables
and starts `mini-sre.service` as a systemd **user** service. Only your Telegram
ID is allowed; SSH hosts can be added later to `~/mini-sre/hosts.yaml`.

To keep the bot running after logout and start it at boot, the installer enables
linger for your account using `sudo loginctl enable-linger "$(id -un)"` when
needed, and verifies `Linger=yes`. Sudo may ask for your password. To check:

```bash
loginctl show-user "$(id -un)" -p Linger
systemctl --user status mini-sre.service
~/mini-sre/mini-sre --version
journalctl --user -u mini-sre.service -f
```

Run the same installation command to update; existing `.env`, `hosts.yaml`,
history and tasks are preserved. To uninstall the service and binary:

```bash
curl -fsSL https://raw.githubusercontent.com/Paramoshka/MiniSRE/main/scripts/install.sh | bash -s -- --uninstall
```

Uninstall keeps configuration, credentials, history and tasks, and leaves linger
enabled for other user services. You can also download and inspect
[the installer](scripts/install.sh) before running it with `bash install.sh` or
`bash install.sh --uninstall`. To install without prompts, export
`DEEPSEEK_API_KEY`, `TELEGRAM_BOT_TOKEN` and `TELEGRAM_USER_ID` before running it.
These values are only used when creating missing configuration files. See
[Telegram setup](docs/telegram.md) for proxies and service troubleshooting.

### Prepare a containerlab host

On a Linux systemd host with Docker and containerlab already installed, run:

```bash
curl -fsSL https://raw.githubusercontent.com/Paramoshka/MiniSRE/main/scripts/install-lab-host.sh -o install-lab-host.sh
sudo bash install-lab-host.sh --ssh-user YOUR_SSH_USER --mgmt-subnet 172.31.250.0/24
```

Choose an unused private `/24`; the subnet above is an example. The script installs
the release helper and restricted sudo rule, builds Alpine/FRR diagnostic images
locally, and enables a system timer for TTL cleanup every minute. Omit the arguments
to enter settings interactively. Repeat the command to update; existing config,
sudoers and lab state are preserved. This requires a release containing the lab
helper. See [containerlab setup](docs/containerlab.md) for dependencies and enabling
the host in the bot.

### Run from source

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
