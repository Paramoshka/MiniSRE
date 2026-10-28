# Telegram bot

[Back to README](../README.md)

Set up personal chat access, proxies, persistent history and a systemd user service.

## Setup

Create a bot, set its token and put your numeric Telegram user ID in
`telegram.allowed_user_ids` in the YAML file (not your username). Incoming Bot
API messages carry this ID in `message.from.id`.

Set `language: ru` or `language: en` at the root of `hosts.yaml` to select the
language of model answers, scheduled check summaries and predefined error
messages. The default is Russian. Saving a valid change applies it to the next
request without clearing history; see [response language](configuration.md#hosts-and-ssh).

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
sequentially; histories are separate per chat and retain all completed requests
until `/clear`, including their tool calls, results and reasoning. There is no
automatic history trimming or summarization. Histories and the
polling offset are persisted in `$XDG_STATE_HOME/mini-sre/telegram.json`, or
`~/.local/state/mini-sre/telegram.json` when `XDG_STATE_HOME` is unset. Override
the file with `-telegram-state PATH`; dedicate one state file to one bot process.
The example user service uses this default without unit changes. CLI history
remains in memory. Conversation size, memory use, state-file size and model input
cost can grow until you clear the conversation. Context caching remains a
provider optimization, not a guarantee of unlimited context or cache retention.

Every model request sends the full retained context. If the provider rejects it
with a recognized context-length error, the bot preserves the previous history,
checkpoints the update offset and suggests `/clear`. It does not automatically
retry with a shortened history or repeat tools. Tools executed before the error
may already have taken effect; rolling back the failed turn does not undo them.
Other model errors keep their normal handling, and `finish_reason=length` alone
is not treated as context overflow: it can mean the generated answer was cut short.

## Persistence and restart behavior

Each processed update is checkpointed before sending its reply, using a synced
temporary file, atomic rename and directory sync. New state directories have
mode `0700` and the state file has mode `0600`; existing parent directories are
not chmodded. The file contains conversation and diagnostic data. On startup,
a missing file creates a fresh state; invalid JSON, an unsupported format,
incomplete tool-call histories or filesystem errors stop
the bot with an error in its journal. A write failure during polling also stops
the bot. Restored chats use the current system prompt.

`/clear` first saves the previous full state in a single `telegram.json.bak`
(or `PATH.bak` with `-telegram-state PATH`), replacing any previous backup. It then
clears only the current chat in memory and the active JSON, preserving other
chats and the polling offset. Both files are written using synced temporary
files and atomic replacement; the active checkpoint is not renamed away while
creating the backup. Backup or checkpoint failures stop the bot without sending
a success confirmation. The backup contains all chats' conversation and diagnostic
data and has mode `0600`; clearing context is not secure erasure. Scheduled tasks
remain unchanged in their separate file.

The confirmation recommends manually clearing the visible Telegram conversation
to avoid confusing old messages with the new context. The bot does not delete
Telegram messages or detect manual chat-history deletion. Message identifiers
are not a count of remaining messages. A backup is never restored automatically,
even if the active JSON is missing or invalid.

Offsets expire after 24 hours without a processed update, matching Telegram's
update retention; chat context is retained. Already lost context cannot be
reconstructed from the visible chat using the Bot API. An interrupted model
request is not checkpointed and may be redelivered. A crash after checkpointing
but before sending can leave a saved answer undelivered; the bot does not retry
that update after restart. This is not an exactly-once delivery guarantee.

## Reply formatting and delivery

Before an ordinary model request, the bot sends `⏳ Принял, разбираюсь…` and
then replaces it with the first part of the completed reply, after checkpointing.
Further parts are sent as separate messages. A model error replaces the status
with an error reply. Sending the status has a two-second timeout and no retries;
failure to send or edit it falls back to normal reply delivery without repeating
the model request. Commands, ignored updates and scheduled reports do not use
this status. It appears when sequential processing picks up the request, not
while the update waits in Telegram's queue. A crash/cancellation or failed
checkpoint can leave an unfinished status; message IDs are not persisted.

A model timeout replaces the status with a specific Russian error explaining
that earlier actions may have taken effect. The bot does not repeat the request
or tools; it checkpoints the update offset and rolls back the failed turn's
conversation history. History rollback does not undo deployed or destroyed labs.
Check the lab's current state before repeating a deployment request. The model
HTTP attempt timeout defaults to `180s` and can be set with `-model-timeout` or
`DEEPSEEK_TIMEOUT`; restart after changing it. Longer timeouts can delay subsequent
messages because chat updates are processed sequentially. Shutdown cancellation
continues to stop processing rather than sending a timeout reply.

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
durations. Entries include the update ID and failure status; failed tool timing
entries also include the error text with known credentials masked and newlines
escaped. Entries omit message text, tool arguments and successful results.
Model durations include SDK retries. These timings begin after the bot picks up
an update and do not measure its time waiting in Telegram's queue. Scheduled checks do not emit
these stage timings.

CLI and Telegram are independent processes sharing the same configuration and
probe code, not the same conversation history. Send SIGINT/SIGTERM to stop the
bot and cancel active requests.

Model timing logs include the request round, whether tool calls were disabled,
`finish_reason` and the number of structured tool calls. They do not include
message content or tool arguments/results. Known runtime credentials are masked
in Telegram logs as well as tool responses.

The system prompt directs broad incident reports through a quick load/CPU,
memory, disk-space and resource-pressure overview, then checks hypotheses based
on the observations. A missing unit or path does not establish service absence;
the agent can use `search_processes` to look beyond the top resource consumers.
These instructions guide the model, rather than enforce a fixed checklist.

After five tool rounds, the final request retains tool schemas, sets
`tool_choice=none` and adds a temporary instruction to summarize observations,
likely causes and check limitations in the user's language. This instruction is
not saved in conversation history.
The final instruction preserves required response formats, including the JSON
contract used by scheduled checks.
If the model returns raw DSML invocation markup as text in any round, or a
structured tool call after tools are disabled, the session makes one additional
model request for a plain-text final answer using only the last valid history.
Calls from rejected responses are never executed; previous tools are not rerun.
If repair also produces markup, tool calls or an empty answer, Telegram sends a
specific error explaining that earlier checks/actions may already have occurred.
The failed turn is rolled back without saving the rejected responses; existing
history is preserved. Model/network errors and cancellation retain their usual
handling. Markdown code examples and block quotations remain valid.
Use `/clear` after updating if an earlier reply saved service markup.
CLI streaming can print content before completed-response validation runs.

## Scheduled checks

Use `/task` to create recurring checks and reports. See
[Scheduled checks](scheduled-checks.md) for commands, limits and restart behavior.

The Telegram avatar is available at [assets/telegram-avatar.png](../assets/telegram-avatar.png).
The README cover shares the same mascot; generation prompts are recorded in
[assets/prompts.md](../assets/prompts.md). Set the bot avatar manually in Telegram.

## Run as a systemd user service

For an automated installation from a prebuilt release, use the
[README quick start](../README.md#install-the-telegram-bot). The installer sets
up the user service, verifies linger, and supports `--uninstall` while preserving
configuration and Telegram state. Re-running it updates the binary and restarts
the service. Restart after editing `.env`; `hosts.yaml` reloads automatically.
If startup fails, inspect `journalctl --user -u mini-sre.service -n 50`.

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

For a user service to run after logout and at boot, enable lingering and verify
it for the same account:

```bash
sudo loginctl enable-linger "$(id -un)"
loginctl show-user "$(id -un)" -p Linger
```

The expected output is `Linger=yes`. Run `systemctl --user` commands as the
regular account; do not install the user service with `sudo`.
