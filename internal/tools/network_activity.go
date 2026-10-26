package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Both snapshots and their monotonic timestamps come from the probe host.
// Arguments are an optional, validated "dev NAME", never shell source.
const networkActivityScript = `set -eu
snapshot() {
  read -r stamp rest < /proc/uptime
  printf '%s\n' "$stamp"
  ip -j -s link show "$@"
}
snapshot "$@"
sleep 1
snapshot "$@"
`

type networkActivityArgs struct {
	HostID    string `json:"host_id"`
	Interface string `json:"interface"`
}

func networkActivityDefinition() Definition {
	return Definition{
		Name:        "get_network_activity",
		Description: "Measure RX/TX byte rates and changes in packet/error/drop counters over about one second, using two ip -j -s link snapshots on the same host. Actual elapsed time comes from /proc/uptime. Optional interface filter; default all interfaces, host_id=local. Requires iproute2 JSON statistics, Bash, sleep and timeout. Missing/reset counters or changed interfaces mean comparison unavailable. No traffic is not proof of network health; drops alone do not identify the cause of application latency. Read-only, no generated test traffic or pipeline support.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"interface":{"type":"string","description":"Optional interface name."}},"additionalProperties":false}`),
		Run: func(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
			var args networkActivityArgs
			if err := decodeObject(raw, &args); err != nil {
				return "", err
			}
			return t.networkActivity(ctx, args)
		},
	}
}

func (t *Tools) networkActivity(ctx context.Context, args networkActivityArgs) (string, error) {
	if err := validateInterface(args.Interface); err != nil {
		return "", err
	}
	flags := []string{"--signal=TERM", "--kill-after=1s", "7s", "bash", "--noprofile", "--norc", "-c", networkActivityScript, "mini-sre-network-activity"}
	if args.Interface != "" {
		flags = append(flags, "dev", args.Interface)
	}
	result, err := t.Runner.RunBounded(ctx, args.HostID, 10*time.Second, "timeout", flags...)
	if err != nil {
		return "", fmt.Errorf("tools: network activity sampling failed: %w; %s", err, commandOutput(result))
	}
	if result.Truncated {
		return "", errors.New("tools: network activity snapshots were truncated; comparison unavailable")
	}
	first, second, elapsed, err := parseNetworkActivity(result.Stdout)
	if err != nil {
		return "", err
	}
	host := args.HostID
	if host == "" {
		host = "local"
	}
	out, complete := formatNetworkActivity(host, first, second, elapsed)
	if result.Stderr != "" {
		out += "Warnings: " + result.Stderr
	}
	if !complete {
		return "", fmt.Errorf("tools: network activity comparison incomplete\n%s", capToolResult(out))
	}
	return out, nil
}

type networkCounters struct {
	Bytes   *uint64 `json:"bytes"`
	Packets *uint64 `json:"packets"`
	Errors  *uint64 `json:"errors"`
	Dropped *uint64 `json:"dropped"`
}

type networkLinkStats struct {
	RX networkCounters `json:"rx"`
	TX networkCounters `json:"tx"`
}

type networkLink struct {
	Index   int               `json:"ifindex"`
	Name    string            `json:"ifname"`
	Stats64 *networkLinkStats `json:"stats64"`
	Stats   *networkLinkStats `json:"stats"`
}

type networkLinkID struct {
	index int
	name  string
}

func parseNetworkActivity(raw string) (map[networkLinkID]networkLink, map[networkLinkID]networkLink, float64, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	var snapshots [2]map[networkLinkID]networkLink
	var stamps [2]float64
	for i := range snapshots {
		var links []networkLink
		var stamp *float64
		if err := dec.Decode(&stamp); err != nil || stamp == nil || math.IsNaN(*stamp) || math.IsInf(*stamp, 0) || *stamp < 0 {
			return nil, nil, 0, errors.New("tools: invalid network sample timestamp")
		}
		stamps[i] = *stamp
		if err := dec.Decode(&links); err != nil || links == nil {
			return nil, nil, 0, errors.New("tools: incomplete or unsupported ip JSON statistics")
		}
		snapshots[i] = make(map[networkLinkID]networkLink, len(links))
		indices, names := make(map[int]bool), make(map[string]bool)
		for _, link := range links {
			if link.Index <= 0 || link.Name == "" || indices[link.Index] || names[link.Name] {
				return nil, nil, 0, errors.New("tools: invalid or duplicate network interface identity")
			}
			indices[link.Index], names[link.Name] = true, true
			snapshots[i][networkLinkID{link.Index, link.Name}] = link
		}
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || stamps[1] <= stamps[0] {
		return nil, nil, 0, errors.New("tools: invalid network sample interval or trailing data")
	}
	return snapshots[0], snapshots[1], stamps[1] - stamps[0], nil
}

func (link networkLink) counters() ([8]uint64, bool) {
	var values [8]uint64
	stats := link.Stats64
	if stats == nil {
		stats = link.Stats
	}
	if stats == nil {
		return values, false
	}
	for i, counter := range []*uint64{stats.RX.Bytes, stats.RX.Packets, stats.RX.Errors, stats.RX.Dropped, stats.TX.Bytes, stats.TX.Packets, stats.TX.Errors, stats.TX.Dropped} {
		if counter == nil {
			return values, false
		}
		values[i] = *counter
	}
	return values, true
}

func formatNetworkActivity(host string, first, second map[networkLinkID]networkLink, elapsed float64) (string, bool) {
	ids := make([]networkLinkID, 0, len(first)+len(second))
	for id := range first {
		ids = append(ids, id)
	}
	for id := range second {
		if _, exists := first[id]; !exists {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].name == ids[j].name {
			return ids[i].index < ids[j].index
		}
		return ids[i].name < ids[j].name
	})
	var out strings.Builder
	fmt.Fprintf(&out, "Network activity on %s: elapsed=%.3fs (probe-host uptime).\n", host, elapsed)
	out.WriteString("RX/TX: bytes/s and interval changes in packets, errors and drops. No test traffic generated.\n")
	complete := len(ids) > 0
	if len(ids) == 0 {
		out.WriteString("No interface measurements; comparison unavailable.\n")
	}
	for _, id := range ids {
		before, hadBefore := first[id]
		after, hasAfter := second[id]
		fmt.Fprintf(&out, "%s (ifindex=%d): ", sanitizeName(id.name), id.index)
		a, validA := before.counters()
		b, validB := after.counters()
		if !hadBefore || !hasAfter || !validA || !validB {
			out.WriteString("comparison unavailable (interface changed or counters missing).\n")
			complete = false
			continue
		}
		reset := false
		for i := range a {
			if b[i] < a[i] {
				reset = true
				break
			}
			b[i] -= a[i]
		}
		if reset {
			out.WriteString("comparison unavailable (counter reset or wrapped).\n")
			complete = false
			continue
		}
		fmt.Fprintf(&out, "RX bytes/s=%.1f packets=%d errors=%d drops=%d; TX bytes/s=%.1f packets=%d errors=%d drops=%d\n",
			float64(b[0])/elapsed, b[1], b[2], b[3], float64(b[4])/elapsed, b[5], b[6], b[7])
	}
	out.WriteString("No traffic or zero errors do not prove network health; error/drop growth alone does not establish the cause of application latency.\n")
	return out.String(), complete
}
