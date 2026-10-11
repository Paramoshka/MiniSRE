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
	out := pipelineOutput(args.HostID, args.Source.Tool, args.Filters, result)
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	for i, code := range result.Codes {
		allowOne := i == 0 && (args.Source.Tool == "ping_host" || args.Source.Tool == "search_file") ||
			i > 0 && args.Filters[i-1].Type == "grep"
		if code != 0 && !(code == 1 && allowOne) {
			return "", fmt.Errorf("tools: pipeline stage %d failed with exit code %d\n%s", i, code, out)
		}
	}
	return out, nil
}

func pipelineSourceArgs(name string, raw json.RawMessage) (toolArgs, error) {
	var args toolArgs
	if name == "list_hosts" || name == "run_pipeline" {
		return args, errors.New("tools: invalid pipeline source")
	}
	for _, spec := range Specs() {
		if spec.Name != name {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			return args, err
		}
		var fields map[string]json.RawMessage
		if len(raw) != 0 {
			if err := decodeObject(raw, &fields); err != nil {
				return args, err
			}
		}
		for key := range fields {
			if _, ok := schema.Properties[key]; !ok || key == "host_id" {
				return args, errors.New("tools: invalid source argument; host_id belongs to the pipeline")
			}
		}
		return decodeArgs(raw)
	}
	return args, errors.New("tools: unknown pipeline source")
}

func (t *Tools) sourceStage(ctx context.Context, name string, args toolArgs) (remote.Stage, error) {
	var cmd remote.Command
	var err error
	stage := remote.Stage{}
	switch name {
	case "get_load_average":
		cmd = loadCommand()
	case "get_disk_usage":
		cmd = diskCommand(args.Path)
	case "get_top_processes":
		var limit int
		cmd, _, limit, err = processCommand(args)
		stage.HeadLines = limit + 1
	case "get_service_status":
		cmd, _, err = serviceStatusCommand(args.ServiceTarget)
	case "get_service_logs":
		cmd, err = serviceLogsCommand(args)
	case "read_file":
		cmd, err = t.readFileCommand(ctx, args)
	case "search_file":
		cmd, err = t.searchFileCommand(ctx, args)
	case "get_ip_rules":
		cmd, err = ipRulesCommand(args)
	case "get_ip_routes":
		cmd, err = ipRoutesCommand(args)
	case "get_route_to":
		cmd, err = routeToCommand(args)
	case "ping_host":
		cmd, err = pingCommand(args)
	case "get_network_sockets":
		cmd, err = socketsCommand(args)
	case "get_network_interfaces":
		stage.Commands, err = interfacesCommands(args)
		return stage, err
	case "get_ip_neighbors":
		cmd, err = neighborsCommand(args)
	case "dig_dns":
		cmd, err = digCommand(args)
	case "nslookup_dns":
		cmd, err = nslookupCommand(args)
	default:
		return stage, errors.New("tools: unsupported pipeline source")
	}
	stage.Commands = []remote.Command{cmd}
	return stage, err
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
		cmd.Args = append(cmd.Args, "-e", filter.Pattern)
	case "head", "tail", "sed":
		if filter.Pattern != "" || filter.Regex || filter.IgnoreCase || filter.Limit != nil || (filter.Type != "sed" && filter.StartLine != nil) {
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
		return remote.Stage{}, errors.New("tools: filter must be grep, head, tail or sed")
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
	for _, stream := range []struct {
		name, text, path string
		size             int64
		truncated        bool
	}{
		{"stdout", result.Stdout, result.StdoutPath, result.StdoutBytes, result.StdoutTruncated},
		{"stderr", result.Stderr, result.StderrPath, result.StderrBytes, result.StderrTruncated},
	} {
		if stream.path != "" {
			fmt.Fprintf(&out, "%s_file=%s bytes=%d (raw output; completion=%t)\n", stream.name, stream.path, stream.size, result.Complete)
		}
		if stream.text != "" {
			fmt.Fprintf(&out, "%s:\n%s\n", stream.name, stream.text)
		}
		if stream.truncated {
			fmt.Fprintf(&out, "[%s preview truncated; total %d bytes]\n", stream.name, stream.size)
		}
	}
	if result.HTTPMetadata != "" {
		fmt.Fprintf(&out, "HTTP metadata (status, server IP, seconds, effective URL):\n%s", result.HTTPMetadata)
	}
	if result.HTTPHeaders != "" {
		fmt.Fprintf(&out, "HTTP headers:\n%s", result.HTTPHeaders)
	}
	return out.String()
}
