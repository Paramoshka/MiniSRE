# Containerlab labs

[Back to README](../README.md)

`mini-sre-lab` is a separate Go binary installed on each enabled Linux lab host.
It runs once per SSH request and exits. A systemd timer invokes its `gc` mode
every minute, so TTL cleanup does not depend on Telegram, the model or SSH connectivity.
It requires Docker, containerlab with support for unprivileged Linux nodes,
iproute2, GNU timeout and systemd. Executables are expected in `/usr/bin`.

## Install on the lab host

Build for that host's OS/architecture using this repository, then copy the
binary and examples to the host. Run the following administrative commands
there after reviewing the configuration:

```bash
go build -o mini-sre-lab ./cmd/mini-sre-lab
sudo install -o root -g root -m 0755 mini-sre-lab /usr/local/sbin/mini-sre-lab
sudo install -d -o root -g root -m 0700 /etc/mini-sre /var/lib/mini-sre/labs
sudo install -o root -g root -m 0600 examples/mini-sre-lab.yaml /etc/mini-sre/lab.yaml
sudoedit /etc/mini-sre/lab.yaml
```

Choose a private IPv4 `/24` management subnet that does not overlap host
addresses, non-default routes (including VPN routes), or existing Docker networks.
The example subnet is a candidate, not a reservation for your host. Set a stable
`owner_id`; do not change it while registered labs exist.

Configure exactly two image profiles, `alpine` and `frr`, with immutable
`repository@sha256:...` references. Preload both images through the same local
Docker daemon used by root. They must provide `ip` with JSON output, iputils
ping, traceroute, tcpdump and timeout supporting `-k`; FRR also needs vtysh,
Zebra, BGP/OSPF daemons and the normal `/etc/frr/daemons` startup mechanism.
The helper never installs packages or pulls images during deployment.

