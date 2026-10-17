package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"mini-sre/internal/remote"
)

type Spec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type Func func(ctx context.Context, args json.RawMessage) (string, error)

type Tools struct {
	Runner *remote.Runner
}

func Specs() []Spec {
	specs := []Spec{
		{
			Name: "list_hosts", Description: "List available host IDs. local is the machine running the tool executor.",
			Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        "get_load_average",
			Description: "Get host load average for the last 1, 5 and 15 minutes with running/total process counts. host_id defaults to local.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_vmstat",
			Description: "Measure current CPU activity, runnable/blocked processes, memory, swap and block I/O with vmstat: three samples at one-second intervals, excluding the initial since-boot report. Memory and swap units are KiB. Requires procps-ng vmstat on the selected host. A short healthy sample does not exclude an earlier incident. Defaults: host_id=local. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_memory",
			Description: "Report Linux memory, swap, PSI pressure stall averages and OOM/reclaim counters from /proc. MemAvailable is an estimate; used swap does not by itself mean active swapping; PSI and counters may be unavailable. A healthy reading does not exclude an earlier incident. Defaults: host_id=local. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_disk_usage",
			Description: "Get disk usage (total, used, available, percent used) for a filesystem path.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"path":{"type":"string","description":"Filesystem path to inspect; default /."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_top_processes",
			Description: "Find resource consumers among visible processes sorted by memory (RSS) or CPU, descending. Only the top rows are returned; use search_processes to find a process by name. Shows PID, user, CPU%, MEM%, RSS in KiB and executable name, without command-line arguments. CPU% is a lifetime average, not an interval measurement, and can exceed 100% on multiple cores. Defaults: host_id=local, sort_by=memory, limit=10.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"sort_by":{"type":"string","enum":["memory","cpu"],"description":"Sort descending by resident memory or lifetime-average CPU; default memory."},"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum number of processes; default 10."}},"additionalProperties":false}`),
		},
		{
			Name:        "search_processes",
			Description: "Search all visible process names by a literal fragment, ignoring case, independently of CPU/RSS rankings. Returns PID and name; no matches is a diagnostic result, not an execution error. Names may be limited to 15 characters: use a short fragment. Does not search command lines or environments. A matching process proves existence, not service health. Defaults: host_id=local, limit=100; at most 500 matches returned.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"pattern":{"type":"string","minLength":1,"maxLength":128,"description":"Literal process name fragment, case insensitive; at most 128 bytes, no NUL or newlines. Use a short fragment because names may be limited to 15 characters."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum matching processes returned; default 100."}},"required":["pattern"],"additionalProperties":false}`),
		},
		{
			Name: "get_service_status", Description: "Read systemd service or Docker container state. backend defaults to systemd; scope defaults to system. Use scope=user for user systemd services. Does not restart services.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"service":{"type":"string","description":"Systemd service name or Docker container name/ID."},"backend":{"type":"string","enum":["systemd","docker"]},"scope":{"type":"string","enum":["system","user"],"description":"Systemd only: system (default) or the executing user's service manager."}},"required":["service"],"additionalProperties":false}`),
		},
		{
			Name: "get_service_logs", Description: "Read recent journalctl or Docker container logs. backend defaults to systemd; scope defaults to system. Use scope=user for user systemd services; omit service for the selected journal. Defaults: 100 records from the last 60 minutes. No follow mode.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"service":{"type":"string"},"backend":{"type":"string","enum":["systemd","docker"]},"scope":{"type":"string","enum":["system","user"],"description":"Systemd only: system (default) or the executing user's journal."},"lines":{"type":"integer","minimum":1,"maximum":500},"since_minutes":{"type":"integer","minimum":1}},"additionalProperties":false}`),
		},
		{
			Name:        "read_file",
			Description: "Read a range of lines or the last lines of one regular file. Requires an absolute path; hidden components are forbidden in both the requested path and resolved symlink target. Sensitive /proc files (environ, cmdline, mem, fd, fdinfo) are forbidden. Defaults: host_id=local, start_line=1, lines=100. Set tail=true to read the end; do not supply start_line with tail=true. No follow mode.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"path":{"type":"string","description":"Absolute path to one regular file; hidden components, . and .. are forbidden."},"start_line":{"type":"integer","minimum":1,"description":"First line, numbered from 1; default 1. Cannot be supplied with tail=true."},"lines":{"type":"integer","minimum":1,"maximum":500,"description":"Number of lines; default 100."},"tail":{"type":"boolean","description":"Read the last lines instead of a range; default false."}},"required":["path"],"additionalProperties":false}`),
		},
		{
			Name:        "search_file",
			Description: "Search one regular file and return matching lines with line numbers. Requires an absolute path; hidden components are forbidden in both the requested path and resolved symlink target. Sensitive /proc files (environ, cmdline, mem, fd, fdinfo) are forbidden. Defaults: host_id=local, literal matching, case sensitive, limit=100. Set regex=true for grep extended regular expressions. No recursive search.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"path":{"type":"string","description":"Absolute path to one regular file; hidden components, . and .. are forbidden."},"pattern":{"type":"string","minLength":1,"description":"Nonempty literal text, or an extended regular expression when regex=true."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum number of matching lines; default 100."},"regex":{"type":"boolean","description":"Use extended regular expressions instead of literal matching; default false."},"ignore_case":{"type":"boolean","description":"Ignore case; default false."}},"required":["path","pattern"],"additionalProperties":false}`),
		},
		{
			Name:        "get_ip_rules",
			Description: "Show policy routing rules with ip rule show. Optional table filters rules referring to that routing table; it does not show routes inside the table. Defaults: host_id=local, family=ipv4. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["ipv4","ipv6"]},"table":{"type":"string","description":"Optional routing table name or positive decimal 32-bit ID without leading zeros."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_ip_routes",
			Description: "Show routes from main, default, local, a custom table, or all tables. The table named default is different from the default route in main. Defaults: host_id=local, family=ipv4, table=main. Optional interface filter. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["ipv4","ipv6"]},"table":{"type":"string","description":"Table name, positive decimal 32-bit ID without leading zeros, or all; default main."},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_route_to",
			Description: "Ask the kernel which route it would use for an IP destination. Optional source IP, packet mark (fwmark), input and output interfaces model policy routing. Address family follows destination; source must match it. This lookup sends no probe packets and does not prove reachability.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"destination":{"type":"string","description":"Required IPv4 or IPv6 address, without CIDR prefix or zone; no DNS hostname."},"source":{"type":"string","description":"Optional source IP of the same family."},"mark":{"type":"integer","minimum":0,"maximum":4294967295,"description":"Optional packet fwmark as an unsigned 32-bit decimal integer."},"input_interface":{"type":"string","description":"Optional incoming interface, for forwarded-packet lookup."},"output_interface":{"type":"string","description":"Optional outgoing interface constraint."}},"required":["destination"],"additionalProperties":false}`),
		},
		{
			Name:        "ping_host",
			Description: "Send a bounded ICMP echo probe to one IP address or DNS hostname from the selected host. Defaults: host_id=local, count=3, family=all (automatic); maximum 5 packets, 2-second reply timeout, 8-second deadline. Packet loss is a diagnostic result, not a command execution error. No flood mode. ICMP replies do not prove application health, and missing replies do not prove a host is down.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"destination":{"type":"string","description":"One IP address or ASCII DNS hostname; use interface for IPv6 link-local scope."},"family":{"type":"string","enum":["all","ipv4","ipv6"]},"interface":{"type":"string","description":"Optional outgoing interface name."},"count":{"type":"integer","minimum":1,"maximum":5,"description":"Number of echo requests; default 3."}},"required":["destination"],"additionalProperties":false}`),
		},
		{
			Name:        "get_network_sockets",
			Description: "Show TCP/UDP sockets with numeric endpoints and process details visible to the executing user. Defaults: host_id=local, protocol=both, state=listening, family=all. Filter by port on either endpoint. TCP connection states require protocol=tcp. Read-only; never closes sockets.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["all","ipv4","ipv6"]},"protocol":{"type":"string","enum":["both","tcp","udp"]},"state":{"type":"string","enum":["listening","all","established","syn-sent","syn-recv","time-wait","close-wait"]},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"Optional local or peer port."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_network_interfaces",
			Description: "Show interface IP addresses, link state, MTU and RX/TX counters using ip address show and ip -s link show. Defaults: host_id=local, family=all, all interfaces. Optional interface filter. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["all","ipv4","ipv6"]},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_ip_neighbors",
			Description: "Show neighbor cache entries and their states using ip neigh show. IPv4 shows ARP neighbors; IPv6 shows NDP neighbors. Defaults: host_id=local, family=ipv4, all interfaces. Optional interface filter. An empty cache alone does not prove a connectivity problem. Read-only.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"family":{"type":"string","enum":["ipv4","ipv6"]},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
		},
		{
			Name:        "dig_dns",
			Description: "Run a bounded dig DNS query from the selected host, retaining full output including DNS status. Requires one ASCII DNS name or IP; IP queries are reverse PTR lookups. Defaults: host_id=local, record_type=A for names or PTR for IPs, system DNS servers, port=53. Timeout 2 seconds per attempt, one try, no search suffixes or .digrc. Sends DNS requests without changing configuration; no zone transfers or arbitrary flags.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"query":{"type":"string","description":"One ASCII DNS name (including SRV underscores or a trailing dot), or IPv4/IPv6 address without a zone for reverse lookup."},"record_type":{"type":"string","enum":["A","AAAA","CNAME","MX","NS","TXT","SOA","SRV","PTR","CAA"],"description":"Default A for names; IP queries require PTR and default to it."},"server":{"type":"string","description":"Optional DNS server as an IPv4/IPv6 address without a zone; default system DNS servers."},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"DNS server port; default 53."}},"required":["query"],"additionalProperties":false}`),
		},
		{
			Name:        "nslookup_dns",
			Description: "Run a noninteractive bounded nslookup DNS query from the selected host. Requires one ASCII DNS name or IP; IP queries are reverse PTR lookups. Defaults: host_id=local, record_type=A for names or PTR for IPs, system DNS servers, port=53. Timeout 2 seconds, retry=1, no search suffixes. Nonzero exit codes retain diagnostic output, including negative DNS answers. Sends DNS requests without changing configuration; no zone transfers or arbitrary flags.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"query":{"type":"string","description":"One ASCII DNS name (including SRV underscores or a trailing dot), or IPv4/IPv6 address without a zone for reverse lookup."},"record_type":{"type":"string","enum":["A","AAAA","CNAME","MX","NS","TXT","SOA","SRV","PTR","CAA"],"description":"Default A for names; IP queries require PTR and default to it."},"server":{"type":"string","description":"Optional DNS server as an IPv4/IPv6 address without a zone; default system DNS servers."},"port":{"type":"integer","minimum":1,"maximum":65535,"description":"DNS server port; default 53."}},"required":["query"],"additionalProperties":false}`),
		},
		{
			Name:        "curl_http",
			Description: "Run a bounded HTTP/HTTPS GET or HEAD from one host, retaining status, response headers, server IP, timings and a body preview. Defaults: host_id=local, method=GET, verified TLS, no redirects; connection timeout 3s, transfer timeout 10s, max body 10 MiB. HTTP 4xx/5xx are diagnostic responses; inspect the HTTP status. Optional headers, follow_redirects (max 5), and insecure (only when explicitly requested by the user). download=true saves the raw body under the original URL filename in a new private /tmp/mini-sre-* directory on the selected host. No uploads, credentials in URLs, arbitrary flags or output paths; requires curl >= 8.4.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"url":{"type":"string","description":"Required absolute http:// or https:// URL, without embedded credentials."},"method":{"type":"string","enum":["GET","HEAD"],"description":"Default GET."},"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"Custom header name/value pairs; no CR/LF/NUL or header files."},"follow_redirects":{"type":"boolean","description":"Default false; follow up to five HTTP/HTTPS redirects."},"insecure":{"type":"boolean","description":"Default false; disable certificate verification only when explicitly requested by the user."},"download":{"type":"boolean","description":"GET only; save raw body to a private /tmp/mini-sre-* directory using a non-hidden filename from the original URL. Default false."}},"required":["url"],"additionalProperties":false}`),
		},
	}
	return append(append(specs, pipelineSpec(specs)), labSpecs()...)
}

