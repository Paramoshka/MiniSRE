# Diagnostic tools

[Back to README](../README.md)

Arguments, defaults, command dependencies and execution limits. You can ask the
agent in natural language; the tool names below describe the API it uses.

- [Command dependencies](#command-dependencies)
- [Tool reference](#tool-reference)
- [Services and logs](#services-and-logs), [processes](#processes), [memory and pressure](#memory-and-pressure), [files](#files)
- [Network](#network-diagnostics), [DNS](#dns), [HTTP](#http-checks-and-downloads)
- [Pipelines and saved output](#pipelines-and-output-files)
- [Execution limits and data access](#execution-limits-and-data-access)

## Command dependencies

Commands are needed on the host where a probe runs, locally or over SSH.

Base probes require Linux with `/proc/loadavg` and GNU `cat` and `df`.

Systemd probes need `systemctl` and `journalctl`; Docker probes need the Docker
CLI and access to its daemon. These commands must be available in the SSH user's
noninteractive PATH on remote hosts. Commands run as the current local or SSH
user; the agent does not invoke `sudo`.

Process probes require `ps` and `pgrep` from procps-ng, locally or in the SSH user's PATH.
The `get_vmstat` probe requires procps-ng `vmstat` with `-w`, `-n`, `-y` and
`-S K` support on the selected host. The agent does not install missing commands.

File probes require GNU `sed` (with `--sandbox`), `grep`, `tail`, `readlink` and
`stat`, locally or in the SSH user's noninteractive PATH.

Network probes require `ip` and `ss` from iproute2, and `ping` from iputils.
They must also be available in the SSH user's noninteractive PATH. ICMP socket
permissions depend on the host's ping configuration; failures are reported
without attempting to elevate privileges.

DNS probes require BIND `dig` and `nslookup`, locally or in the SSH user's
noninteractive PATH.

HTTP probes require curl >= 8.4, including on remote hosts. Pipelines and HTTP
probes also require Bash and GNU coreutils (`timeout`, `mktemp`, `mkdir`, `stat`,
`head`, `cat`, `rm`, `rmdir`); filters use GNU `grep`, `tail` and sandboxed `sed`.
These commands must be in the executing user's noninteractive PATH. No PTY or
privilege escalation is used.

## Tool reference

Diagnostic probes except `list_hosts` accept `host_id`, defaulting to `local`.
The optional [containerlab tools](containerlab.md) require an explicit enabled
`host_id` and use a separately installed host helper.

| Tool | Other arguments | Behavior |
|---|---|---|
| `list_hosts` | none | list configured IDs and `local` |
| `get_load_average` | none | load for 1, 5, 15 minutes and process counts |
| `get_vmstat` | none | three samples at one-second intervals: CPU, runnable/blocked processes, memory, swap and block I/O; excludes the since-boot report |
| `get_memory` | none | memory, swap, PSI stall averages and OOM/reclaim counters from `/proc`; unavailable sources are reported as limitations |
| `get_disk_usage` | `path`, default `/` | total, used, available bytes formatted as human-readable sizes |
| `get_top_processes` | `sort_by`, default `memory`; `limit`, default `10` | descending ranking by RSS or lifetime-average CPU, with PID, user, CPU%, MEM%, RSS in KiB and process name |
| `search_processes` | required `pattern`; `limit=100` | case-insensitive literal name search across all visible processes, returning PID and name |
| `get_service_status` | required `service`; `backend`, default `systemd`; `scope`, default `system` | systemd unit state or Docker container `.State` |
| `get_service_logs` | `service`, `backend`, `scope`, `lines`, `since_minutes` | recent journal or container logs |
| `list_failed_services` | `scope`, default `system` | failed systemd units via `systemctl --failed`; empty output means none in that scope |
| `list_containers` | `running_only=false`, `limit=100` | Docker containers with ID, name, image, state, status and ports; includes stopped containers by default |
| `read_file` | required `path`; `start_line=1`, `lines=100`, `tail=false` | read a line range or the last lines of one file |
| `search_file` | required `path`, `pattern`; `limit=100`, `regex=false`, `ignore_case=false` | matching lines with line numbers from one file |
| `get_ip_rules` | `family=ipv4`, optional `table` | policy routing rules, optionally filtered by referenced table |
| `get_ip_routes` | `family=ipv4`, `table=main`, optional `interface` | routes from one table or `all` tables |
| `get_route_to` | required IP `destination`; optional `source`, `mark`, `input_interface`, `output_interface` | the kernel's route lookup for the supplied packet attributes |
| `ping_host` | required `destination`; `count=3`, `family=all`, optional `interface` | bounded ICMP echo probe to one IP address or DNS hostname |
| `get_network_sockets` | `protocol=both`, `state=listening`, `family=all`, optional `port` | TCP/UDP sockets with numeric endpoints and visible process details |
| `get_network_interfaces` | `family=all`, optional `interface` | IP addresses, link state, MTU and RX/TX counters |
| `get_ip_neighbors` | `family=ipv4`, optional `interface` | ARP or NDP neighbor cache entries and states |
| `dig_dns` | required `query`; optional `record_type`, `server`, `port` | bounded DNS query with full dig output and DNS status |
| `nslookup_dns` | required `query`; optional `record_type`, `server`, `port` | bounded noninteractive DNS query with nslookup |
| `curl_http` | required `url`; `method=GET`, optional `headers`, `follow_redirects=false`, `insecure=false`, `download=false` | HTTP/HTTPS response status, headers, server IP, timings and body preview, or saved raw body |
| `run_pipeline` | required `source`; optional `filters`, `stdout=inline`, `stderr=inline` | source output through validated filters; return, save or discard streams, retaining all stage statuses |
| `lab_deploy` | required `host_id`, `topo_yaml`; `ttl_minutes=60` | create/configure one restricted FRR/Alpine lab with host-side TTL |
| `lab_inspect` | required `host_id`; optional `name` | inspect existing labs, nodes, management IPs and registered TTL |
| `lab_run` | required `host_id`, `name`, `node`, `checks` | bounded node diagnostics; explicit per-check statuses |
| `lab_destroy` | required `host_id`; `name` or `all_mine=true` | delete only registered helper-owned labs |

## Services and logs

`list_failed_services` runs `systemctl --failed` and returns every failed unit,
not only services: sockets, mounts and other unit types are included. `scope`
defaults to `system`; `scope=user` reads the executing user's manager. An empty
result means no failed units in that scope, not that the host has no problems.
The command does not require root and reports permission or manager errors
explicitly.

`list_containers` runs `docker ps` with a fixed format and returns ID, name,
image, state, status and ports. Stopped containers are included by default; set
`running_only=true` to hide them. `limit` accepts 1..500 rows and defaults to 100.
The Docker CLI and daemon access must be available to the executing user. An empty
list is a diagnostic result; a daemon or permission failure is an error. Neither
discovery tool proves that a unit or container is healthy.

`backend` is `systemd` or `docker`, selected per request. Systemd names may omit
the `.service` suffix; Docker targets are container names or IDs, not Compose or
Swarm service names. An inactive/failed unit or exited container is a valid
status result. Missing services, unavailable commands/daemons and permission
errors are reported explicitly. For systemd logs, omit `service` to read the
general journal. Docker logs require a container.

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

## Processes

`get_vmstat` runs `vmstat -w -n -y -S K 1 3`, taking approximately three seconds.
It works locally and over SSH, and can also be used as a pipeline source. The
standalone tool preserves the table with a sampling/units explanation; pipelines
use the raw table, as for other sources. Memory and swap columns use KiB; CPU
columns are percentages. Interpret column names because procps-ng versions can
expose additional columns. Used swap (`swpd`) does not alone imply active swapping
(`si`/`so`), and I/O wait (`wa`) does not alone prove a disk fault. A short healthy
sample cannot rule out an earlier spike. Missing commands, failed/incomplete or
truncated standalone samples are reported as errors, not healthy results.

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

Use `search_processes` to find a process independently of its CPU or memory
ranking. `pattern` is a literal, case-insensitive name fragment of 1..128 bytes,
without NUL or newlines; `limit` is 1..500 matching processes (default 100).
The search runs before the result limit. No matches is a successful diagnostic
result, not an execution error. Names may be limited to 15 characters, so use a
short fragment such as `fail2ban`. Command lines and environments are not searched.
Output states this scope and marks omitted matches when the result is limited.
A match proves process existence, not service health or its relationship to a
panel. Filtering `get_top_processes` cannot establish that a process is absent.
`search_processes` is also a pipeline source; its match limit applies before
downstream filters, and exit code 1 means no matches.

## Memory and pressure

`get_memory` reads `/proc/meminfo`, `/proc/vmstat` and the PSI files
`/proc/pressure/cpu`, `/proc/pressure/memory` and `/proc/pressure/io`. It reports
total, free and available memory (with the share of `MemTotal`), swap use,
dirty/writeback pages, slab and commit accounting, PSI `some`/`full` averages over
10, 60 and 300 seconds, and cumulative OOM/reclaim counters.

`MemAvailable` is the kernel's estimate of memory available for new workloads; on
kernels without it, the report uses `MemFree+Buffers+Cached` and marks the value as
a fallback estimate. Used swap does not by itself mean active swapping. PSI shows
stalled-time percentages, not free memory or I/O throughput. `oom_kill` and other
counters are cumulative since boot, so they can reveal an earlier incident even
when current readings look healthy. Missing PSI files or counters are reported as
limitations; a failed `/proc/meminfo` read is an error. These readings do not prove
application health.

Example requests: "check memory and PSI on web-1", "was anything OOM-killed on
local?", and "show swap and writeback pressure on storage-1".

## Files

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
Sensitive `/proc` components `environ`, `cmdline`, `mem`, `fd` and `fdinfo`
are forbidden, including process aliases and task paths. This applies to local
and SSH file tools and file sources in pipelines. Both the requested and resolved
paths are checked. Ordinary diagnostics such as `/proc/meminfo` and
`/proc/loadavg` remain available.

For example, ask "read lines 20 through 40 of /etc/nginx/nginx.conf on web-1",
"show the last 50 lines of /var/log/nginx/error.log", or
"find lines matching ^listen in /etc/nginx/nginx.conf using a regular expression".

These checks prevent accidental reads of hidden paths; they are not a security
boundary against concurrent filesystem changes, hard-link aliases, or secrets
stored in ordinary config files. File contents are sent to the model and may be
saved in Telegram conversation history. Configured SSH passwords, the executor's
`TELEGRAM_BOT_TOKEN` and `DEEPSEEK_API_KEY`, and configured proxy URLs/passwords
are automatically redacted
from tool responses. Proxy masking covers `TELEGRAM_HTTP_PROXY`, `HTTP_PROXY`,
`HTTPS_PROXY`, `ALL_PROXY` and the lowercase standard proxy variables.
Other application secrets and credentials known only to a remote host are not
covered. Limit access with local/SSH user permissions when needed.

## Network diagnostics

Network probes run in the executing process's network namespace, locally or on
the SSH host. They do not change routes, rules, interfaces or socket state.
`ping_host` sends echo requests; the other network probes read diagnostic state.
There is no namespace switching or arbitrary command/filter argument support.

Rules, route tables and neighbors accept `family=ipv4` (default) or `ipv6`.
Interfaces, sockets and ping additionally accept `all` (default): interfaces and
sockets include both families, while ping chooses automatically. Table names
include `main`, `local`, `default` and aliases configured on the target host;
numeric IDs must be positive decimal 32-bit values without leading zeros.
`get_ip_routes` also accepts `table=all`. `get_ip_rules` with `table` shows rules
referencing that table, not its routes. The table named `default` is distinct
from a default route in `main`; missing tables and failed lookups remain errors.

`get_route_to` accepts literal IP addresses, not DNS names or CIDR prefixes, and
chooses IPv4/IPv6 from the destination. The optional source must use the same
family. `mark` is an unsigned 32-bit JSON integer (including zero), representing
the packet's fwmark. Input and output interfaces model incoming traffic or
constrain the output device; the kernel may reject invalid combinations.
IPv6 addresses with a zone suffix are not accepted: use `output_interface` for
route lookup or `interface` for ping to select a link-local scope.

Ping sends `count=1..5` packets, defaults to 3, uses a two-second reply timeout
and an eight-second deadline within the executor's 15-second command timeout.
No flood or follow mode is exposed. Packet loss (ping exit code 1) is a diagnostic
result with an explicit failure note; command errors remain errors. Missing
ICMP replies can mean filtering, and successful ping does not prove application
health. Route lookup also does not prove that packets reach the destination.

Socket queries accept `protocol=tcp`, `udp` or `both`. States are `listening`
(default), `all`, `established`, `syn-sent`, `syn-recv`, `time-wait` or `close-wait`;
connection states require `protocol=tcp`. `port=1..65535` matches either endpoint.
Process details depend on user permissions. Interface counters are cumulative,
not rates, and an empty neighbor cache alone is not proof of a network failure.

Example requests: "show routes from table 100 and the rules selecting it on
web-1", "check the route to 192.0.2.5 from 192.0.2.1 with fwmark 42",
"ping 192.0.2.5 three times", "show TCP close-wait sockets on port 443", and
"check eth0 addresses, errors and neighbors".

## DNS

`dig_dns` and `nslookup_dns` accept one ASCII DNS name (including SRV underscores
and a trailing dot) or an IP address without a zone. Names default to `record_type=A`;
IP addresses perform reverse lookups and require `PTR` (the default for IPs).
Allowed types are `A`, `AAAA`, `CNAME`, `MX`, `NS`, `TXT`, `SOA`, `SRV`, `PTR` and
`CAA`. Optional `server` must be a literal IPv4/IPv6 address; otherwise the host's
system DNS servers are used. `port` defaults to 53 and accepts 1..65535.

Both tools send DNS requests without changing host configuration. They disable
search suffixes and use a two-second query timeout; dig uses one try and ignores
`.digrc`, while nslookup uses `retry=1`. The executor's timeout still bounds the
whole command. No interactive mode, zone transfer or arbitrary flags are exposed.
Full output is retained: dig can return `NXDOMAIN` or `SERVFAIL` with exit code 0;
nslookup's nonzero exits are reported as errors with their diagnostic output.
A resolved name alone does not prove application health.

Example requests: "dig AAAA example.com on web-1 using DNS server 192.0.2.53",
"nslookup MX example.com", and "reverse DNS lookup of 192.0.2.5".

## HTTP checks and downloads

`curl_http` accepts one absolute HTTP/HTTPS URL without embedded credentials.
`method` is `GET` (default) or `HEAD`; request bodies and uploads are not exposed.
`headers` is a name/value object, including `Host` or `Accept`. Header names are
validated; CR/LF/NUL in values and reading headers from files are forbidden.
Requests retain the host's normal proxy and CA environment settings. `.curlrc`
and URL globbing are disabled, and only HTTP/HTTPS protocols are allowed.

Defaults are a three-second connection timeout, ten-second transfer timeout,
verified TLS, no redirects and no retries. `follow_redirects=true` follows at
most five HTTP/HTTPS redirects. Custom headers follow curl's normal redirect
behavior; trusted-location authentication forwarding is not enabled.
`insecure=true` disables certificate verification and is only for explicit user
requests; the result records that it was disabled. HTTP 4xx/5xx retain their
status and body as diagnostic responses. DNS, connection, TLS, timeout and size
errors retain the execution diagnostic instead of being reported as successful
checks. Response metadata and headers precede the body preview.

`download=true` requires GET and a URL ending in a non-hidden filename, at most
255 bytes. The body is saved as `/tmp/mini-sre-XXXXXXXXXX/data/<filename>` on the
selected host. The name comes from the original URL without its query string;
percent escapes stay encoded, and `Content-Disposition` is ignored. The result
reports the host, path and byte count. Repeated downloads use separate directories.
The transfer is capped at 10 MiB even without `Content-Length`; curl >= 8.4 is
checked before the request. Failed downloads can retain a bounded partial file,
marked `possibly_partial=true` in the error report.

## Pipelines and output files

`run_pipeline` chooses a source by its existing tool name and arguments. Sources
are all command-based diagnostic tools, including curl; `list_hosts`, `get_memory`,
nested pipelines and all `lab_*` tools are excluded. Source arguments keep their original
validation and defaults. `host_id` belongs to the pipeline, and applies to every stage. Source
streams contain raw command output rather than standalone human-readable
formatting; top processes still respects its requested row limit. Interfaces
emit address output followed by link counters.

Up to four stdin filters are allowed: `grep` (literal by default, optional
`regex`/`ignore_case`, `limit=100`), `head`/`tail` (`lines=100`), or `sed` as a line
range (`start_line=1`, `lines=100`). Match and line limits accept 1..500. No
arbitrary programs, argv, shell expressions, sed scripts or filter file paths
are exposed. Filters run before the final preview is limited, so a match beyond
the first 64 KiB can still be found. Limiting filters drain their remaining input
to avoid a misleading upstream SIGPIPE error.

`stdout` supports `inline`, `file` or `discard`; `stderr` additionally supports
`merge`, joining each stage's stderr into its stdout before the next filter.
Defaults are separate inline streams. Discarding both streams is the equivalent
of hiding data with `>/dev/null 2>&1`; execution status remains visible. Every
stage's exit code is returned. Grep exit code 1 means an empty result, and ping
exit code 1 retains its packet-loss meaning; other unexpected codes are errors.
Execution status does not establish service or application health.

Example: nginx logs containing errors, last 50 matches, saved on `web`:

```json
{
  "host_id": "web",
  "source": {"tool": "get_service_logs", "args": {"service": "nginx", "lines": 500}},
  "filters": [
    {"type": "grep", "pattern": "error", "ignore_case": true},
    {"type": "tail", "lines": 50}
  ],
  "stdout": "file",
  "stderr": "merge"
}
```

Without filters, any allowed source can be saved or run with hidden output.
For curl as a source, `download=true` is forbidden: use `stdout=file` to save the
body as the pipeline's `stdout.txt`. HTTP status, headers and timings remain
separate from the filtered body.

Pipelines and HTTP checks have a shared 15-second deadline covering preparation,
SSH and execution. Files are capped at 10 MiB each by the process's file-size
limit. Inline previews include up to 32 KiB stdout, 16 KiB stderr, 8 KiB HTTP
headers and 4 KiB HTTP metadata within the Runner's 64 KiB capture budget.
Truncation is marked. Temporary directories use mode 0700 and files mode 0600.
Only requested output files are retained after normal completion; errors also
retain requested diagnostic artifacts. Read them with `read_file`/`search_file`
on the same host. Saved files contain raw bytes, potentially including secrets;
only responses sent to the model are redacted. Files stay until host `/tmp`
cleanup; the agent has no general file-writing, deletion or execution tool.

Local cancellation stops the pipeline's process group. Remote cancellation sends
a termination signal when possible; an independent remote watchdog also bounds
execution after a disconnect. If no final report is received, statuses are `-1`
and `execution_complete=false`; announced artifact paths may be unconfirmed.
Interrupted jobs can leave private temporary files.

## Execution limits and data access

Each diagnostic command has a 15-second timeout, including SSH connection and handshake.
Lab requests instead have a 120-second host-side limit and a 125-second caller
limit including SSH; each node diagnostic has its own 15-second limit. Lab
privileges, resource constraints and cleanup are described in [Containerlab](containerlab.md).
Captured stdout and stderr together are limited to 64 KiB, with an explicit
truncation marker. Local arguments are passed directly to the process; SSH
arguments are quoted for the remote shell. The model cannot supply a program or
arbitrary shell command. Cancelling SSH closes the connection, but does not
guarantee that the remote process has exited. Logs can contain application
secrets; review which hosts/users the agent may access before sending them to
the model.
