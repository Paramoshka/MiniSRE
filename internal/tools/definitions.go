package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"mini-sre/internal/remote"
)

const (
	defaultToolTimeout = 50 * time.Second
	labToolTimeout     = 130 * time.Second
)

// Definition declares one model-visible tool and its handler.
type Definition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Run         RunFunc
	Pipeline    *PipelineSource // nil when the tool cannot be a pipeline source
	Mutating    bool            // must not be scheduled or exposed as read-only
	Timeout     time.Duration   // zero uses defaultToolTimeout
}

type RunFunc func(ctx context.Context, t *Tools, args json.RawMessage) (string, error)

// PipelineSource describes how a tool maps to a pipeline source stage.
type PipelineSource struct {
	Stage  func(ctx context.Context, t *Tools, args toolArgs) (remote.Stage, error)
	ExitOK func(code int) bool // nil means only exit code 0 is successful
}

func pipelineCommand(build func(toolArgs) (remote.Command, error)) *PipelineSource {
	return &PipelineSource{Stage: func(_ context.Context, _ *Tools, args toolArgs) (remote.Stage, error) {
		cmd, err := build(args)
		if err != nil {
			return remote.Stage{}, err
		}
		return remote.Stage{Commands: []remote.Command{cmd}}, nil
	}}
}

func pipelineStatic(build func() remote.Command) *PipelineSource {
	return pipelineCommand(func(toolArgs) (remote.Command, error) { return build(), nil })
}

func pipelineStage(fn func(ctx context.Context, t *Tools, args toolArgs) (remote.Stage, error)) *PipelineSource {
	return &PipelineSource{Stage: fn}
}

func pipelineExit(source *PipelineSource, ok func(code int) bool) *PipelineSource {
	source.ExitOK = ok
	return source
}

var diagnosticExit = func(code int) bool { return code == 1 }

func (d Definition) spec() Spec {
	return Spec{Name: d.Name, Description: d.Description, Parameters: d.Parameters, Mutating: d.Mutating}
}

func runDefinition(ctx context.Context, def Definition, t *Tools, raw json.RawMessage) (string, error) {
	timeout := def.Timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return def.Run(ctx, t, raw)
}

// standard decodes the shared argument object before calling the handler.
func standard(fn func(context.Context, *Tools, toolArgs) (string, error)) RunFunc {
	return func(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
		args, err := decodeArgs(raw)
		if err != nil {
			return "", err
		}
		return fn(ctx, t, args)
	}
}

var (
	definitionsOnce   sync.Once
	cachedDefinitions []Definition
)

func definitions() []Definition {
	definitionsOnce.Do(func() {
		base := baseDefinitions()
		labs := labDefinitions()
		defs := make([]Definition, 0, len(base)+1+len(labs))
		defs = append(defs, base...)
		defs = append(defs, pipelineDefinition(base))
		cachedDefinitions = append(defs, labs...)
	})
	return cachedDefinitions
}

