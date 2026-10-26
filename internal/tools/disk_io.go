package tools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

func diskIODefinition() Definition {
	return Definition{
		Name:        "get_disk_io",
		Description: "Measure disk I/O by device with three one-second iostat samples, excluding the since-boot report. Reports read/write rates, request latency, queue length and utilization. Requires sysstat iostat with -d -x -k -y support. Use with vmstat and PSI to investigate current I/O stalls; high utilization alone does not establish saturation on SSD/RAID. Defaults: host_id=local. Read-only; no pipeline support.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"}},"additionalProperties":false}`),
		Run: func(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
			var args struct {
				HostID string `json:"host_id"`
			}
			if err := decodeObject(raw, &args); err != nil {
				return "", err
			}
			return t.diskIO(ctx, args.HostID)
		},
	}
}

func (t *Tools) diskIO(ctx context.Context, hostID string) (string, error) {
	result, err := t.command(ctx, hostID, "env", "S_COLORS=never", "iostat", "-d", "-x", "-k", "-y", "1", "3")
	if err != nil {
		return "", err
	}
	if result.Truncated {
		return "", errors.New("tools: iostat output was truncated; no complete interval report")
	}
	if err := validateDiskIO(result.Stdout); err != nil {
		return "", err
	}
	return "Disk I/O: 3 samples at 1-second intervals; since-boot report omitted. " +
		"Rates: KiB/s; await: milliseconds including queue time; aqu-sz/avgqu-sz: average queue length; %util: device busy time. " +
		"High %util alone does not establish SSD/RAID saturation. A short healthy sample does not exclude an earlier incident.\n" + commandOutput(result), nil
}

// iostat columns vary across sysstat releases. Validate the metrics we need,
// but retain each release's headers and additional numeric columns verbatim.
func validateDiskIO(raw string) error {
	reports, rows, columns := 0, 0, 0
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.TrimSuffix(fields[0], ":") == "Device" {
			if reports > 0 && rows == 0 {
				return errors.New("tools: iostat returned no device measurements")
			}
			present := make(map[string]bool, len(fields))
			for _, field := range fields {
				present[field] = true
			}
			if !present["r/s"] || !present["w/s"] || !present["rkB/s"] || !present["wkB/s"] || !present["%util"] ||
				(!present["aqu-sz"] && !present["avgqu-sz"]) || (!present["await"] && !(present["r_await"] && present["w_await"])) {
				return errors.New("tools: unsupported iostat columns; extended read/write rates, latency, queue and utilization are required")
			}
			reports++
			rows, columns = 0, len(fields)
			continue
		}
		if reports == 0 && strings.HasPrefix(line, "Linux ") {
			continue
		}
		if reports == 0 || len(fields) != columns {
			return errors.New("tools: incomplete or unsupported iostat row")
		}
		for _, field := range fields[1:] {
			value, err := strconv.ParseFloat(field, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return errors.New("tools: invalid iostat measurement")
			}
		}
		rows++
	}
	if reports != 3 || rows == 0 {
		return errors.New("tools: iostat did not return three interval samples with device measurements")
	}
	return nil
}