func Registry(runner *remote.Runner) map[string]Func {
	t := &Tools{Runner: runner}
	registry := map[string]Func{}
	for _, spec := range Specs() {
		name := spec.Name
		registry[name] = func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args toolArgs
			var err error
			if name != "run_pipeline" && name != "get_vmstat" && !strings.HasPrefix(name, "lab_") {
				args, err = decodeArgs(raw)
				if err != nil {
					return "", err
				}
			}
			var out string
			switch name {
			case "list_hosts":
				out = strings.Join(runner.Hosts(), "\n")
			case "get_load_average":
				out, err = t.LoadAverage(ctx, args.HostID)
			case "get_vmstat":
				var vmstatArgs struct {
					HostID string `json:"host_id"`
				}
				err = decodeObject(raw, &vmstatArgs)
				if err == nil {
					out, err = t.VMStat(ctx, vmstatArgs.HostID)
				}
			case "get_memory":
				out, err = t.Memory(ctx, args.HostID)
			case "get_disk_usage":
				out, err = t.DiskUsage(ctx, args.HostID, args.Path)
			case "get_top_processes":
				out, err = t.TopProcesses(ctx, args)
			case "search_processes":
				out, err = t.SearchProcesses(ctx, args)
			case "get_service_status":
				out, err = t.ServiceStatus(ctx, args.HostID, args.ServiceTarget)
			case "get_service_logs":
				out, err = t.ServiceLogs(ctx, args)
			case "read_file":
				out, err = t.ReadFile(ctx, args)
			case "search_file":
				out, err = t.SearchFile(ctx, args)
			case "get_ip_rules":
				out, err = t.IPRules(ctx, args)
			case "get_ip_routes":
				out, err = t.IPRoutes(ctx, args)
			case "get_route_to":
				out, err = t.RouteTo(ctx, args)
			case "ping_host":
				out, err = t.PingHost(ctx, args)
			case "get_network_sockets":
				out, err = t.NetworkSockets(ctx, args)
			case "get_network_interfaces":
				out, err = t.NetworkInterfaces(ctx, args)
			case "get_ip_neighbors":
				out, err = t.IPNeighbors(ctx, args)
			case "dig_dns":
				out, err = t.DigDNS(ctx, args)
			case "nslookup_dns":
				out, err = t.NslookupDNS(ctx, args)
			case "run_pipeline":
				out, err = t.Pipeline(ctx, raw)
			case "curl_http":
				out, err = t.CurlHTTP(ctx, args)
			case "lab_deploy", "lab_inspect", "lab_run", "lab_destroy":
				out, err = t.Lab(ctx, name, raw)
			default:
				return "", errors.New("tools: no handler for tool")
			}
			if err != nil {
				return "", errors.New(runner.Redact(err.Error()))
			}
			return runner.Redact(out), nil
		}
	}
	return registry
}

