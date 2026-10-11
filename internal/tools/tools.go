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
	return []Spec{
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
			Name:        "get_disk_usage",
			Description: "Get disk usage (total, used, available, percent used) for a filesystem path.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"path":{"type":"string","description":"Filesystem path to inspect; default /."}},"additionalProperties":false}`),
		},
		{
			Name:        "get_top_processes",
			Description: "List visible processes sorted by memory (RSS) or CPU, descending. Shows PID, user, CPU%, MEM%, RSS in KiB and executable name, without command-line arguments. CPU% is a lifetime average, not an interval measurement, and can exceed 100% on multiple cores. Defaults: host_id=local, sort_by=memory, limit=10.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","description":"Host ID from list_hosts; default local."},"sort_by":{"type":"string","enum":["memory","cpu"],"description":"Sort descending by resident memory or lifetime-average CPU; default memory."},"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum number of processes; default 10."}},"additionalProperties":false}`),
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
			Description: "Read a range of lines or the last lines of one regular file. Requires an absolute path; hidden components are forbidden in both the requested path and resolved symlink target. Defaults: host_id=local, start_line=1, lines=100. Set tail=true to read the end; do not supply start_line with tail=true. No follow mode.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"path":{"type":"string","description":"Absolute path to one regular file; hidden components, . and .. are forbidden."},"start_line":{"type":"integer","minimum":1,"description":"First line, numbered from 1; default 1. Cannot be supplied with tail=true."},"lines":{"type":"integer","minimum":1,"maximum":500,"description":"Number of lines; default 100."},"tail":{"type":"boolean","description":"Read the last lines instead of a range; default false."}},"required":["path"],"additionalProperties":false}`),
		},
		{
			Name:        "search_file",
			Description: "Search one regular file and return matching lines with line numbers. Requires an absolute path; hidden components are forbidden in both the requested path and resolved symlink target. Defaults: host_id=local, literal matching, case sensitive, limit=100. Set regex=true for grep extended regular expressions. No recursive search.",
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
	}
}

func Registry(runner *remote.Runner) map[string]Func {
	t := &Tools{Runner: runner}
	registry := map[string]Func{}
	for _, spec := range Specs() {
		name := spec.Name
		registry[name] = func(ctx context.Context, raw json.RawMessage) (string, error) {
			args, err := decodeArgs(raw)
			if err != nil {
				return "", err
			}
			var out string
			switch name {
			case "list_hosts":
				out = strings.Join(runner.Hosts(), "\n")
			case "get_load_average":
				out, err = t.LoadAverage(ctx, args.HostID)
			case "get_disk_usage":
				out, err = t.DiskUsage(ctx, args.HostID, args.Path)
			case "get_top_processes":
				out, err = t.TopProcesses(ctx, args)
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
}

func decodeArgs(raw json.RawMessage) (toolArgs, error) {
	var args toolArgs
	if len(raw) == 0 {
		return args, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return args, errors.New("tools: arguments must be a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return args, errors.New("tools: invalid arguments; check field names and types")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return args, errors.New("tools: expected one argument object")
	}
	return args, nil
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
