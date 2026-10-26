package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type directoryUsageArgs struct {
	HostID string `json:"host_id"`
	Path   string `json:"path"`
	Limit  *int   `json:"limit"`
}

func directoryUsageDefinition() Definition {
	return Definition{
		Name:        "get_directory_usage",
		Description: "Find the largest immediate subdirectories by allocated bytes, including their descendants. Use only to investigate space exhaustion, on a specific absolute directory path. Recursively scans one filesystem without following nested symlinks; hidden entries are excluded. GNU du and timeout are required. The scan is bounded to 15 seconds but can create I/O load. Partial scans are errors with incomplete observations, never a complete ranking. Defaults: host_id=local, limit=10 (1..50). Read-only; no pipeline support.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"path":{"type":"string","description":"Required absolute directory path; hidden components are forbidden."},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["path"],"additionalProperties":false}`),
		Run: func(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
			var args directoryUsageArgs
			if err := decodeObject(raw, &args); err != nil {
				return "", err
			}
			return t.directoryUsage(ctx, args)
		},
	}
}

func (t *Tools) directoryUsage(ctx context.Context, args directoryUsageArgs) (string, error) {
	limit := 10
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 50 {
		return "", errors.New("tools: limit must be 1..50")
	}
	root, err := t.checkedDirectoryPath(ctx, args.HostID, args.Path)
	if err != nil {
		return "", err
	}
	result, runErr := t.Runner.RunBounded(ctx, args.HostID, 18*time.Second,
		"timeout", "--signal=TERM", "--kill-after=1s", "15s",
		"du", "--null", "--block-size=1", "--max-depth=1", "--one-file-system",
		"--no-dereference", "--exclude=.*", "--", root)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	entries, total, parseErr := parseDirectoryUsage(result.Stdout, root)
	complete := runErr == nil && !result.Truncated && parseErr == nil
	host := args.HostID
	if host == "" {
		host = "local"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Directory usage on %s: %s; complete=%t\n", host, sanitizeName(root), complete)
	out.WriteString("Allocated bytes; one filesystem; hidden entries excluded; nested symlinks not followed.\n")
	if total != nil {
		fmt.Fprintf(&out, "Directory total (includes immediate files): %d bytes (%s)\n", *total, humanBytes(*total))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].bytes == entries[j].bytes {
			return entries[i].name < entries[j].name
		}
		return entries[i].bytes > entries[j].bytes
	})
	if complete {
		out.WriteString("Largest immediate subdirectories:\n")
	} else {
		out.WriteString("Incomplete observations only; not a complete ranking or total:\n")
	}
	for _, entry := range entries[:min(limit, len(entries))] {
		fmt.Fprintf(&out, "%d bytes (%s)\t%s\n", entry.bytes, humanBytes(entry.bytes), sanitizeName(entry.name))
	}
	if len(entries) == 0 {
		out.WriteString("No subdirectory records returned.\n")
	}
	if len(entries) > limit {
		fmt.Fprintf(&out, "[showing %d of %d subdirectories]\n", limit, len(entries))
	}
	if !complete {
		if runErr != nil {
			fmt.Fprintf(&out, "Execution: %v\n", runErr)
		}
		if parseErr != nil {
			fmt.Fprintf(&out, "Output: %v\n", parseErr)
		}
		if result.Truncated {
			out.WriteString("[command output truncated at 64 KiB]\n")
		}
		if result.Stderr != "" {
			out.WriteString(result.Stderr)
		}
		// Registry and Session keep only the error when it is non-nil. Include
		// observations here, and keep scheduled checks from accepting an ok verdict.
		return "", fmt.Errorf("tools: directory usage incomplete\n%s", capToolResult(out.String()))
	}
	if result.Stderr != "" {
		fmt.Fprintf(&out, "Warnings: %s", result.Stderr)
	}
	return out.String(), nil
}

type directoryUsageEntry struct {
	name  string
	bytes uint64
}

func parseDirectoryUsage(raw, root string) ([]directoryUsageEntry, *uint64, error) {
	var entries []directoryUsageEntry
	var total *uint64
	seen := make(map[string]bool)
	for raw != "" {
		record, rest, ok := strings.Cut(raw, "\x00")
		if !ok {
			return entries, total, errors.New("unfinished du record")
		}
		raw = rest
		size, name, ok := strings.Cut(record, "\t")
		bytes, err := strconv.ParseUint(size, 10, 64)
		if !ok || err != nil || seen[name] || validateFilePath(name) != nil {
			return entries, total, errors.New("invalid du record")
		}
		seen[name] = true
		if name == root {
			total = &bytes
		} else if path.Dir(name) == root {
			entries = append(entries, directoryUsageEntry{name: path.Base(name), bytes: bytes})
		} else {
			return entries, total, errors.New("du returned a path outside the requested depth")
		}
	}
	if total == nil {
		return entries, nil, errors.New("du did not return the directory total")
	}
	return entries, total, nil
}