[The image Dockerfile](../examples/lab-images/Dockerfile) adds diagnostic
packages to approved Alpine or Alpine-based FRR images and preserves FRR's
existing startup command. See [local image builds](#local-image-builds) for
commands that build both profiles without publishing them. The current helper
still requires repository digests in the host config; local tags and image IDs
are not accepted.

Replace the SSH username in [the sudoers example](../examples/mini-sre-lab.sudoers).
Install only that exact helper command, without wildcard arguments:

```bash
sudo visudo -cf examples/mini-sre-lab.sudoers
sudo install -o root -g root -m 0440 examples/mini-sre-lab.sudoers /etc/sudoers.d/mini-sre-lab
sudo install -o root -g root -m 0644 examples/mini-sre-lab-gc.service examples/mini-sre-lab-gc.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mini-sre-lab-gc.timer
```

The helper refuses non-root execution, symlinked configuration/state paths,
and non-root-owned or group/world-writable parent directories. Its child
environment is fixed; caller-supplied Docker/CLAB environment overrides are not inherited.

Finally, explicitly enable the host in the agent's `hosts.yaml`:

```yaml
lab_hosts: [lab-1]
```

`lab-1` must already be an SSH host ID. Use `[local]` only when the helper is
installed on the agent machine. Lab tools require an explicit enabled `host_id`;
existing configurations keep lab execution disabled.

## Local image builds

Run these commands from the repository root on the lab host, using the same
Docker daemon as the root helper. These base references were checked on
2026-10-12: Alpine 3.24.2 and the Alpine-based FRR 10.7.1. The build downloads
base images and diagnostic packages; no registry login or image push is needed.

```bash
ALPINE_BASE='docker.io/library/alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
FRR_BASE='quay.io/frrouting/frr@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2'

sudo docker build -f examples/lab-images/Dockerfile \
  --build-arg BASE_IMAGE="$ALPINE_BASE" \
  -t mini-sre-alpine:local .

sudo docker build -f examples/lab-images/Dockerfile \
  --build-arg BASE_IMAGE="$FRR_BASE" \
  -t mini-sre-frr:local .

sudo docker image inspect --format '{{.RepoTags}} {{.Id}}' \
  mini-sre-alpine:local mini-sre-frr:local
```

Both resulting images contain `iproute2`, `iputils`, `traceroute`, `tcpdump`
and GNU coreutils (including `timeout`). FRR retains its entrypoint and startup
command because the Dockerfile does not override them.

These commands prepare local images, but do not yet make them usable by the
current helper. Its config accepts only `repository@sha256:<64 hex digits>`;
`mini-sre-alpine:local`, `mini-sre-frr:local` and bare `sha256:...` image IDs
are rejected. A local image ID is not a repository manifest digest: do not
append it to a repository name with `@sha256:` or reuse the base image's digest
for a derived image. To deploy these builds without publishing, the helper
needs support for local image IDs first. With the current helper, use separately
published and preloaded derived images and their own repository digests.

## Describe and operate a lab

Ask mini-sre to create a lab, including its target host and intended routing.
The model supplies restricted YAML, not arbitrary containerlab YAML. See
[the client/router/server example](../examples/lab-topology.yaml).

The document has `name`, a `nodes` mapping and a `links` list. Each node accepts:

- `profile`: `alpine` or `frr`; the host config chooses the actual image.
- `interfaces`: mapping from linked `eth1`..`eth99` or `lo` to IPv4 CIDR lists.
- `routes`: entries with IPv4 `destination` CIDR, `via` IP and linked `interface`.
- `forwarding`: boolean, default false.
- `frr_config`: text for FRR nodes, default a minimal integrated configuration.

Links are pairs such as `[client:eth1, router:eth1]`; endpoints cannot be
reused or refer to the host. Node names are at most 24 characters; names accept
letters, digits, underscores and hyphens. Lab names receive the `msre-lab-`
prefix and are at most 48 characters including that prefix. YAML aliases,
unknown fields, arbitrary images, commands, host links, mounts and lifecycle
hooks are rejected. IPv6 data-plane configuration is outside this first version.
Internal data subnets may repeat across isolated labs but cannot overlap the
lab's management subnet.

FRR configuration supports `frr defaults`, `frr version`, `hostname`,
`service integrated-vtysh-config`, `log stdout`, `router bgp ASN`, `router ospf`,
router IDs, `neighbor IP remote-as ASN`, `neighbor IP activate`,
`address-family ipv4 unicast`, `network PREFIX` with optional `area AREA`,
`redistribute connected|static`, `interface lo|ethN`, `ip ospf area AREA`,
`passive-interface`, `no bgp ebgp-requires-policy`,
`no bgp network import-check`, `exit` and `exit-address-family`.
Blank lines and `!`/`#` comments are allowed; shell, include and file operations
are rejected. Configure interface IPs and static routes through the structured
fields above. Unsupported syntax is an explicit error, not silently ignored.

| Tool | Arguments and behavior |
|---|---|
| `lab_deploy` | `host_id`, `topo_yaml`, optional `ttl_minutes=60` (5..120); create and configure one lab |
| `lab_inspect` | `host_id`, optional `name`; JSON with nodes, management IPs and statuses; managed labs additionally have links and expiry |
| `lab_run` | `host_id`, `name`, `node`, `checks`; 1..4 bounded diagnostic checks |
| `lab_destroy` | `host_id` and either `name` or `all_mine=true`; remove only registered labs |

`lab_run` supports `interfaces`, `routes`, `neighbors`, `bgp`, `ospf`, `ping`,
`traceroute`, and `tcpdump`. Ping/traceroute require a unicast IPv4
`destination`. Ping/tcpdump accept an optional `interface` (`lo`, `eth0` or a
data interface); tcpdump defaults to `any`. Ping sends three packets, traceroute
uses at most eight hops, and tcpdump captures at most twenty packets. Each
check has a 15-second execution budget and an in-container 14-second watchdog.
No arbitrary argv, capture filters, follow mode or shell is exposed.

Results include `exit_code`, `timed_out`, `truncated`, `stdout` and `stderr`.
Each stream preview is capped at 1 KiB. Packet loss and empty timed-out captures
remain diagnostic results. A missing command or an incompatible image returns
its failure status. Existing labs can be inspected and probed but are not
registered for deletion; host/shared-container network modes cannot be probed.
New lab tools cannot be pipeline sources. Scheduled `/task` checks may inspect
and probe labs but cannot deploy or destroy them, even if the model attempts it.

## Resources, persistence and recovery

The helper allows one registered lab per host, at most eight nodes and 32 links.
Every node has a 256 MiB memory limit, 0.5 CPU limit and no automatic restart.
Deployment also requires `MemAvailable` greater than or equal to total node
limits plus a 512 MiB reserve. The 2 GiB maximum covers node memory, not Docker
and host overhead; eight nodes can consume up to four CPU cores.

Nodes use `privileged: false`. Alpine adds `NET_ADMIN`/`NET_RAW`; FRR also adds
`SYS_ADMIN`, which its Zebra daemon requests. Host networking and user-supplied
capabilities are forbidden. Containerlab external access is disabled; Docker
can still change its own host networking/firewall state. Containers share the
host kernel, so these controls are not VM isolation. Images must be trusted.

The helper creates the management bridge network with ownership labels before
calling containerlab, and checkpoints its Docker ID. Containerlab attaches
nodes to that network; the helper removes it explicitly if containerlab leaves it.

Files live under root-owned `/var/lib/mini-sre/labs`, with a durable record
containing the topology, network ID, owner/config snapshot and expiry. Operations acquire
one host-side file lock. A pending record is persisted before deployment;
interrupted deployments expire after 125 seconds, past the host watchdog.
Successful deployment sets the requested TTL from readiness time. Request
processing is bounded at 120 seconds on the host; the caller allows 125 seconds
including SSH overhead. GNU timeout also bounds children after helper death.

Normal TTL cleanup starts on the next minute tick, subject to the host being
up, Docker availability and the operation lock. The timer catches missed ticks
after reboot. Failed cleanup keeps the registry and is retried on later ticks.
Inspect cleanup failures with:

```bash
sudo journalctl -u mini-sre-lab-gc.service -n 50
systemctl list-timers mini-sre-lab-gc.timer
```

Destroy validates container and network ownership, rejects foreign attachments
or a replaced network ID, and regenerates a missing topology from
the saved record, and targets that exact topology. It never invokes
`containerlab destroy --all`. Registry/files are removed only after confirming
the lab's containers and management network are gone. If someone attaches
foreign resources or changes ownership labels, cleanup refuses to proceed.
Do not manually delete the registry or change its owner ID; damaged records
require operator recovery rather than guessed deletion. TTL is unrelated to
host `/tmp` cleanup, and deleting temporary files does not delete containers.

## Verification on a test host

Install the helper and approved images on a disposable Linux host first. Ask
the bot to create the example topology with TTL 5 minutes. Inspect the three
nodes and probe client→server (`10.30.0.2`); inspect routes and neighbors on the
router. To capture traffic concurrently, start a bounded tcpdump manually on
the test node while generating ping traffic, then compare it with `lab_run` output.
Checks within one `lab_run` execute sequentially.

Also create a two-FRR-node BGP/OSPF topology and verify adjacency with the
structured checks. Stop the Telegram bot while a lab exists; confirm the
host timer removes it after TTL. Keep an unrelated existing lab and confirm
it survives `all_mine` and GC. Exercise an interrupted deployment, a missing
generated topology file and a Docker outage; inspect retained records and retry
cleanup after Docker returns. These live tests mutate the test host's network.
