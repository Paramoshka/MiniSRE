package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"mini-sre/internal/lab"
)

func labSpecs() []Spec {
	return []Spec{
		{Name: "lab_deploy", Description: "Create and configure one bounded containerlab lab, only when the user requests creation. Requires explicitly configured host_id. topo_yaml is restricted YAML: name; nodes mapping (profile=frr|alpine, interfaces mapping lo or linked eth1..eth99 to IPv4 CIDR lists, routes list with destination CIDR/via IPv4/interface, forwarding boolean, frr_config text for BGP/OSPF); links list of [node:eth1,node:eth1] pairs. No containerlab passthrough fields or shell commands. Name gets msre-lab- prefix. At most 8 nodes, 256 MiB and 0.5 CPU each; one active lab per host. TTL defaults to 60 minutes. Uses preloaded approved images and reserved management network; validates host conflicts before deployment. Config syntax supports frr defaults, hostname, service integrated-vtysh-config, router bgp/ospf, router IDs, neighbor remote-as/activate, address-family ipv4 unicast, network (optionally area), redistribute connected/static, passive-interface, ip ospf area, no bgp ebgp-requires-policy/network import-check, exit and comments.", Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","minLength":1},"topo_yaml":{"type":"string","minLength":1,"maxLength":262144},"ttl_minutes":{"type":"integer","minimum":5,"maximum":120}},"required":["host_id","topo_yaml"],"additionalProperties":false}`)},
		{Name: "lab_inspect", Description: "Inspect containerlab labs on an explicitly configured lab host. Optional name selects one lab. Returns JSON with nodes, management IPs, states and links/TTL for helper-managed labs. Does not deploy or change labs.", Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1}},"required":["host_id"],"additionalProperties":false}`)},
		{Name: "lab_run", Description: "Run 1..4 bounded diagnostics inside one containerlab node discovered through lab_inspect. Requires host_id, lab name and node name. Checks: interfaces, routes, neighbors, bgp, ospf, ping (3 packets), traceroute (8 hops), tcpdump (20 packets, timeout 14 seconds). ping/traceroute require an IPv4 destination; ping/tcpdump optionally accept interface. Each result includes exit_code, timed_out, truncated, stdout/stderr. A completed execution is not proof of healthy connectivity. No arbitrary commands, packet filters or host/shared network namespaces.", Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1},"node":{"type":"string","minLength":1},"checks":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{"type":{"type":"string","enum":["interfaces","routes","neighbors","bgp","ospf","ping","traceroute","tcpdump"]},"destination":{"type":"string"},"interface":{"type":"string"}},"required":["type"],"additionalProperties":false}}},"required":["host_id","name","node","checks"],"additionalProperties":false}`)},
		{Name: "lab_destroy", Description: "Remove only labs registered to the host helper, when the user explicitly requests deletion. Supply name OR all_mine=true. Never removes other containerlab labs. TTL cleanup works independently on the lab host.", Parameters: json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1},"all_mine":{"type":"boolean"}},"required":["host_id"],"additionalProperties":false}`)},
	}
}

type labArgs struct {
	HostID     string      `json:"host_id"`
	Topology   string      `json:"topo_yaml"`
	TTLMinutes *int        `json:"ttl_minutes"`
	Name       string      `json:"name"`
	AllMine    bool        `json:"all_mine"`
	Node       string      `json:"node"`
	Checks     []lab.Check `json:"checks"`
}

func (t *Tools) Lab(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var args labArgs
	if err := decodeObject(raw, &args); err != nil {
		return "", err
	}
	if args.HostID == "" || !slices.Contains(t.Runner.Config.LabHosts, args.HostID) {
		return "", errors.New("tools: host_id is required and must be enabled in lab_hosts")
	}
	request := lab.Request{Action: strings.TrimPrefix(name, "lab_"), Topology: args.Topology, Name: args.Name, AllMine: args.AllMine, Node: args.Node, Checks: args.Checks}
	if args.TTLMinutes != nil {
		if *args.TTLMinutes < 5 || *args.TTLMinutes > 120 {
			return "", errors.New("tools: ttl_minutes must be 5..120")
		}
		request.TTLMinutes = *args.TTLMinutes
	}
	if err := lab.ValidateRequest(request); err != nil {
		return "", err
	}
	if request.Action == "deploy" {
		if _, err := lab.ParseTopology(request.Topology); err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	result, err := t.Runner.RunLab(ctx, args.HostID, data)
	if err != nil {
		return "", fmt.Errorf("tools: %w; %s", err, commandOutput(result))
	}
	if result.Truncated || !json.Valid([]byte(result.Stdout)) {
		return "", errors.New("tools: lab helper returned truncated or invalid JSON")
	}
	return result.Stdout, nil
}
