package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mini-sre/internal/remote"
)

const (
	meminfoPath          = "/proc/meminfo"
	vmstatPath           = "/proc/vmstat"
	pressureCPUpath      = "/proc/pressure/cpu"
	pressureMemoryPath   = "/proc/pressure/memory"
	pressureIOPath       = "/proc/pressure/io"
	memoryReportUnitByte = 1024
)

type psiValues struct {
	avg10, avg60, avg300 float64
	total                uint64
}

type memorySample struct {
	host   string
	values map[string]uint64
	psi    map[string]map[string]psiValues
	psiErr string
	vm     map[string]uint64
	vmErr  string
}

func meminfoCommand() remote.Command {
	return remote.Command{Program: "cat", Args: []string{meminfoPath}}
}

func vmstatReadCommand() remote.Command {
	return remote.Command{Program: "cat", Args: []string{vmstatPath}}
}

func pressureCommand() remote.Command {
	return remote.Command{Program: "grep", Args: []string{"-H", ".", pressureCPUpath, pressureMemoryPath, pressureIOPath}}
}

func (t *Tools) Memory(ctx context.Context, hostID string) (string, error) {
	if hostID == "" {
		hostID = "local"
	}
	result, err := t.command(ctx, hostID, "cat", meminfoPath)
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", errors.New("tools: /proc/meminfo output was truncated")
	}
	values, err := parseMeminfo(result.Stdout)
	if err != nil {
		return "", err
	}

	sample := memorySample{host: hostID, values: values}
	vmResult, vmErr := t.Runner.Run(ctx, hostID, vmstatReadCommand().Program, vmstatReadCommand().Args...)
	if vmErr != nil || vmResult.Truncated {
		sample.vmErr = optionalError(vmResult, vmErr)
	} else {
		sample.vm = parseVMStat(vmResult.Stdout)
		if len(sample.vm) == 0 {
			sample.vmErr = "no relevant counters found"
		}
	}

	psiResult, psiErr := t.Runner.Run(ctx, hostID, pressureCommand().Program, pressureCommand().Args...)
	sample.psi = parsePSI(psiResult.Stdout)
	if psiResult.Truncated {
		sample.psiErr = "output truncated"
	} else if len(sample.psi) == 0 {
		sample.psiErr = optionalError(psiResult, psiErr)
	} else if psiErr != nil && strings.TrimSpace(psiResult.Stderr) != "" {
		sample.psiErr = strings.TrimSpace(psiResult.Stderr)
	}

	return formatMemoryReport(sample), nil
}

func optionalError(result remote.Result, err error) string {
	if text := strings.TrimSpace(result.Stderr); text != "" {
		return text
	}
	if result.Truncated {
		return "output truncated"
	}
	if err != nil {
		return err.Error()
	}
	return "unavailable"
}

func parseMeminfo(raw string) (map[string]uint64, error) {
	values := make(map[string]uint64)
	for _, line := range strings.Split(raw, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[strings.TrimSpace(key)] = value
	}
	if values["MemTotal"] == 0 {
		return nil, errors.New("tools: /proc/meminfo has no valid MemTotal")
	}
	return values, nil
}

func parseVMStat(raw string) map[string]uint64 {
	counters := map[string]uint64{
		"oom_kill":       0,
		"pswpout":        0,
		"pgmajfault":     0,
		"pgscan_direct":  0,
		"pgsteal_direct": 0,
	}
	present := make(map[string]uint64)
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, ok := counters[fields[0]]; !ok {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		present[fields[0]] = value
	}
	return present
}

func parsePSI(raw string) map[string]map[string]psiValues {
	values := make(map[string]map[string]psiValues)
	for _, line := range strings.Split(raw, "\n") {
		path, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		resource := path
		if index := strings.LastIndex(path, "/"); index >= 0 {
			resource = path[index+1:]
		}
		switch resource {
		case "cpu", "memory", "io":
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 5 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		var value psiValues
		for _, field := range fields[1:] {
			key, number, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "avg10":
				value.avg10, _ = strconv.ParseFloat(number, 64)
			case "avg60":
				value.avg60, _ = strconv.ParseFloat(number, 64)
			case "avg300":
				value.avg300, _ = strconv.ParseFloat(number, 64)
			case "total":
				value.total, _ = strconv.ParseUint(number, 10, 64)
			}
		}
		if values[resource] == nil {
			values[resource] = make(map[string]psiValues)
		}
		values[resource][kind] = value
	}
	return values
}

