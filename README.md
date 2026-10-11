# mini-sre

![mini-sre — on-call robot mascot](assets/readme-cover.png)

A minimal SRE agent for DeepSeek (OpenAI-compatible API): a CLI chat with streaming,
history, and function calling.

## Features

- REPL and one-shot mode, the answer is printed as it is generated;
- read-only tools for load average, disk space, systemd/Docker status and logs;
- top processes by resident memory or lifetime-average CPU;
- bounded file reading and search, with hidden paths blocked;
- the same probes run locally or over SSH on hosts configured in YAML;
- a Telegram bot for personal messages and forwarded text from allowed users;
- persistent scheduled agent checks, incident notifications and periodic summaries;
- token and DeepSeek server cache stats after every answer;
- DeepSeek thinking mode (disabled by default).

## Requirements

- Go 1.25+;
- Linux with `/proc/loadavg` and GNU coreutils (`cat`, `df`);
- a DeepSeek API key.

Systemd probes need `systemctl` and `journalctl`; Docker probes need the Docker
CLI and access to its daemon. These commands must be available in the SSH user's
noninteractive PATH on remote hosts. Commands run as the current local or SSH
user; the agent does not invoke `sudo`.

The process probe requires `ps` from procps-ng, locally or in the SSH user's PATH.

File probes require GNU `sed` (with `--sandbox`), `grep`, `tail`, `readlink` and
`stat`, locally or in the SSH user's noninteractive PATH.

## Usage

```bash
export DEEPSEEK_API_KEY=sk-...

go run ./cmd "what is the load average and how much space is on /?"   # one-shot
go run ./cmd                                                          # interactive REPL
```

Build:

```bash
go build -o mini-sre ./cmd
```

## Flags

| Flag | Default | Description |
|---|---|---|
| `-model` | `deepseek-flash` | model ID (`deepseek-flash`, `deepseek-v4-pro`) |
| `-base-url` | `https://api.deepseek.com` | API base URL |
| `-temperature` | server default (1.0) | 0..2; ignored in thinking mode |
| `-thinking` | `false` | enable thinking mode |
| `-reasoning-effort` | server default (`high` when thinking is enabled) | `low`, `high`, `max`; used only in thinking mode |
| `-reasoning` | `false` | print `reasoning_content` to stderr |
| `-stream` | `true` | print the answer as it is generated |
| `-config` | empty | YAML file with SSH hosts and Telegram allowed user IDs; empty means local only |
| `-telegram` | `false` | run the bot instead of the console; requires `-config` |
| `-telegram-state` | XDG state directory or `~/.local/state/mini-sre/telegram.json` | JSON state file used only in Telegram mode |
| `-telegram-tasks` | `tasks.json` beside the Telegram state file | editable scheduled tasks and execution state, Telegram mode only |

The key is read from the `DEEPSEEK_API_KEY` environment variable, never from
arguments. On startup the CLI loads `.env` from the current directory if present;
real environment variables take precedence over the file. A malformed `.env`
aborts startup.

## Model settings in .env

These settings apply to CLI requests, Telegram conversations and scheduled checks:

| Environment variable | Default | Description |
|---|---|---|
| `DEEPSEEK_THINKING` | `false` | Enable thinking with `true` |
| `DEEPSEEK_REASONING_EFFORT` | server default (`high`) | `low`, `high` or `max`; sent only when thinking is enabled |
| `DEEPSEEK_TEMPERATURE` | server default (`1.0`) | Number from `0` to `2`; ignored by DeepSeek in thinking mode |

