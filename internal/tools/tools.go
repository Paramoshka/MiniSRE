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
	Mutating    bool
}

type Func func(ctx context.Context, args json.RawMessage) (string, error)

const maxToolResult = 128 * 1024

type Tools struct {
	Runner *remote.Runner
}

// Specs returns the model-visible tool schemas.
func Specs() []Spec {
	defs := definitions()
	specs := make([]Spec, 0, len(defs))
	for _, def := range defs {
		specs = append(specs, def.spec())
	}
	return specs
}

// Registry maps tool names to handlers. The same configuration snapshot is
// used for the handler, its connection and output redaction.
func Registry(runner *remote.Runner) map[string]Func {
	t := &Tools{Runner: runner}
	defs := definitions()
	registry := make(map[string]Func, len(defs))
	for _, def := range defs {
		def := def
		registry[def.Name] = func(ctx context.Context, raw json.RawMessage) (string, error) {
			if err := validateArgKeys(def.Parameters, raw); err != nil {
				return "", err
			}
			out, err := runDefinition(ctx, def, t, raw)
			if err != nil {
				return "", errors.New(runner.Redact(err.Error()))
			}
			return capToolResult(runner.Redact(out)), nil
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
	Priority     string `json:"priority"`
	Boot         string `json:"boot"`
	Output       string `json:"output"`
	SortBy       string `json:"sort_by"`
	Limit        *int   `json:"limit"`
	StartLine    *int   `json:"start_line"`
	Tail         bool   `json:"tail"`
	RunningOnly  bool   `json:"running_only"`
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
	TCPInfo         bool    `json:"tcp_info"`
	State           string  `json:"state"`
	Port            *int    `json:"port"`

	ServerHostID string `json:"server_host_id"`
	ClientHostID string `json:"client_host_id"`
	BindAddress  string `json:"bind_address"`
	Duration     *int   `json:"duration"`

	Query      string `json:"query"`
	RecordType string `json:"record_type"`
	Server     string `json:"server"`
	ServerName string `json:"server_name"`

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

func capToolResult(out string) string {
	if len(out) <= maxToolResult {
		return out
	}
	cut := maxToolResult
	// Do not split a UTF-8 sequence.
	for cut > 0 && out[cut]&0xC0 == 0x80 {
		cut--
	}
	return out[:cut] + fmt.Sprintf("\n[result truncated at %d KiB]", maxToolResult/1024)
}

func schemaProperties(parameters json.RawMessage) (map[string]json.RawMessage, error) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(parameters, &schema); err != nil {
		return nil, errors.New("tools: invalid tool schema")
	}
	return schema.Properties, nil
}

// validateArgKeys rejects top-level arguments that the tool schema does not
// declare, so fields belonging to other tools cannot be silently ignored.
func validateArgKeys(parameters, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var args map[string]json.RawMessage
	if err := decodeObject(raw, &args); err != nil {
		return err
	}
	properties, err := schemaProperties(parameters)
	if err != nil {
		return err
	}
	for key := range args {
		if _, ok := properties[key]; !ok {
			return errors.New("tools: invalid arguments; check field names and types")
		}
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
