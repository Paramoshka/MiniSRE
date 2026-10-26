# Diagnostic tools

[Back to README](../README.md)

Arguments, defaults, command dependencies and execution limits. You can ask the
agent in natural language; the tool names below describe the API it uses.

- [Command dependencies](#command-dependencies)
- [Tool reference](#tool-reference)
- [Services and logs](#services-and-logs), [processes](#processes), [memory and pressure](#memory-and-pressure), [files](#files)
- [Disk space and I/O](#disk-space-and-io)
- [Network](#network-diagnostics), [Throughput](#throughput-tests), [DNS](#dns), [HTTP](#http-checks-and-downloads), [TLS certificates](#tls-certificates)
- [Pipelines and saved output](#pipelines-and-output-files)
- [Execution limits and data access](#execution-limits-and-data-access)

## Command dependencies

Commands are needed on the host where a probe runs, locally or over SSH.

Base probes require Linux with `/proc/loadavg` and GNU `cat` and `df`.
`get_disk_io` additionally needs `env` and sysstat `iostat` supporting
`-d -x -k -y`. `get_directory_usage` needs GNU `du`, `timeout`, `readlink`
and `stat`. Missing commands are reported; the bot does not install packages.

Systemd probes need `systemctl` and `journalctl`; Docker probes need the Docker
CLI and access to its daemon. These commands must be available in the SSH user's
noninteractive PATH on remote hosts. Commands run as the current local or SSH
user; the agent does not invoke `sudo`.

Process probes require `ps` and `pgrep` from procps-ng, locally or in the SSH user's PATH.
The `get_vmstat` probe requires procps-ng `vmstat` with `-w`, `-n`, `-y` and
`-S K` support on the selected host. `get_top_processes` with
`sort_by=cpu_interval` additionally requires procps-ng `top` with `-b`, `-n`,
`-d`, `-o`, `-w` and `-i` support, plus `env` from coreutils. The probe runs
`top` with `HOME` and `XDG_CONFIG_HOME` pointing at a nonexistent path, so a
personal `toprc` cannot enable command-line display or change idle filtering.
The agent does not install missing commands.

File probes require GNU `sed` (with `--sandbox`), `grep`, `tail`, `readlink` and
`stat`; directory listings additionally need GNU `find`, locally or in the SSH
user's noninteractive PATH.

Network probes require `ip` and `ss` from iproute2, and `ping` from iputils.
`tcp_info=true` requires `ss -i`. `get_network_activity` needs `ip -j -s link`
JSON statistics (`stats64` or `stats`), readable `/proc/uptime`, Bash, `sleep`
and GNU `timeout`.
They must also be available in the SSH user's noninteractive PATH. ICMP socket
permissions depend on the host's ping configuration; failures are reported
without attempting to elevate privileges. `check_tcp_port` also requires Bash and
`timeout` on the probe host. `measure_throughput` requires `iperf3` 3.x and GNU
`timeout` on both hosts; the server host also needs `ss` from iproute2 to check
that the port is free and confirm the temporary listener.

DNS probes require BIND `dig` and `nslookup`, locally or in the SSH user's
noninteractive PATH.

HTTP probes require curl >= 8.4, including on remote hosts. Pipelines and HTTP
probes also require Bash and GNU coreutils (`timeout`, `mktemp`, `mkdir`, `stat`,
`head`, `cat`, `rm`, `rmdir`); filters use GNU `grep`, `tail`, `sort`, `uniq` and
sandboxed `sed`.
These commands must be in the executing user's noninteractive PATH. No PTY or
privilege escalation is used.

TLS certificate probes require OpenSSL 1.1.0 or newer for `-verify_hostname` and
`-verify_ip`, plus Bash and `timeout` on the probe host.

## Tool reference

Diagnostic probes except `list_hosts` accept `host_id`, defaulting to `local`;
`measure_throughput` instead takes explicit `server_host_id` and `client_host_id`.
The optional [containerlab tools](containerlab.md) require an explicit enabled
`host_id` and use a separately installed host helper.

| Tool | Other arguments | Behavior |
|---|---|---|
| `list_hosts` | none | list configured IDs and `local` |
| `get_load_average` | none | load for 1, 5, 15 minutes and process counts |
| `get_vmstat` | none | three samples at one-second intervals: CPU, runnable/blocked processes, memory, swap and block I/O; excludes the since-boot report |
| `get_memory` | none | memory, swap, PSI stall averages and OOM/reclaim counters from `/proc`; unavailable sources are reported as limitations |
| `get_disk_usage` | `path`, default `/` | total, used, available bytes and inode counts, with usage percentages; unsupported inode statistics are unavailable |
| `get_directory_usage` | required `path`; `limit=10` (1..50) | allocated bytes in the directory and its largest immediate subdirectories; bounded recursive scan on one filesystem |
| `get_disk_io` | none | three one-second per-device samples of read/write rates, latency, queue length and utilization |
| `get_top_processes` | `sort_by`, default `memory`; `limit`, default `10` | descending ranking by RSS, lifetime-average CPU, or one-second interval CPU (`cpu_interval` via `top`), with PID, user, CPU%, MEM%, RSS in KiB and process name |
| `search_processes` | required `pattern`; `limit=100` | case-insensitive literal name search across all visible processes, returning PID and name |
| `get_service_status` | required `service`; `backend`, default `systemd`; `scope`, default `system` | systemd unit state or Docker container `.State` |
| `get_service_logs` | `service`, `backend`, `scope`, `lines`, `since_minutes`, `priority`, `boot`, `output` | recent journal or container logs; systemd priority, boot and output-format filters |
| `list_failed_services` | `scope`, default `system` | failed systemd units via `systemctl --failed`; empty output means none in that scope |
| `list_containers` | `running_only=false`, `limit=100` | Docker containers with ID, name, image, state, status and ports; includes stopped containers by default |
| `read_file` | required `path`; `start_line=1`, `lines=100`, `tail=false` | read a line range or the last lines of one file |
| `search_file` | required `path`, `pattern`; `limit=100`, `regex=false`, `ignore_case=false` | matching lines with line numbers from one file |
| `list_directory` | required `path`; `limit=100` | one directory level with type, size, modification time and symlink target; hidden entries are omitted |
| `get_ip_rules` | `family=ipv4`, optional `table` | policy routing rules, optionally filtered by referenced table |
| `get_ip_routes` | `family=ipv4`, `table=main`, optional `interface` | routes from one table or `all` tables |
| `get_route_to` | required IP `destination`; optional `source`, `mark`, `input_interface`, `output_interface` | the kernel's route lookup for the supplied packet attributes |
| `ping_host` | required `destination`; `count=3`, `family=all`, optional `interface` | bounded ICMP echo probe to one IP address or DNS hostname |
| `check_tcp_port` | required `destination`, `port` | one bounded TCP connection from the selected host; reports success or the connection error |
| `measure_throughput` | required `server_host_id`, `client_host_id`, `destination`, `bind_address`; `port=5201`, `duration=10` | temporary one-off iperf3 server and one TCP upload; summary bitrate, bytes and retransmits; active, mutating and not schedulable |
| `check_tls_certificate` | required `destination`; `port=443`, optional `server_name` | certificate subject, issuer, validity, SANs, key, protocol/cipher and verification result |
| `get_network_sockets` | `protocol=both`, `state=listening`, `family=all`, optional `port`, `tcp_info=false` | TCP/UDP sockets; optional internal TCP information requires `protocol=tcp` |
| `get_network_interfaces` | `family=all`, optional `interface` | IP addresses, link state, MTU and RX/TX counters |
| `get_network_activity` | optional `interface` | RX/TX bytes/s and interval changes in packet, error and drop counters; no test traffic |
| `get_ip_neighbors` | `family=ipv4`, optional `interface` | ARP or NDP neighbor cache entries and states |
| `dig_dns` | required `query`; optional `record_type`, `server`, `port` | bounded DNS query with full dig output and DNS status |
| `nslookup_dns` | required `query`; optional `record_type`, `server`, `port` | bounded noninteractive DNS query with nslookup |
| `curl_http` | required `url`; `method=GET`, optional `headers`, `follow_redirects=false`, `insecure=false`, `download=false` | HTTP/HTTPS response status, headers, server IP, timings and body preview, or saved raw body |
| `run_pipeline` | required `source`; optional `filters`, `stdout=inline`, `stderr=inline` | source output through validated grep/head/tail/sed/count filters; return, save or discard streams, retaining all stage statuses |
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
`running_only=true` to hide them. `limit` accepts 1..500 rows and defaults to 100;
it is passed to Docker as `--last`, so the daemon returns at most that many
containers in the same newest-first order. The Docker CLI and daemon access must
be available to the executing user. An empty list is a diagnostic result; a daemon
or permission failure is an error. Neither discovery tool proves that a unit or
container is healthy.

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

For systemd, `priority` selects a minimum severity: `err` includes `emerg` through
`err`, and the allowed values are `emerg`, `alert`, `crit`, `err`, `warning`,
`notice`, `info` and `debug`. `boot` accepts `current`, `previous`, or a
32-character lowercase hexadecimal boot ID. These filters are useful for kernel
errors and OOM events and for checking whether an incident happened before a
reboot. The relative `since_minutes` window still applies, so use a large enough
value with `boot=previous`. `output=cat` omits timestamps, host and unit metadata
and leaves only messages; use it as a pipeline source before `count` when
repeated log lines would otherwise differ by timestamp. `priority`, `boot` and
`output` are rejected for Docker logs.

## Disk space and I/O

`get_disk_usage` checks both bytes and inodes using GNU `df`. Exhausted inodes
can prevent file creation even when bytes remain available. Unsupported or
zero-total inode statistics are shown as unavailable, not healthy. Its pipeline
source now includes `itotal`, `iused` and `iavail` after the existing byte columns.

`get_directory_usage` requires a specific absolute directory path. The root
undergoes the same canonical-path checks as `list_directory`. GNU `du` scans
recursively, prints a directory total (including immediate files) and immediate
subdirectory totals, and the tool returns the largest `limit=1..50` subdirectories.
Sizes measure allocated bytes, not apparent file lengths. It stays on one
filesystem, excludes hidden entries at every level, and does not follow nested
symlinks. Its output depth does not limit traversal work: use it only when
investigating space exhaustion. Exclusions, permissions and filesystem accounting
can explain differences from `df`.

An independent remote `timeout` limits the scan to 15 seconds, with a one-second
kill grace and an 18-second executor budget. Unfinished, malformed or truncated
output and command failures return an error containing any complete records
already observed, explicitly marked incomplete. Such observations are not a
complete ranking or total, and cannot support an `ok` scheduled result.

`get_disk_io` runs `env S_COLORS=never iostat -d -x -k -y 1 3`: three interval
samples, without the since-boot report or ANSI colors. Rates are KiB/s; `await`
or `r_await`/`w_await` are milliseconds including queue time; `aqu-sz` (older
`avgqu-sz`) is average queue length. `%util` measures busy time; it does not by
itself establish saturation of SSDs or RAID. Correlate with `vmstat`, PSI and
the reported symptom. Do not add stacked-device counters as independent disks.
Missing commands, unsupported columns, incomplete samples and truncated output
are errors. A short healthy sample does not rule out an intermittent incident.

These two new tools are available in chat and scheduled checks, but are not
pipeline sources. They do not change host configuration or generate load tests.

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

`get_top_processes` accepts `sort_by=memory`, `sort_by=cpu` or
`sort_by=cpu_interval` and `limit=1..50`. `cpu_interval` runs
`env HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent top -b -n 2 -d 1 -o +%CPU -w 256 -i`
and reports the second sample, so CPU percentages cover a one-second interval;
idle processes are omitted and the first since-boot table is
discarded. The isolated configuration keeps a personal `toprc` from enabling
command-line display or changing idle filtering. When the second table contains
no rows, the report says that no non-idle processes were observed. It requires
procps-ng `top` and `env`. If the output is truncated before the second table,
the probe fails instead of reporting the since-boot table. For example, ask "which processes use the most memory on web-1?",
"top 5 processes by CPU on local", or "what used CPU in the last second on web-1?".
Memory sorting uses RSS (resident physical
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

`list_directory` returns one directory level with the entry type, size for regular
files, modification time and, for symlinks, the target. It requires an absolute
directory path and follows the same hidden-component rules as the file probes.
Hidden entries are omitted, and a symlink target containing a hidden component is
reported as `[hidden target]` instead of being disclosed. The listing does not
recurse; use `read_file` or `search_file` on a discovered file. Up to 500 entries
can be requested; the default is 100. Names containing control characters are
escaped in the report.

For example, ask "read lines 20 through 40 of /etc/nginx/nginx.conf on web-1",
"show the last 50 lines of /var/log/nginx/error.log",
"list /var/log/nginx on web-1", or
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

`check_tcp_port` opens one TCP connection from the selected host to one IP address
or ASCII DNS hostname and a required port (1..65535). The connection attempt has a
five-second deadline. Connection refusal, timeout and DNS failures are diagnostic
results; a missing Bash or `timeout` command is an error. The probe sends no
application data and changes no host configuration, but it does create a TCP
connection. A successful handshake proves only L4 reachability; it does not prove
that the service is healthy or that the peer completed its own startup checks.
The destination must be an IP address without a zone or an ASCII DNS name; use
`ping_host` for ICMP and `curl_http` for HTTP-level checks.

Socket queries accept `protocol=tcp`, `udp` or `both`. States are `listening`
(default), `all`, `established`, `syn-sent`, `syn-recv`, `time-wait` or `close-wait`;
connection states require `protocol=tcp`. `port=1..65535` matches either endpoint.
Process details depend on user permissions. Interface counters are cumulative,
not rates, and an empty neighbor cache alone is not proof of a network failure.

`get_network_sockets` with `tcp_info=true` adds `ss -i` and requires
`protocol=tcp`. Use `state=established` and a known port for ongoing transfers.
RTT and its variation are milliseconds; retransmissions, congestion windows and
other fields depend on the kernel and iproute2 version. This is a snapshot:
counters may cover the connection lifetime, not the current incident. No matching
connections means no transfer evidence, not a healthy network. The option also
works in pipelines, which retain raw `ss` output.

`get_network_activity` takes two JSON link snapshots in one bounded SSH/local
command, separated by a one-second sleep. It uses probe-host uptime timestamps
for the actual interval, so SSH reconnection time is not counted as sampling
time. It reports RX/TX bytes/s and changes in packets, errors and drops. An
optional interface name limits both snapshots. Interfaces are matched by index
and name; missing statistics, changed interfaces and decreasing counters make
the comparison unavailable rather than producing zero or wrapped values.
Malformed or truncated snapshots are errors. When only some interfaces can be
compared, their observations are retained inside an explicitly incomplete error.
No traffic or zero errors does not prove health; growing drops alone do not
establish the cause of application latency. Sampling generates no test traffic,
is available in scheduled checks, and is not a pipeline source.

Example requests: "show routes from table 100 and the rules selecting it on
web-1", "check the route to 192.0.2.5 from 192.0.2.1 with fwmark 42",
"ping 192.0.2.5 three times", "show TCP close-wait sockets on port 443", and
"check eth0 addresses, errors and neighbors".

## Throughput tests

`measure_throughput` runs one TCP upload between two configured hosts with
iperf3. `server_host_id` and `client_host_id` are explicit and must differ;
`local` can be either endpoint. `destination` is the server address as seen by
the client: an IP address without a zone or an ASCII DNS name. `bind_address` is
required and must be a unicast IP address owned by the server. When the server
has several addresses, use `get_network_interfaces` and ask which address to
bind; do not infer it from private ranges. Use a private address and an unused
port: the tool refuses a port that is already listening, and the one-off server
accepts the first client that connects. `port` defaults to 5201 and `duration`
accepts 1..10 seconds (default 10). The test always uses one TCP stream and
client-to-server direction; reverse, UDP and parallel streams are not exposed.

The tool starts a temporary `iperf3 -s -1` on the server, bound to
`bind_address`, waits for `ss` to show the listening port, runs
`iperf3 -c destination -t duration -J` on the client, then stops the server. The
server has a 35-second watchdog and the client caller has a duration+10-second
limit. Both hosts must have `iperf3` 3.x and GNU `timeout`; the server host also
needs `ss`. The temporary listener is removed after one client; the tool stops
the server on a best-effort basis, and a hard crash is bounded by the 35-second
watchdog. The test saturates the path for its duration and is excluded from
scheduled checks.

Connection refusal, timeout and DNS failures are diagnostic results; missing
commands or invalid arguments are errors. The summary reports sender and
receiver bitrates, transferred bytes, retransmits and status. It measures L4 TCP
throughput only: it does not prove application performance, and a firewall or
NAT path that blocks the port will make the test fail. The test requires an
existing route from the client to `destination`; it does not create WireGuard,
routes or firewall rules.

Example requests: "measure upload throughput from proxy to node-1, server
node-1, listen on 10.8.0.2", "run a 5-second iperf3 test from local to web-1 on
port 5202", and "compare throughput from the proxy to both cluster nodes".

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

## TLS certificates

`check_tls_certificate` connects to `destination:port` (default 443), retrieves the
certificate chain with OpenSSL and reports the subject, issuer, validity window,
days remaining, serial, SANs, public key, negotiated protocol/cipher, chain length
and the OpenSSL verification result. Optional `server_name` overrides SNI and the
expected name. For a DNS destination without `server_name` the probe sends SNI and
verifies the hostname; for an IP destination it verifies the IP SAN without SNI.
When `server_name` is an IP address, no SNI is sent. `server_name` may be a DNS
name or an IP address.

Verification uses the probe host's trust store. A failed verification, an expired
or not-yet-valid certificate, or a connection failure is reported as diagnostic
data, not as a tool execution error. A missing OpenSSL command is an error. The
probe sends no application data, but it does establish a TLS connection. A valid
certificate does not prove that the application behind it is healthy. The full
OpenSSL output is not returned; only the parsed certificate details and any
diagnostics are included.

Example requests: "check the certificate for example.com", "when does the
certificate on 192.0.2.10:8443 expire?", and "verify TLS for example.com using
server name api.example.com".

## Pipelines and output files

`run_pipeline` chooses a source by its existing tool name and arguments. Sources
are all command-based diagnostic tools, including curl; `list_hosts`, `get_memory`,
`list_directory`, `check_tls_certificate`, `measure_throughput`, nested pipelines
and all `lab_*` tools are excluded. Source arguments keep their original
validation and defaults. `host_id` belongs to the pipeline, and applies to every stage. Source
streams contain raw command output rather than standalone human-readable
formatting; top processes still respects its requested row limit. In a pipeline,
`get_top_processes` accepts only `sort_by=memory` or `sort_by=cpu`;
`cpu_interval` is not a source because it emits two sampling tables. Interfaces
emit address output followed by link counters.

Up to four stdin filters are allowed: `grep` (literal by default, optional
`regex`/`ignore_case`/`invert`, `limit=100`), `head`/`tail` (`lines=100`), `sed` as
a line range (`start_line=1`, `lines=100`), or `count`. `invert=true` keeps lines
that do not match and is valid only for `grep`. `count` groups identical lines,
sorts by descending occurrence count and returns the most frequent lines; its
`limit` defaults to 20 and `ignore_case` folds case. To count repeated journal
messages, set the `get_service_logs` source argument `output=cat`; otherwise the
timestamp and unit prefix make each line unique. Failures from its internal
`sort`/`uniq` commands are reported as a failed stage; SIGPIPE from the final
`head` limit is expected and is not an error. Match and line limits accept
1..500. No
arbitrary programs, argv, shell expressions, sed scripts or filter file paths
are exposed. Filters run before the final preview is limited, so a match beyond
the first 64 KiB can still be found. Limiting filters drain their remaining input
to avoid a misleading upstream SIGPIPE error.

`stdout` supports `inline`, `file` or `discard`; `stderr` additionally supports
`merge`, joining each stage's stderr into its stdout before the next filter.
Defaults are separate inline streams. Discarding both streams is the equivalent
of hiding data with `>/dev/null 2>&1`; execution status remains visible. Every
stage's exit code is returned. Grep exit code 1 means an empty result, ping
exit code 1 retains its packet-loss meaning, and `check_tcp_port` exit codes 1 and
124 retain their connection-failure and timeout meanings; other unexpected codes
are errors. Execution status does not establish service or application health.

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

Most diagnostic commands have a 15-second timeout, including SSH connection and handshake.
`get_directory_usage` bounds the scan to 15 seconds plus a one-second kill
grace, within an 18-second caller limit. `get_network_activity` uses a
seven-second watchdog, one-second kill grace and ten-second caller limit.
`measure_throughput` has longer limits: its temporary server has a 35-second
watchdog and a 40-second caller limit, and its client has a duration+10-second
caller limit. Each tool call also has a 50-second overall limit, or 130 seconds
for lab tools; its returned text is capped at 128 KiB with a truncation marker.
Lab requests instead have a 120-second host-side limit and a 125-second caller
limit including SSH; each node diagnostic has its own 15-second limit. Lab
privileges, resource constraints and cleanup are described in [Containerlab](containerlab.md).
Scheduled checks receive only non-mutating tools; lab deployment, deletion and
throughput tests are excluded.
Captured stdout and stderr together are limited to 64 KiB, with an explicit
truncation marker. Local arguments are passed directly to the process; SSH
arguments are quoted for the remote shell. The model cannot supply a program or
arbitrary shell command. Cancelling SSH closes the connection, but does not
guarantee that the remote process has exited. Logs can contain application
secrets; review which hosts/users the agent may access before sending them to
the model.
