package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mini-sre/internal/remote"
)

func TestFormatLoadAverage(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "full",
			raw:  "0.45 0.32 0.28 1/1234 5678\n",
			want: "load average: 0.45 0.32 0.28 (running 1/1234)",
		},
		{
			name: "no procs",
			raw:  "1.00 0.50 0.25",
			want: "load average: 1.00 0.50 0.25",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := formatLoadAverage(tt.raw)
			if err != nil {
				t.Fatalf("formatLoadAverage: %v", err)
			}
			if got != tt.want {
				t.Errorf("formatLoadAverage = %q, want %q", got, tt.want)
			}
		})
	}

	for _, raw := range []string{"", "0.1 0.2"} {
		t.Run("invalid_"+raw, func(t *testing.T) {
			if _, err := formatLoadAverage(raw); err == nil {
				t.Errorf("formatLoadAverage(%q) = nil error, want error", raw)
			}
		})
	}
}

func TestLoadAverage(t *testing.T) {
	if _, err := os.Stat(loadAveragePath); err != nil {
		t.Skipf("%s unavailable: %v", loadAveragePath, err)
	}

	got, err := (&Tools{Runner: &remote.Runner{}}).LoadAverage(context.Background(), "local")
	if err != nil {
		t.Fatalf("LoadAverage: %v", err)
	}
	if !strings.HasPrefix(got, "load average: ") {
		t.Errorf("LoadAverage = %q, want load average prefix", got)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{5 * 1024 * 1024 * 1024, "5.0 GiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDiskUsage(t *testing.T) {
	dir := t.TempDir()
	tools := &Tools{Runner: &remote.Runner{}}
	got, err := tools.DiskUsage(context.Background(), "local", dir)
	if err != nil {
		t.Fatalf("DiskUsage(%q): %v", dir, err)
	}
	if !strings.HasPrefix(got, dir+": total=") || !strings.Contains(got, "use=") {
		t.Errorf("DiskUsage = %q, want %q prefix with usage", got, dir)
	}

	root, err := tools.DiskUsage(context.Background(), "local", "")
	if err != nil {
		t.Fatalf("DiskUsage(default): %v", err)
	}
	if !strings.HasPrefix(root, "/: total=") {
		t.Errorf("DiskUsage(default) = %q, want / prefix", root)
	}

	if _, err := tools.DiskUsage(context.Background(), "local", dir+"/missing"); err == nil {
		t.Error("DiskUsage(missing) = nil error, want error")
	}
}

func TestRegistry(t *testing.T) {
	registry := Registry(&remote.Runner{})

	load, ok := registry["get_load_average"]
	if !ok {
		t.Fatal("registry missing get_load_average")
	}
	if _, err := load(context.Background(), nil); err != nil {
		t.Errorf("get_load_average: %v", err)
	}

	disk, ok := registry["get_disk_usage"]
	if !ok {
		t.Fatal("registry missing get_disk_usage")
	}

	out, err := disk(context.Background(), json.RawMessage(`{"path":"/"}`))
	if err != nil {
		t.Fatalf("get_disk_usage: %v", err)
	}
	if !strings.HasPrefix(out, "/: total=") {
		t.Errorf("get_disk_usage = %q, want / prefix", out)
	}

	out, err = disk(context.Background(), nil)
	if err != nil {
		t.Fatalf("get_disk_usage(default): %v", err)
	}
	if !strings.HasPrefix(out, "/: total=") {
		t.Errorf("get_disk_usage(default) = %q, want / prefix", out)
	}

	if _, err := disk(context.Background(), json.RawMessage(`{bad`)); err == nil {
		t.Error("get_disk_usage(invalid args) = nil error, want error")
	}
}

func TestRegistryRejectsForeignArguments(t *testing.T) {
	registry := Registry(&remote.Runner{})
	for _, tc := range []struct {
		name, raw string
	}{
		{"get_load_average", `{"count":5}`},
		{"get_memory", `{"path":"/"}`},
		{"list_hosts", `{"path":"/"}`},
		{"run_pipeline", `{"source":{"tool":"get_load_average"},"bogus":true}`},
		{"lab_inspect", `{"host_id":"local","topo_yaml":"name: demo"}`},
		{"measure_throughput", `{"host_id":"local"}`},
	} {
		if _, err := registry[tc.name](context.Background(), json.RawMessage(tc.raw)); err == nil {
			t.Errorf("%s accepted arguments from another tool: %s", tc.name, tc.raw)
		}
	}
}

func TestMutatingTools(t *testing.T) {
	for _, spec := range Specs() {
		want := spec.Name == "lab_deploy" || spec.Name == "lab_destroy" || spec.Name == "measure_throughput"
		if spec.Mutating != want {
			t.Errorf("tool %q mutating=%t, want %t", spec.Name, spec.Mutating, want)
		}
	}
}

func TestDefinitionTimeout(t *testing.T) {
	def := Definition{
		Name: "slow", Description: "slow",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Timeout:    10 * time.Millisecond,
		Run: func(ctx context.Context, _ *Tools, _ json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	if _, err := runDefinition(context.Background(), def, &Tools{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("definition timeout: %v", err)
	}
}

func TestCapToolResult(t *testing.T) {
	short := strings.Repeat("a", 100)
	if got := capToolResult(short); got != short {
		t.Fatal("short result was changed")
	}
	got := capToolResult(strings.Repeat("a", maxToolResult+100))
	if !strings.HasPrefix(got, strings.Repeat("a", maxToolResult)) || !strings.Contains(got, "truncated") {
		t.Fatal("long result was not capped")
	}
	got = capToolResult(strings.Repeat("a", maxToolResult-1) + "€tail")
	if !utf8.ValidString(got) {
		t.Fatal("cap split a UTF-8 sequence")
	}
}

func TestDefinitions(t *testing.T) {
	seen := make(map[string]bool)
	for _, def := range definitions() {
		if def.Name == "" || def.Description == "" || len(def.Parameters) == 0 || def.Run == nil {
			t.Errorf("incomplete definition: %+v", def)
		}
		if seen[def.Name] {
			t.Errorf("duplicate tool definition %q", def.Name)
		}
		seen[def.Name] = true
	}
}

func TestSpecs(t *testing.T) {
	specs := Specs()
	if len(specs) != 32 {
		t.Fatalf("Specs() = %d, want 32", len(specs))
	}
	registry := Registry(&remote.Runner{})
	for _, spec := range specs {
		if _, ok := registry[spec.Name]; !ok {
			t.Errorf("spec %q has no registry entry", spec.Name)
		}
		if spec.Description == "" {
			t.Errorf("spec %q has empty description", spec.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Errorf("spec %q parameters: %v", spec.Name, err)
		}
	}
}