For Flash 4.1, use the model ID `deepseek-flash` (the application's default).
To enable maximum reasoning effort:

```dotenv
DEEPSEEK_THINKING=true
DEEPSEEK_REASONING_EFFORT=max
```

For more predictable answers without thinking:

```dotenv
DEEPSEEK_THINKING=false
DEEPSEEK_TEMPERATURE=0.2
```

Explicit flags override their corresponding environment variables, including
`-thinking=false`. `-temperature=-1` restores the server's default temperature
even when `DEEPSEEK_TEMPERATURE` is set. Real environment variables override
`.env`; unset or empty values use the defaults above. Invalid settings stop
startup. Restart the bot after changing these settings.

Temperature changes the randomness of token selection: lower values favor
likely continuations and more consistent wording; higher values produce more
varied answers. It does not set reasoning depth or guarantee factual accuracy,
and even `0` does not guarantee identical answers. For this diagnostic assistant,
`0.2` is a reasonable starting point without thinking. In thinking mode,
temperature has no effect; use reasoning effort to control deliberation instead.
Maximum effort can increase response time and token usage, so it is not a way
to make the bot respond faster. See DeepSeek's
[thinking mode documentation](https://api-docs.deepseek.com/guides/thinking_mode/)
and [temperature guide](https://api-docs.deepseek.com/quick_start/parameter_settings/).

## Hosts and SSH

Copy `hosts.example.yaml` to `hosts.yaml` and replace the example hosts and
credentials. The latter is ignored by Git. Keep the config and private keys
readable only by their owner (`chmod 600 hosts.yaml keys/sre_ed25519`). If you use
another config filename, keep that file outside Git too.

```bash
go run ./cmd -config hosts.yaml "how much free space is in /var on web-1?"
go run ./cmd -config hosts.yaml
```

`local` always means the machine running this process; it is available without
configuration and cannot be redefined. Other IDs come from the `hosts` mapping.
Each host needs `address`, `user` and exactly one of `key_file` or `password`.
The default port is 22. Passwords are literal YAML strings, not environment
references. Keys with a passphrase are not supported in this MVP.

Relative `key_file` and `known_hosts` paths are resolved against the YAML file's
directory. Omit `known_hosts` to use the current user's `~/.ssh/known_hosts`.
The file must contain the server's independently verified host key; unknown or
changed host keys are rejected. The agent does not add keys automatically.

With `-config`, the YAML is watched and reloaded after saving, including when an
editor atomically replaces the file. Hosts, `known_hosts` and Telegram allowed
user IDs are published together. Invalid YAML, a temporarily missing file, or an
empty Telegram allowlist in bot mode is logged without replacing the last valid
configuration. A fatal watcher error stops the process. Each probe uses one
configuration snapshot for its connection and password redaction; the next probe
uses the updated settings. Revoking Telegram access blocks subsequent requests
without deleting the user's history. `.env` and model settings remain startup-only.

Key files and `known_hosts` are read
when making a connection, so one unavailable remote host does not disable local
checks. YAML syntax, unknown fields, invalid ports and conflicting credentials
are rejected at startup. Host IDs are exposed to the model; addresses, passwords,
key paths and key contents are not included in the host listing or prompts.

SSH connection failures are logged to stderr with the host ID, connection stage,
and underlying error. Configured passwords are redacted. For the user service,
read these diagnostics with `journalctl --user -u mini-sre.service -n 50`.
Detailed errors may include local file paths and server addresses; they are not
sent to the model.

## Diagnostic tools

All probes except `list_hosts` accept `host_id`, defaulting to `local`.

| Tool | Other arguments | Behavior |
|---|---|---|
| `list_hosts` | none | list configured IDs and `local` |
| `get_load_average` | none | load for 1, 5, 15 minutes and process counts |
| `get_disk_usage` | `path`, default `/` | total, used, available bytes formatted as human-readable sizes |
| `get_top_processes` | `sort_by`, default `memory`; `limit`, default `10` | descending ranking by RSS or lifetime-average CPU, with PID, user, CPU%, MEM%, RSS in KiB and process name |
| `get_service_status` | required `service`; `backend`, default `systemd`; `scope`, default `system` | systemd unit state or Docker container `.State` |
| `get_service_logs` | `service`, `backend`, `scope`, `lines`, `since_minutes` | recent journal or container logs |
| `read_file` | required `path`; `start_line=1`, `lines=100`, `tail=false` | read a line range or the last lines of one file |
| `search_file` | required `path`, `pattern`; `limit=100`, `regex=false`, `ignore_case=false` | matching lines with line numbers from one file |

`backend` is `systemd` or `docker`, selected per request. Systemd names may omit
the `.service` suffix; Docker targets are container names or IDs, not Compose or
Swarm service names. An inactive/failed unit or exited container is a valid
status result. Missing services, unavailable commands/daemons and permission
errors are reported explicitly. For systemd logs, omit `service` to read the
general journal. Docker logs require a container.

`get_top_processes` accepts `sort_by=memory` or `sort_by=cpu` and `limit=1..50`.
For example, ask "which processes use the most memory on web-1?" or
"top 5 processes by CPU on local". Memory sorting uses RSS (resident physical
memory), not virtual address space. RSS includes shared pages, so summing it across processes can
double-count memory. CPU is averaged over each process's entire lifetime and can
exceed 100% on multiple cores; it does not measure a recent one-second interval.
Only processes visible to the local or SSH user are included. Full command-line
arguments and environment variables are not requested. The report returns at
most the selected number of complete rows, even if the lower-ranked `ps` output
exceeds the executor's capture limit.

For systemd, `scope` is `system` (default) or `user`. User scope selects the
service manager and journal of the local process user or the configured SSH
user. It requires that user's systemd manager to be available. To inspect the
agent installed with the example user unit, use
`{"host_id":"local","service":"mini-sre","scope":"user"}` with either service
tool. Omit `service` with `scope=user` to read that user's general journal.
Docker does not accept `scope`.

Logs default to the last 100 records from the last 60 minutes. `lines` must be
1..500 and `since_minutes` must be positive. Both stdout and stderr logs are
included. There is no follow mode. Journal access depends on the selected user's
permissions; a restricted journal can contain fewer records than the full
system journal.

File probes require an absolute path to one existing regular file. `read_file`
accepts `lines=1..500` and a positive `start_line` (numbered from 1); `tail=true`
reads the last lines and cannot be combined with an explicit `start_line`.
`search_file` accepts a nonempty pattern and `limit=1..500` matching lines.
Matching is literal and case sensitive by default; `regex=true` uses grep's
extended regular expressions and `ignore_case=true` ignores case. No matches
is a successful result; invalid regular expressions and command failures are
errors. Neither tool follows growing files or searches directories recursively.

Every component of the requested path and resolved symlink target is checked.
Names starting with a dot are forbidden, including `.env`, `.ssh`, `.aws`,
`.config`, `.` and `..`. A normal-looking symlink pointing into a hidden
directory is also rejected. Directories, devices, FIFOs and sockets are rejected
before reading. Symlinks to ordinary allowed files are supported.

For example, ask "read lines 20 through 40 of /etc/nginx/nginx.conf on web-1",
"show the last 50 lines of /var/log/nginx/error.log", or
"find lines matching ^listen in /etc/nginx/nginx.conf using a regular expression".

These checks prevent accidental reads of hidden paths; they are not a security
boundary against concurrent filesystem changes, hard-link aliases, or secrets
stored in ordinary config files. File contents are sent to the model and may be
saved in Telegram conversation history. Only configured SSH passwords are
automatically redacted. Limit access with local/SSH user permissions when needed.

Each command has a 15-second timeout, including SSH connection and handshake.
Captured stdout and stderr together are limited to 64 KiB, with an explicit
truncation marker. Local arguments are passed directly to the process; SSH
arguments are quoted for the remote shell. The model cannot supply a program or
arbitrary shell command. Cancelling SSH closes the connection, but does not
guarantee that the remote process has exited. Logs can contain application
secrets; review which hosts/users the agent may access before sending them to
the model.

## Telegram

Create a bot, set its token and put your numeric Telegram user ID in
`telegram.allowed_user_ids` in the YAML file (not your username). Incoming Bot
API messages carry this ID in `message.from.id`.

```bash
export TELEGRAM_BOT_TOKEN='your-bot-token'
go run ./cmd -config hosts.yaml -telegram
```

`DEEPSEEK_API_KEY` is also required. Both tokens may be stored in the ignored
`.env` file; use owner-only permissions for it. The bot uses outbound long
polling, so the nettop needs access to Telegram and DeepSeek, without a public
HTTP endpoint. A pre-existing webhook causes a clear startup error and is not
removed automatically. Run only one polling process per bot token.

To use an HTTP proxy only for Telegram, add this startup setting to `.env`:

```dotenv
TELEGRAM_HTTP_PROXY=http://127.0.0.1:8080
```

For a proxy that requires a username and password:

```dotenv
TELEGRAM_HTTP_PROXY=http://user:password@proxy.example:8080
```

Replace `user` and `password` with the proxy credentials. Percent-encode special
characters in credentials, for example `@` as `%40` and `#` as `%23`.
HTTP and HTTPS proxy URLs are supported. Restart the bot after changing it.
This setting takes precedence over `HTTPS_PROXY` and `NO_PROXY` for Telegram;
it does not change the model client's transport. If unset or empty, the bot
uses Go's standard `HTTPS_PROXY` / `NO_PROXY` environment settings. Global
proxy settings can also affect DeepSeek requests. Invalid Telegram proxy URLs
stop startup without printing the URL or its credentials.

The bot accepts text and forwarded text in personal chats from allowed users.
Other senders, groups and non-text messages are ignored before calling the model.
Use `/start` for help and `/clear` to reset the chat. Requests are processed
sequentially; histories are separate per chat and retain the last 20 completed
requests, including their tool calls, results and reasoning. Histories and the
polling offset are persisted in `$XDG_STATE_HOME/mini-sre/telegram.json`, or
`~/.local/state/mini-sre/telegram.json` when `XDG_STATE_HOME` is unset. Override
the file with `-telegram-state PATH`; dedicate one state file to one bot process.
The example user service uses this default without unit changes. CLI history
remains in memory.

Each processed update is checkpointed before sending its reply, using a synced
temporary file, atomic rename and directory sync. New state directories have
mode `0700` and the state file has mode `0600`; existing parent directories are
not chmodded. The file contains conversation and diagnostic data. On startup,
a missing file creates a fresh state; invalid JSON, an unsupported format,
incomplete tool-call histories or filesystem errors stop
the bot with an error in its journal. A write failure during polling also stops
the bot. `/clear` removes the saved context before confirming, but does not
delete visible Telegram messages. Restored chats use the current system prompt.

Offsets expire after 24 hours without a processed update, matching Telegram's
update retention; chat context is retained. Already lost context cannot be
reconstructed from the visible chat using the Bot API. An interrupted model
request is not checkpointed and may be redelivered. A crash after checkpointing
but before sending can leave a saved answer undelivered; the bot does not retry
that update after restart. This is not an exactly-once delivery guarantee.

Telegram receives final answers with fenced code blocks, inline code and
`**bold**` text converted to native Telegram entities. Command contents are
preserved literally. Other Markdown syntax remains plain text; incomplete
fenced blocks are kept literally. Replies are split into messages of at most
4000 UTF-16 units and 100 entities, preferably at line boundaries, retaining
formatting in each part. Streaming, reasoning output and token statistics remain console
features. Network polling failures retry with a delay capped at 30 seconds;
Telegram's longer `retry_after` is respected. Replies use at most three delivery
attempts; delivery failures are logged and do not rerun the model. A network
failure after Telegram accepted a reply can result in a duplicate reply.

For ordinary chat requests, the journal records each model call and diagnostic
tool's start and duration, followed by state checkpoint and reply delivery
durations. Entries include the update ID and failure status; timing entries
omit message text, tool arguments and results. Model durations include SDK
retries. These timings begin after the bot picks up an update and do not
measure its time waiting in Telegram's queue. Scheduled checks do not emit
these stage timings.

CLI and Telegram are independent processes sharing the same configuration and
probe code, not the same conversation history. Send SIGINT/SIGTERM to stop the
bot and cancel active requests.

### Scheduled checks

Create a schedule in a personal chat:

```text
/task add 5m 1h Check nginx on web-1; report a problem if the service is not active.
/task add 1h 6h Check load on web-1; report a problem if the 5-minute load average exceeds 4.
```

The first interval controls checks, the second controls reports. Intervals use
Go duration syntax (`5m`, `1h`, `24h`), must be at least one minute, and the report
interval must be at least the check interval. New and resumed tasks first run
after one check interval. This is an interval scheduler, not calendar cron.

The MVP allows **at most five tasks per bot**, shared across all allowed users,
including paused tasks. `/task list` shows your task IDs; `/task delete ID`
deletes a task and frees a slot. Pausing a task does not free a slot. The limit
is also checked when loading or reloading JSON, so manual edits cannot bypass
it. If an existing file contains more than five tasks, remove excess entries
manually before starting the bot. Checks still run sequentially; five slow
checks can exceed a one-minute interval.

Use `/task list`, `/task show ID`, `/task pause ID`, `/task resume ID` and
`/task delete ID`. Users can only manage their own tasks; `/clear` only clears
chat history. Repeated delivery of the same mutating Telegram update does not
repeat the command. Revoking the owner's allowlist access suspends scheduled
checks and deliveries without deleting the task.

Each check calls the model in a fresh session with the saved prompt and the
existing diagnostic tools. The agent chooses which tools to use. It does not
share ordinary chat history or remember previous scheduled conversations.
There is no HTTP probe yet: the current tools inspect hosts, disks, processes,
services and logs. Process CPU values remain lifetime averages, not recent
interval measurements. Provide explicit criteria in the task text when exact
thresholds matter; otherwise the model uses its judgement and reports make
that limitation clear.

Results distinguish healthy, problem and unknown. Missing diagnostic calls,
invalid model output and execution errors are counted as unknown, rather than
healthy checks or observed incidents. The first problem and subsequent
transitions into a problem or back to healthy trigger immediate messages;
unknown results do not count as recovery. Periodic reports show completed checks,
problem counts, unknown results, interrupted runs and skipped intervals. Report
counts are computed from saved results without another model call.

Checks run one at a time in a background worker with a two-minute timeout per
run; the ordinary chat remains available. A separate worker sends scheduled
messages using the existing Telegram retries. If many tasks or slow model calls
exceed available time, checks can be delayed and intervals skipped. The process
must be running to perform checks; systemd keeps it running, but there is no
external cron entry or service installation by the bot. Every check incurs model
API usage.

Tasks are stored separately from chat history in a versioned JSON file, by
default `tasks.json` beside the selected `-telegram-state` file. Use
`-telegram-tasks PATH` for a different location. Run one process per tasks file,
and choose separate paths for separate bots. The task file and new state
directories use the same `0600` / `0700` permissions and atomic synced writes
as chat state.

For manual edits, create a task through Telegram first, then edit `prompt`,
`every`, `report_every` or `enabled` in its JSON entry, or remove an entry. Keep
IDs and owners unchanged; `state` and `last_command` are maintained by the bot.
The scheduler watches the directory (including atomic file replacements),
debounces events for 200 ms, and rereads the file every minute as a fallback.
Its own writes do not reset schedules or counters. Existing runtime state is
preserved when task definitions change; results from a task changed or deleted
while a check was running are discarded.

An invalid file at startup stops the bot without overwriting it. An invalid or
missing file during operation is logged and suspends scheduler writes and new
checks until repaired; completed results wait in memory. Last valid schedules
are retained. Filesystem write failures and fatal watcher errors stop the bot.
Editors and the bot do not share a filesystem transaction: before saving the
bot rereads and reconciles changes, but simultaneous external writes can still
race. Pause the service for bulk edits requiring strict preservation.

After restart, an interrupted run is marked as interrupted and overdue tasks
are checked once using current data. Missed historical checks are not replayed.
State is saved before each model request and before incident delivery. Failed
summary delivery retains counts for the next report; failed incident delivery
is logged and the result remains in periodic statistics. Crashes and uncertain
network delivery can still cause duplicated notifications. This is not an
exactly-once delivery guarantee.

The Telegram avatar is available at [assets/telegram-avatar.png](assets/telegram-avatar.png).
The README cover shares the same mascot; generation prompts are recorded in
[assets/prompts.md](assets/prompts.md). Set the bot avatar manually in Telegram.

For continuous operation, `examples/mini-sre.service` is a **user** systemd unit
expecting the binary, `.env` and `hosts.yaml` in `~/mini-sre`. Adjust its paths if
you keep the project elsewhere. Build the binary using the command above, then:

```bash
mkdir -p ~/.config/systemd/user
install -m 644 examples/mini-sre.service ~/.config/systemd/user/mini-sre.service
systemctl --user daemon-reload
systemctl --user enable --now mini-sre.service
journalctl --user -u mini-sre.service
```

For a user service to run without a login session, enable lingering for that
account using `loginctl enable-linger USER` if your system permits it.

## REPL commands

| Command | Action |
|---|---|
| `/usage` | tokens and cache hit/miss of the last answer |
| `/clear` | clear history (the system prompt is kept) |
| `/exit`, `/quit` | quit |

## Development

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
  so the agent explicitly disables it, otherwise `-temperature` would be a no-op.