func formatMemoryReport(sample memorySample) string {
	values := sample.values
	total := values["MemTotal"]
	available, exists := values["MemAvailable"]
	estimated := false
	if !exists {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
		estimated = true
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Memory on %s:\n", sample.host)
	fmt.Fprintf(&out, "MemTotal: %s\n", humanBytes(total*memoryReportUnitByte))
	fmt.Fprintf(&out, "MemFree: %s\n", humanBytes(values["MemFree"]*memoryReportUnitByte))
	fmt.Fprintf(&out, "MemAvailable: %s (%.1f%% of MemTotal)", humanBytes(available*memoryReportUnitByte), percentOf(available, total))
	if estimated {
		out.WriteString(" [fallback estimate: MemFree+Buffers+Cached]")
	}
	out.WriteString("\n")

	appendMemoryFields(&out, "Page cache:", values, "Buffers", "Cached", "SReclaimable", "Shmem")
	appendMemoryFields(&out, "Process memory:", values, "AnonPages", "Mapped")
	appendMemoryFields(&out, "Kernel/VM:", values, "Slab", "PageTables", "Unevictable", "Dirty", "Writeback")

	if committed, ok := values["Committed_AS"]; ok {
		if limit, hasLimit := values["CommitLimit"]; hasLimit && limit > 0 {
			fmt.Fprintf(&out, "Committed_AS: %s (%.1f%% of CommitLimit %s)\n", humanBytes(committed*memoryReportUnitByte), percentOf(committed, limit), humanBytes(limit*memoryReportUnitByte))
		} else {
			fmt.Fprintf(&out, "Committed_AS: %s (%.1f%% of MemTotal)\n", humanBytes(committed*memoryReportUnitByte), percentOf(committed, total))
		}
	}

	swapTotal := values["SwapTotal"]
	swapFree := values["SwapFree"]
	if swapTotal == 0 {
		out.WriteString("Swap: not configured\n")
	} else {
		swapUsed := uint64(0)
		if swapTotal > swapFree {
			swapUsed = swapTotal - swapFree
		}
		fmt.Fprintf(&out, "Swap: total=%s used=%s free=%s (%.1f%% used)\n",
			humanBytes(swapTotal*memoryReportUnitByte), humanBytes(swapUsed*memoryReportUnitByte),
			humanBytes(swapFree*memoryReportUnitByte), percentOf(swapUsed, swapTotal))
	}

	if len(sample.psi) > 0 {
		out.WriteString("PSI stall time (percentage of wall time over 10/60/300 seconds):\n")
		for _, resource := range []string{"cpu", "memory", "io"} {
			kinds := sample.psi[resource]
			for _, kind := range []string{"some", "full"} {
				value, ok := kinds[kind]
				if !ok {
					continue
				}
				fmt.Fprintf(&out, "  %s %s: avg10=%.2f%% avg60=%.2f%% avg300=%.2f%% total=%.1fs\n",
					resource, kind, value.avg10, value.avg60, value.avg300, float64(value.total)/1e6)
			}
		}
		if sample.psiErr != "" {
			fmt.Fprintf(&out, "  PSI limitations: %s\n", sample.psiErr)
		}
	} else {
		out.WriteString("PSI: unavailable")
		if sample.psiErr != "" {
			fmt.Fprintf(&out, ": %s", sample.psiErr)
		}
		out.WriteString("\n")
	}

	if len(sample.vm) > 0 {
		out.WriteString("OOM and reclaim counters (cumulative since boot):\n")
		for _, key := range []string{"oom_kill", "pswpout", "pgmajfault", "pgscan_direct", "pgsteal_direct"} {
			if value, ok := sample.vm[key]; ok {
				fmt.Fprintf(&out, "  %s=%d\n", key, value)
			}
		}
		if sample.vmErr != "" {
			fmt.Fprintf(&out, "  Counter limitations: %s\n", sample.vmErr)
		}
	} else {
		out.WriteString("OOM and reclaim counters: unavailable")
		if sample.vmErr != "" {
			fmt.Fprintf(&out, ": %s", sample.vmErr)
		}
		out.WriteString("\n")
	}

	out.WriteString("Notes: MemAvailable is an estimate. Used swap does not by itself mean active swapping. Committed_AS is not an enforcement limit under heuristic overcommit. PSI values are averages; counters are cumulative. These readings do not prove application health.")
	return out.String()
}

func appendMemoryFields(out *strings.Builder, prefix string, values map[string]uint64, keys ...string) {
	wrote := false
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		if !wrote {
			out.WriteString(prefix)
			wrote = true
		}
		fmt.Fprintf(out, " %s=%s", key, humanBytes(value*memoryReportUnitByte))
	}
	if wrote {
		out.WriteString("\n")
	}
}

func percentOf(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
