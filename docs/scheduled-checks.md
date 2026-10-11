# Scheduled checks

[Back to README](../README.md)

Recurring diagnostics in the [Telegram bot](telegram.md), with incident notifications and periodic reports.

## Create and manage tasks

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
Scheduled checks use the same [diagnostic tools](tools.md) as chat, including
HTTP, DNS and network probes. Process CPU values remain lifetime averages, not recent
interval measurements. Provide explicit criteria in the task text when exact
thresholds matter; otherwise the model uses its judgement and reports make
that limitation clear.

## Results and notifications

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

## Storage and manual edits

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

## Restart and delivery guarantees

After restart, an interrupted run is marked as interrupted and overdue tasks
are checked once using current data. Missed historical checks are not replayed.
State is saved before each model request and before incident delivery. Failed
summary delivery retains counts for the next report; failed incident delivery
is logged and the result remains in periodic statistics. Crashes and uncertain
network delivery can still cause duplicated notifications. This is not an
exactly-once delivery guarantee.
