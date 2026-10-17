package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"mini-sre/internal/remote"
)

type pipelineArgs struct {
	HostID string `json:"host_id"`
	Source struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	} `json:"source"`
	Filters []pipelineFilter `json:"filters"`
	Stdout  string           `json:"stdout"`
	Stderr  string           `json:"stderr"`
}

type pipelineFilter struct {
	Type       string `json:"type"`
	Pattern    string `json:"pattern"`
	Regex      bool   `json:"regex"`
	IgnoreCase bool   `json:"ignore_case"`
	Invert     bool   `json:"invert"`
	Limit      *int   `json:"limit"`
	Lines      *int   `json:"lines"`
	StartLine  *int   `json:"start_line"`
}

func (t *Tools) Pipeline(ctx context.Context, raw json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var args pipelineArgs
	if err := decodeObject(raw, &args); err != nil {
		return "", err
	}
	if len(args.Filters) > 4 {
		return "", errors.New("tools: at most four pipeline filters are allowed")
	}
	plan := remote.Pipeline{Stdout: args.Stdout, Stderr: args.Stderr}
	if plan.Stdout == "" {
		plan.Stdout = "inline"
	}
	if plan.Stderr == "" {
		plan.Stderr = "inline"
	}
	if plan.Stdout != "inline" && plan.Stdout != "file" && plan.Stdout != "discard" {
		return "", errors.New("tools: invalid stdout mode")
	}
	if plan.Stderr != "inline" && plan.Stderr != "file" && plan.Stderr != "discard" && plan.Stderr != "merge" {
		return "", errors.New("tools: invalid stderr mode")
	}
	// Validate filters before source preparation can run filesystem checks.
	for _, filter := range args.Filters {
		stage, err := filterStage(filter)
		if err != nil {
			return "", err
		}
		plan.Stages = append(plan.Stages, stage)
	}
	sourceArgs, err := pipelineSourceArgs(args.Source.Tool, args.Source.Args)
	if err != nil {
		return "", err
	}
	sourceArgs.HostID = args.HostID
	source, err := t.sourceStage(ctx, args.Source.Tool, sourceArgs)
	if err != nil {
		return "", err
	}
	plan.Stages = append([]remote.Stage{source}, plan.Stages...)
	result, err := t.Runner.RunPipeline(ctx, args.HostID, plan)
	if args.Source.Tool == "curl_http" && len(result.Codes) > 0 && result.Codes[0] == 63 {
		result.Limited = true
	}
	out := pipelineOutput(args.HostID, args.Source.Tool, args.Filters, result)
	if args.Source.Tool == "curl_http" && sourceArgs.Insecure {
		out = "TLS certificate verification disabled (insecure=true).\n" + out
	}
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	for i, code := range result.Codes {
		if !pipelineStageSucceeded(args.Source.Tool, args.Filters, i, code) {
			return "", fmt.Errorf("tools: pipeline stage %d failed with exit code %d\n%s", i, code, out)
		}
	}
	return out, nil
}

func pipelineStageSucceeded(source string, filters []pipelineFilter, index, code int) bool {
	if code == 0 {
		return true
	}
	if index == 0 {
		for _, def := range definitions() {
			if def.Name == source && def.Pipeline != nil && def.Pipeline.ExitOK != nil {
				return def.Pipeline.ExitOK(code)
			}
		}
		return false
	}
	return code == 1 && filters[index-1].Type == "grep"
}

func pipelineSourceArgs(name string, raw json.RawMessage) (toolArgs, error) {
	var args toolArgs
	for _, def := range definitions() {
		if def.Name != name {
			continue
		}
		if def.Pipeline == nil {
			return args, errors.New("tools: invalid pipeline source")
		}
		properties, err := schemaProperties(def.Parameters)
		if err != nil {
			return args, err
		}
		var fields map[string]json.RawMessage
		if len(raw) != 0 {
			if err := decodeObject(raw, &fields); err != nil {
				return args, err
			}
		}
		for key := range fields {
			if _, ok := properties[key]; !ok || key == "host_id" {
				return args, errors.New("tools: invalid source argument; host_id belongs to the pipeline")
			}
		}
		return decodeArgs(raw)
	}
	return args, errors.New("tools: unknown pipeline source")
}

func (t *Tools) sourceStage(ctx context.Context, name string, args toolArgs) (remote.Stage, error) {
	for _, def := range definitions() {
		if def.Name != name {
			continue
		}
		if def.Pipeline == nil {
			return remote.Stage{}, errors.New("tools: unsupported pipeline source")
		}
		return def.Pipeline.Stage(ctx, t, args)
	}
	return remote.Stage{}, errors.New("tools: unsupported pipeline source")
}

