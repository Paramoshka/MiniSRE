# Telegram bot

[Back to README](../README.md)

Set up personal chat access, proxies, persistent history and a systemd user service.

## Setup

Create a bot, set its token and put your numeric Telegram user ID in
`telegram.allowed_user_ids` in the YAML file (not your username). Incoming Bot
API messages carry this ID in `message.from.id`.

```bash
export TELEGRAM_BOT_TOKEN='your-bot-token'
go run ./cmd -config hosts.yaml -telegram
```

`DEEPSEEK_API_KEY` is also required. Both tokens may be stored in the ignored
`.env` file; use owner-only permissions for it. The bot uses outbound long
polling, so the host running the bot needs access to Telegram and DeepSeek, without a public
HTTP endpoint. A pre-existing webhook causes a clear startup error and is not
removed automatically. Run only one polling process per bot token.

## Proxy

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

## Chat commands and history

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

## Persistence and restart behavior

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

## Reply formatting and delivery

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

## Journal diagnostics

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

## Scheduled checks

Use `/task` to create recurring checks and reports. See
[Scheduled checks](scheduled-checks.md) for commands, limits and restart behavior.

The Telegram avatar is available at [assets/telegram-avatar.png](../assets/telegram-avatar.png).
The README cover shares the same mascot; generation prompts are recorded in
[assets/prompts.md](../assets/prompts.md). Set the bot avatar manually in Telegram.

## Run as a systemd user service

For continuous operation, [examples/mini-sre.service](../examples/mini-sre.service)
is a **user** systemd unit expecting the binary, `.env` and `hosts.yaml` in `~/mini-sre`. Adjust its paths if
you keep the project elsewhere. Build the binary from the repository root,
then install the unit:

```bash
go build -o mini-sre ./cmd
mkdir -p ~/.config/systemd/user
install -m 644 examples/mini-sre.service ~/.config/systemd/user/mini-sre.service
systemctl --user daemon-reload
systemctl --user enable --now mini-sre.service
journalctl --user -u mini-sre.service
```

For a user service to run without a login session, enable lingering for that
account using `loginctl enable-linger USER` if your system permits it.