type toolArgs struct {
	HostID string `json:"host_id"`
	Path   string `json:"path"`
	ServiceTarget
	Lines        *int   `json:"lines"`
	SinceMinutes *int   `json:"since_minutes"`
	SortBy       string `json:"sort_by"`
	Limit        *int   `json:"limit"`
	StartLine    *int   `json:"start_line"`
	Tail         bool   `json:"tail"`
	Pattern      string `json:"pattern"`
	Regex        bool   `json:"regex"`
	IgnoreCase   bool   `json:"ignore_case"`

	Family          string  `json:"family"`
	Interface       string  `json:"interface"`
	Table           string  `json:"table"`
	Destination     string  `json:"destination"`
	Source          string  `json:"source"`
	Mark            *uint32 `json:"mark"`
	InputInterface  string  `json:"input_interface"`
	OutputInterface string  `json:"output_interface"`
	Count           *int    `json:"count"`
	Protocol        string  `json:"protocol"`
	State           string  `json:"state"`
	Port            *int    `json:"port"`

	Query      string `json:"query"`
	RecordType string `json:"record_type"`
	Server     string `json:"server"`

	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Headers         map[string]string `json:"headers"`
	FollowRedirects bool              `json:"follow_redirects"`
	Insecure        bool              `json:"insecure"`
	Download        bool              `json:"download"`
}

func decodeArgs(raw json.RawMessage) (toolArgs, error) {
	var args toolArgs
	err := decodeObject(raw, &args)
	return args, err
}

func decodeObject(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return errors.New("tools: arguments must be a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return errors.New("tools: invalid arguments; check field names and types")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("tools: expected one argument object")
	}
	return nil
}

func (t *Tools) command(ctx context.Context, hostID, program string, args ...string) (remote.Result, error) {
	for _, arg := range args {
		if strings.ContainsRune(arg, '\x00') {
			return remote.Result{}, errors.New("tools: arguments must not contain NUL bytes")
		}
	}
	result, err := t.Runner.Run(ctx, hostID, program, args...)
	if err != nil {
		return result, fmt.Errorf("tools: %w; %s", err, commandOutput(result))
	}
	return result, nil
}

func commandOutput(result remote.Result) string {
	out := result.Stdout
	if result.Stderr != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += result.Stderr
	}
	if result.Truncated {
		out += "\n[output truncated at 64 KiB]"
	}
	return out
}
