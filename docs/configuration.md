# Configuration

[Back to README](../README.md)

CLI flags, model settings, SSH credentials and configuration reloads.

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

Copy [hosts.example.yaml](../hosts.example.yaml) to `hosts.yaml` and replace the
example hosts and credentials. `hosts.yaml` is ignored by Git. Keep the config and private keys
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