func baseDefinitions() []Definition {
	return []Definition{
		{
			Name: "list_hosts", Description: "List available host IDs. local is the machine running the tool executor.",
			Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Run: standard(func(_ context.Context, t *Tools, _ toolArgs) (string, error) {
				return strings.Join(t.Runner.Hosts(), "\n"), nil
			}),
		},
		{
			Name:        "get_load_average",
			Description: "Get host load average for the last 1, 5 and 15 minutes with running/total process counts. host_id defaults to local.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.LoadAverage(ctx, args.HostID)
			}),
			Pipeline: pipelineStatic(loadCommand),
		},
		{
			Name:        "get_vmstat",
			Description: "Measure current CPU activity, runnable/blocked processes, memory, swap and block I/O with vmstat: three samples at one-second intervals, excluding the initial since-boot report. Memory and swap units are KiB. Requires procps-ng vmstat on the selected host. A short healthy sample does not exclude an earlier incident. Defaults: host_id=local. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."}},"additionalProperties":false}`),
			Run: func(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
				var args struct {
					HostID string `json:"host_id"`
				}
				if err := decodeObject(raw, &args); err != nil {
					return "", err
				}
				return t.VMStat(ctx, args.HostID)
			},
			Pipeline: pipelineStatic(vmstatCommand),
		},
		{
			Name:        "get_memory",
			Description: "Report Linux memory, swap, PSI pressure stall averages and OOM/reclaim counters from /proc. MemAvailable is an estimate; used swap does not by itself mean active swapping; PSI and counters may be unavailable. A healthy reading does not exclude an earlier incident. Defaults: host_id=local. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.Memory(ctx, args.HostID)
			}),
		},
		{
			Name:        "get_disk_usage",
			Description: "Get disk usage (total, used, available, percent used) for a filesystem path.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"path":{"type":"string","description":"Filesystem path to inspect; default /."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.DiskUsage(ctx, args.HostID, args.Path)
			}),
			Pipeline: pipelineCommand(func(args toolArgs) (remote.Command, error) {
				return diskCommand(args.Path), nil
			}),
		},
		{
			Name:        "get_top_processes",
			Description: "Find resource consumers among visible processes sorted by memory (RSS), lifetime CPU, or a one-second interval CPU sample (cpu_interval), descending. cpu_interval uses procps-ng top with an isolated configuration, omits idle processes and does not read a personal toprc. Only the top rows are returned; use search_processes to find a process by name. Shows PID, user, CPU%, MEM%, RSS in KiB and executable name, without command-line arguments. CPU% is a lifetime average for cpu and an interval measurement for cpu_interval; it can exceed 100% on multiple cores. Defaults: host_id=local, sort_by=memory, limit=10.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"sort_by":{"type":"string","enum":["memory","cpu","cpu_interval"],"description":"Sort descending by resident memory, lifetime-average CPU, or a one-second interval CPU sample; default memory."},"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum number of processes; default 10."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.TopProcesses(ctx, args)
			}),
			Pipeline: pipelineStage(func(_ context.Context, _ *Tools, args toolArgs) (remote.Stage, error) {
				if args.SortBy == "cpu_interval" {
					return remote.Stage{}, errors.New("tools: cpu_interval is not supported as a pipeline source")
				}
				cmd, _, limit, err := processCommand(args)
				if err != nil {
					return remote.Stage{}, err
				}
				return remote.Stage{Commands: []remote.Command{cmd}, HeadLines: limit + 1}, nil
			}),
		},
		{
			Name:        "search_processes",
			Description: "Search all visible process names by a literal fragment, ignoring case, independently of CPU/RSS rankings. Returns PID and name; no matches is a diagnostic result, not an execution error. Names may be limited to 15 characters: use a short fragment. Does not search command lines or environments. A matching process proves existence, not service health. Defaults: host_id=local, limit=100; at most 500 matches returned.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"pattern":{"type":"string","minLength":1,"maxLength":128,"description":"Literal process name fragment, case insensitive; at most 128 bytes, no NUL or newlines. Use a short fragment because names may be limited to 15 characters."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum matching processes returned; default 100."}},"required":["pattern"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.SearchProcesses(ctx, args)
			}),
			Pipeline: &PipelineSource{
				Stage: func(_ context.Context, _ *Tools, args toolArgs) (remote.Stage, error) {
					cmd, limit, err := processSearchCommand(args)
					if err != nil {
						return remote.Stage{}, err
					}
					return remote.Stage{Commands: []remote.Command{cmd}, HeadLines: limit}, nil
				},
				ExitOK: diagnosticExit,
			},
		},
		{
			Name: "get_service_status", Description: "Read systemd service or Docker container state. backend defaults to systemd; scope defaults to system. Use scope=user for user systemd services. Does not restart services.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"service":{"type":"string","description":"Systemd service name or Docker container name/ID."},"backend":{"type":"string","enum":["systemd","docker"]},"scope":{"type":"string","enum":["system","user"],"description":"Systemd only: system (default) or the executing user's service manager."}},"required":["service"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.ServiceStatus(ctx, args.HostID, args.ServiceTarget)
			}),
			Pipeline: pipelineCommand(func(args toolArgs) (remote.Command, error) {
				cmd, _, err := serviceStatusCommand(args.ServiceTarget)
				return cmd, err
			}),
		},
		{
			Name:        "list_failed_services",
			Description: "List failed systemd units, including services, sockets and mounts, with systemctl --failed. scope defaults to system; scope=user uses the executing user's manager. Empty output means no failed units in that scope. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"scope":{"type":"string","enum":["system","user"],"description":"Systemd manager to inspect; default system."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.FailedServices(ctx, args.HostID, args.Scope)
			}),
			Pipeline: pipelineCommand(failedServicesCommand),
		},
		{
			Name: "get_service_logs", Description: "Read recent journalctl or Docker container logs. backend defaults to systemd; scope defaults to system. Use scope=user for user systemd services; omit service for the selected journal. Systemd only: priority selects a minimum severity, boot selects the current, previous or an explicit boot ID, and output=cat omits timestamps and metadata for counting repeated messages. Defaults: 100 records from the last 60 minutes. No follow mode.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"service":{"type":"string"},"backend":{"type":"string","enum":["systemd","docker"]},"scope":{"type":"string","enum":["system","user"],"description":"Systemd only: system (default) or the executing user's journal."},"lines":{"type":"integer","minimum":1,"maximum":500},"since_minutes":{"type":"integer","minimum":1},"priority":{"type":"string","enum":["emerg","alert","crit","err","warning","notice","info","debug"],"description":"Systemd only: minimum journal severity."},"boot":{"type":"string","description":"Systemd only: current, previous, or a 32-character lowercase hexadecimal boot ID."},"output":{"type":"string","enum":["short-iso","cat"],"description":"Systemd only: output format; cat omits timestamps and metadata, useful before count."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.ServiceLogs(ctx, args)
			}),
			Pipeline: pipelineCommand(serviceLogsCommand),
		},
		{
			Name:        "list_containers",
			Description: "List Docker containers with ID, name, image, state, status and ports. Includes stopped containers by default; running_only=true hides them. limit defaults to 100 and accepts 1..500 rows. Requires Docker CLI and daemon access. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"running_only":{"type":"boolean","description":"Only running containers; default false (include stopped)."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum rows returned; default 100."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.Containers(ctx, args)
			}),
			Pipeline: pipelineStage(func(_ context.Context, _ *Tools, args toolArgs) (remote.Stage, error) {
				cmd, limit, err := containersCommand(args)
				if err != nil {
					return remote.Stage{}, err
				}
				return remote.Stage{Commands: []remote.Command{cmd}, HeadLines: limit}, nil
			}),
		},
		{
			Name:        "read_file",
			Description: "Read a range of lines or the last lines of one regular file. Requires an absolute path; hidden components are forbidden in both the requested path and resolved symlink target. Sensitive /proc files (environ, cmdline, mem, fd, fdinfo) are forbidden. Defaults: host_id=local, start_line=1, lines=100. Set tail=true to read the end; do not supply start_line with tail=true. No follow mode.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"path":{"type":"string","description":"Absolute path to one regular file; hidden components, . and .. are forbidden."},"start_line":{"type":"integer","minimum":1,"description":"First line, numbered from 1; default 1. Cannot be supplied with tail=true."},"lines":{"type":"integer","minimum":1,"maximum":500,"description":"Number of lines; default 100."},"tail":{"type":"boolean","description":"Read the last lines instead of a range; default false."}},"required":["path"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.ReadFile(ctx, args)
			}),
			Pipeline: pipelineStage(func(ctx context.Context, t *Tools, args toolArgs) (remote.Stage, error) {
				cmd, err := t.readFileCommand(ctx, args)
				if err != nil {
					return remote.Stage{}, err
				}
				return remote.Stage{Commands: []remote.Command{cmd}}, nil
			}),
		},
		{
			Name:        "search_file",
			Description: "Search one regular file and return matching lines with line numbers. Requires an absolute path; hidden components are forbidden in both the requested path and resolved symlink target. Sensitive /proc files (environ, cmdline, mem, fd, fdinfo) are forbidden. Defaults: host_id=local, literal matching, case sensitive, limit=100. Set regex=true for grep extended regular expressions. No recursive search.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"path":{"type":"string","description":"Absolute path to one regular file; hidden components, . and .. are forbidden."},"pattern":{"type":"string","minLength":1,"description":"Nonempty literal text, or an extended regular expression when regex=true."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum number of matching lines; default 100."},"regex":{"type":"boolean","description":"Use extended regular expressions instead of literal matching; default false."},"ignore_case":{"type":"boolean","description":"Ignore case; default false."}},"required":["path","pattern"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.SearchFile(ctx, args)
			}),
			Pipeline: &PipelineSource{
				Stage: func(ctx context.Context, t *Tools, args toolArgs) (remote.Stage, error) {
					cmd, err := t.searchFileCommand(ctx, args)
					if err != nil {
						return remote.Stage{}, err
					}
					return remote.Stage{Commands: []remote.Command{cmd}}, nil
				},
				ExitOK: diagnosticExit,
			},
		},
		{
			Name:        "list_directory",
			Description: "List one directory level with type, size, modification time and symlink target. Requires an absolute path; hidden path components and hidden entries are not shown, and symlink targets with hidden components are hidden. Does not recurse. limit defaults to 100 and accepts 1..500. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"path":{"type":"string","description":"Absolute directory path; hidden components, . and .. are forbidden."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum entries returned; default 100."}},"required":["path"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.Directory(ctx, args)
			}),
		},
		{
			Name:        "get_ip_rules",
			Description: "Show policy routing rules with ip rule show. Optional table filters rules referring to that routing table; it does not show routes inside the table. Defaults: host_id=local, family=ipv4. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["ipv4","ipv6"]},"table":{"type":"string","description":"Optional routing table name or positive decimal 32-bit ID without leading zeros."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.IPRules(ctx, args)
			}),
			Pipeline: pipelineCommand(ipRulesCommand),
		},
		{
			Name:        "get_ip_routes",
			Description: "Show routes from main, default, local, a custom table, or all tables. The table named default is different from the default route in main. Defaults: host_id=local, family=ipv4, table=main. Optional interface filter. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["ipv4","ipv6"]},"table":{"type":"string","description":"Table name, positive decimal 32-bit ID without leading zeros, or all; default main."},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.IPRoutes(ctx, args)
			}),
			Pipeline: pipelineCommand(ipRoutesCommand),
		},
		{
			Name:        "get_route_to",
			Description: "Ask the kernel which route it would use for an IP destination. Optional source IP, packet mark (fwmark), input and output interfaces model policy routing. Address family follows destination; source must match it. This lookup sends no probe packets and does not prove reachability.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"destination":{"type":"string","description":"Required IPv4 or IPv6 address, without CIDR prefix or zone; no DNS hostname."},"source":{"type":"string","description":"Optional source IP of the same family."},"mark":{"type":"integer","minimum":0,"maximum":4294967295,"description":"Optional packet fwmark as an unsigned 32-bit decimal integer."},"input_interface":{"type":"string","description":"Optional incoming interface, for forwarded-packet lookup."},"output_interface":{"type":"string","description":"Optional outgoing interface constraint."}},"required":["destination"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.RouteTo(ctx, args)
			}),
			Pipeline: pipelineCommand(routeToCommand),
		},
		{
			Name:        "ping_host",
			Description: "Send a bounded ICMP echo probe to one IP address or DNS hostname from the selected host. Defaults: host_id=local, count=3, family=all (automatic); maximum 5 packets, 2-second reply timeout, 8-second deadline. Packet loss is a diagnostic result, not a command execution error. No flood mode. ICMP replies do not prove application health, and missing replies do not prove a host is down.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"destination":{"type":"string","description":"One IP address or ASCII DNS hostname; use interface for IPv6 link-local scope."},"family":{"type":"string","enum":["all","ipv4","ipv6"]},"interface":{"type":"string","description":"Optional outgoing interface name."},"count":{"type":"integer","minimum":1,"maximum":5,"description":"Number of echo requests; default 3."}},"required":["destination"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.PingHost(ctx, args)
			}),
			Pipeline: pipelineExit(pipelineCommand(pingCommand), diagnosticExit),
		},
		{
			Name:        "check_tcp_port",
			Description: "Open one bounded TCP connection from the selected host to one IP address or DNS hostname and port, with a 5-second timeout. Reports success or the connection error; no application data is sent. A successful handshake does not prove application health. Requires Bash and timeout on the probe host. Defaults: host_id=local; port is required. Active probe; does not change host configuration.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"destination":{"type":"string","description":"One IP address without a zone or ASCII DNS hostname."},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"TCP port to connect to."}},"required":["destination","port"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.TCPConnect(ctx, args)
			}),
			Pipeline: pipelineExit(pipelineCommand(tcpCommand), func(code int) bool {
				return code == 1 || code == 124
			}),
		},
		{
			Name:        "check_tls_certificate",
			Description: "Retrieve and verify the TLS certificate for one host and port with OpenSSL. Reports subject, issuer, validity and days remaining, SANs, key, protocol/cipher, chain length and hostname/chain verification. Optional server_name overrides SNI and the expected name. Verification failure is reported as data. Requires openssl 1.1.0+, Bash and timeout on the probe host. A valid certificate does not prove application health; the probe sends no application data.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"destination":{"type":"string","description":"One IP address without a zone or ASCII DNS hostname."},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"TLS port; default 443."},"server_name":{"type":"string","description":"Optional SNI and expected DNS name or IP address; defaults to destination."}},"required":["destination"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.TLSCertificate(ctx, args)
			}),
		},
		{
			Name:        "get_network_sockets",
			Description: "Show TCP/UDP sockets with numeric endpoints and process details visible to the executing user. Defaults: host_id=local, protocol=both, state=listening, family=all. Filter by port on either endpoint. TCP connection states require protocol=tcp. Read-only; never closes sockets.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["all","ipv4","ipv6"]},"protocol":{"type":"string","enum":["both","tcp","udp"]},"state":{"type":"string","enum":["listening","all","established","syn-sent","syn-recv","time-wait","close-wait"]},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"Optional local or peer port."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.NetworkSockets(ctx, args)
			}),
			Pipeline: pipelineCommand(socketsCommand),
		},
		{
			Name:        "get_network_interfaces",
			Description: "Show interface IP addresses, link state, MTU and RX/TX counters using ip address show and ip -s link show. Defaults: host_id=local, family=all, all interfaces. Optional interface filter. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["all","ipv4","ipv6"]},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.NetworkInterfaces(ctx, args)
			}),
			Pipeline: pipelineStage(func(_ context.Context, _ *Tools, args toolArgs) (remote.Stage, error) {
				commands, err := interfacesCommands(args)
				if err != nil {
					return remote.Stage{}, err
				}
				return remote.Stage{Commands: commands}, nil
			}),
		},
		{
			Name:        "get_ip_neighbors",
			Description: "Show neighbor cache entries and their states using ip neigh show. IPv4 shows ARP neighbors; IPv6 shows NDP neighbors. Defaults: host_id=local, family=ipv4, all interfaces. Optional interface filter. An empty cache alone does not prove a connectivity problem. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["ipv4","ipv6"]},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.IPNeighbors(ctx, args)
			}),
			Pipeline: pipelineCommand(neighborsCommand),
		},
		{
			Name:        "dig_dns",
			Description: "Run a bounded dig DNS query from the selected host, retaining full output including DNS status. Requires one ASCII DNS name or IP; IP queries are reverse PTR lookups. Defaults: host_id=local, record_type=A for names or PTR for IPs, system DNS servers, port=53. Timeout 2 seconds per attempt, one try, no search suffixes or .digrc. Sends DNS requests without changing configuration; no zone transfers or arbitrary flags.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"query":{"type":"string","description":"One ASCII DNS name (including SRV underscores or a trailing dot), or IPv4/IPv6 address without a zone for reverse lookup."},"record_type":{"type":"string","enum":["A","AAAA","CNAME","MX","NS","TXT","SOA","SRV","PTR","CAA"],"description":"Default A for names; IP queries require PTR and default to it."},"server":{"type":"string","description":"Optional DNS server as an IPv4/IPv6 address without a zone; default system DNS servers."},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"DNS server port; default 53."}},"required":["query"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.DigDNS(ctx, args)
			}),
			Pipeline: pipelineCommand(digCommand),
		},
		{
			Name:        "nslookup_dns",
			Description: "Run a noninteractive bounded nslookup DNS query from the selected host. Requires one ASCII DNS name or IP; IP queries are reverse PTR lookups. Defaults: host_id=local, record_type=A for names or PTR for IPs, system DNS servers, port=53. Timeout 2 seconds, retry=1, no search suffixes. Nonzero exit codes retain diagnostic output, including negative DNS answers. Sends DNS requests without changing configuration; no zone transfers or arbitrary flags.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"query":{"type":"string","description":"One ASCII DNS name (including SRV underscores or a trailing dot), or IPv4/IPv6 address without a zone for reverse lookup."},"record_type":{"type":"string","enum":["A","AAAA","CNAME","MX","NS","TXT","SOA","SRV","PTR","CAA"],"description":"Default A for names; IP queries require PTR and default to it."},"server":{"type":"string","description":"Optional DNS server as an IPv4/IPv6 address without a zone; default system DNS servers."},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"DNS server port; default 53."}},"required":["query"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.NslookupDNS(ctx, args)
			}),
			Pipeline: pipelineCommand(nslookupCommand),
		},
		{
			Name:        "curl_http",
			Description: "Run a bounded HTTP/HTTPS GET or HEAD from one host, retaining status, response headers, server IP, timings and a body preview. Defaults: host_id=local, method=GET, verified TLS, no redirects; connection timeout 3s, transfer timeout 10s, max body 10 MiB. HTTP 4xx/5xx are diagnostic responses; inspect the HTTP status. Optional headers, follow_redirects (max 5), and insecure (only when explicitly requested by the user). download=true saves the raw body under the original URL filename in a new private /tmp/mini-sre-* directory on the selected host. No uploads, credentials in URLs, arbitrary flags or output paths; requires curl >= 8.4.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"url":{"type":"string","description":"Required absolute http:// or https:// URL, without embedded credentials."},"method":{"type":"string","enum":["GET","HEAD"],"description":"Default GET."},"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"Custom header name/value pairs; no CR/LF/NUL or header files."},"follow_redirects":{"type":"boolean","description":"Default false; follow up to five HTTP/HTTPS redirects."},"insecure":{"type":"boolean","description":"Default false; disable certificate verification only when explicitly requested by the user."},"download":{"type":"boolean","description":"GET only; save raw body to a private /tmp/mini-sre-* directory using a non-hidden filename from the original URL. Default false."}},"required":["url"],"additionalProperties":false}`),
			Run: standard(func(ctx context.Context, t *Tools, args toolArgs) (string, error) {
				return t.CurlHTTP(ctx, args)
			}),
			Pipeline: pipelineStage(func(ctx context.Context, t *Tools, args toolArgs) (remote.Stage, error) {
				if args.Download {
					return remote.Stage{}, errors.New("tools: pipeline source cannot use download; use stdout=file instead")
				}
				cmd, _, err := curlCommand(args)
				if err != nil {
					return remote.Stage{}, err
				}
				if err := t.checkCurlVersion(ctx, args.HostID); err != nil {
					return remote.Stage{}, err
				}
				return remote.Stage{Commands: []remote.Command{cmd}}, nil
			}),
		},
	}
}