func filterStage(filter pipelineFilter) (remote.Stage, error) {
	var cmd remote.Command
	switch filter.Type {
	case "grep":
		if filter.Pattern == "" || strings.ContainsRune(filter.Pattern, 0) || filter.Lines != nil || filter.StartLine != nil {
			return remote.Stage{}, errors.New("tools: grep requires a nonempty pattern; lines and start_line are not supported")
		}
		limit := 100
		if filter.Limit != nil {
			limit = *filter.Limit
		}
		if limit < 1 || limit > 500 {
			return remote.Stage{}, errors.New("tools: grep limit must be 1..500")
		}
		mode := "-F"
		if filter.Regex {
			mode = "-E"
		}
		cmd = remote.Command{Program: "grep", Args: []string{mode, "-a", "--color=never", "-m", strconv.Itoa(limit)}}
		if filter.IgnoreCase {
			cmd.Args = append(cmd.Args, "-i")
		}
		if filter.Invert {
			cmd.Args = append(cmd.Args, "-v")
		}
		cmd.Args = append(cmd.Args, "-e", filter.Pattern)
	case "count":
		if filter.Pattern != "" || filter.Regex || filter.Invert || filter.Lines != nil || filter.StartLine != nil {
			return remote.Stage{}, errors.New("tools: count does not accept pattern, regex, invert or line-range arguments")
		}
		limit := 20
		if filter.Limit != nil {
			limit = *filter.Limit
		}
		if limit < 1 || limit > 500 {
			return remote.Stage{}, errors.New("tools: count limit must be 1..500")
		}
		sortCommand := remote.Command{Program: "sort"}
		uniqCommand := remote.Command{Program: "uniq", Args: []string{"-c"}}
		if filter.IgnoreCase {
			sortCommand.Args = append(sortCommand.Args, "-f")
			uniqCommand.Args = append(uniqCommand.Args, "-i")
		}
		return remote.Stage{Pipe: []remote.Command{
			sortCommand,
			uniqCommand,
			{Program: "sort", Args: []string{"-rn"}},
			{Program: "head", Args: []string{"-n", strconv.Itoa(limit)}},
		}, Drain: true}, nil
	case "head", "tail", "sed":
		if filter.Pattern != "" || filter.Regex || filter.IgnoreCase || filter.Invert || filter.Limit != nil || (filter.Type != "sed" && filter.StartLine != nil) {
			return remote.Stage{}, errors.New("tools: invalid line-filter arguments")
		}
		lines, start := 100, 1
		if filter.Lines != nil {
			lines = *filter.Lines
		}
		if filter.StartLine != nil {
			start = *filter.StartLine
		}
		if lines < 1 || lines > 500 || start < 1 || start > math.MaxInt-(lines-1) {
			return remote.Stage{}, errors.New("tools: invalid line range; lines must be 1..500")
		}
		cmd = remote.Command{Program: filter.Type, Args: []string{"-n", strconv.Itoa(lines)}}
		if filter.Type == "sed" {
			cmd.Args = []string{"--sandbox", "-n", fmt.Sprintf("%d,%dp;%dq", start, start+lines-1, start+lines-1)}
		}
	default:
		return remote.Stage{}, errors.New("tools: filter must be grep, head, tail, sed or count")
	}
	return remote.Stage{Commands: []remote.Command{cmd}, Drain: true}, nil
}

func pipelineOutput(host, source string, filters []pipelineFilter, result remote.PipelineResult) string {
	if host == "" {
		host = "local"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "host=%s execution_complete=%t limited=%t\n", host, result.Complete, result.Limited)
	for i, code := range result.Codes {
		name := source
		if i > 0 {
			name = filters[i-1].Type
		}
		fmt.Fprintf(&out, "stage[%d]=%s exit_code=%d\n", i, name, code)
	}
	if result.HTTPMetadata != "" {
		fmt.Fprintf(&out, "HTTP metadata (status, server IP, seconds, effective URL):\n%s", result.HTTPMetadata)
	}
	if result.HTTPHeaders != "" {
		fmt.Fprintf(&out, "HTTP headers:\n%s", result.HTTPHeaders)
	}
	if len(result.HTTPMetadata) >= 4096 {
		out.WriteString("\n[HTTP metadata preview limited to 4096 bytes]\n")
	}
	if len(result.HTTPHeaders) >= 8192 {
		out.WriteString("\n[HTTP headers preview limited to 8192 bytes]\n")
	}
	partial := !result.Complete || result.Limited
	for i, code := range result.Codes {
		if !pipelineStageSucceeded(source, filters, i, code) {
			partial = true
		}
	}
	for _, stream := range []struct {
		name, text, path string
		size             int64
		truncated        bool
	}{
		{"stdout", result.Stdout, result.StdoutPath, result.StdoutBytes, result.StdoutTruncated},
		{"stderr", result.Stderr, result.StderrPath, result.StderrBytes, result.StderrTruncated},
	} {
		if stream.path != "" {
			size := strconv.FormatInt(stream.size, 10)
			if !result.Complete {
				size = "unknown"
			}
			fmt.Fprintf(&out, "%s_file=%s bytes=%s (raw output; possibly_partial=%t)\n", stream.name, stream.path, size, partial)
		}
		if stream.text != "" {
			fmt.Fprintf(&out, "%s:\n%s\n", stream.name, stream.text)
		}
		if stream.truncated {
			fmt.Fprintf(&out, "[%s preview truncated; total %d bytes]\n", stream.name, stream.size)
		}
	}
	return out.String()
}
