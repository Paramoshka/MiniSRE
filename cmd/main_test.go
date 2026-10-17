package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mini-sre/internal/agent"
	"mini-sre/internal/config"
	"mini-sre/internal/session"
)

func TestAgentUsesPipelineAndCurl(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip(err)
	}
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte("#!/bin/sh\nprintf 'error agent-secret\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "HTTP body agent-secret") }))
	defer server.Close()
	curlArgs, err := json.Marshal(map[string]any{"url": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Hosts: map[string]config.Host{"web": {Password: "agent-secret"}}}
	calls := 0
	s := &session.Session{
		Tools: toolSpecs(), RunTool: toolRunner(func() config.Config { return cfg }),
		Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
			calls++
			if calls == 1 {
				names := map[string]bool{}
				for _, tool := range req.Tools {
					names[tool.Name] = true
				}
				if !names["run_pipeline"] || !names["curl_http"] {
					t.Fatal("new tool definitions missing")
				}
				return &agent.Response{ToolCalls: []agent.ToolCall{
					{ID: "logs", Name: "run_pipeline", Arguments: `{"source":{"tool":"get_service_logs"},"filters":[{"type":"grep","pattern":"error"}]}`},
					{ID: "http", Name: "curl_http", Arguments: string(curlArgs)},
				}}, nil
			}
			results := map[string]string{}
			for _, m := range req.Messages {
				if m.Role == agent.RoleTool {
					results[m.ToolCallID] = m.Content
				}
			}
			if strings.Contains(results["http"], "curl >= 8.4") {
				t.Skip("installed curl is older than required")
			}
			for name, out := range results {
				if strings.HasPrefix(out, "error:") || strings.Contains(out, "agent-secret") {
					t.Fatalf("%s result = %q", name, out)
				}
			}
			if !strings.Contains(results["logs"], "error [redacted]") || !strings.Contains(results["http"], "HTTP body [redacted]") {
				t.Fatalf("missing results: %+v", results)
			}
			return &agent.Response{Content: "Checked"}, nil
		},
	}
	response, err := s.Ask(context.Background(), "Check logs and HTTP")
	if err != nil || response.Content != "Checked" || calls != 2 {
		t.Fatalf("agent flow = %+v, %v, calls=%d", response, err, calls)
	}
}

func TestConfiguredAgentUsesProbesWithoutExposingCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hosts.yaml")
	content := "hosts:\n  web-1:\n    address: 192.0.2.123\n    user: sre\n    password: test-ssh-secret\n  storage-1:\n    address: 192.0.2.124\n    user: sre\n    key_file: private-key\n"
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"ps":         "#!/bin/sh\nprintf '%s\\n' '    PID USER %CPU %MEM RSS COMMAND' '     42 sre 99.0 2.5 102400 worker'\n",
		"systemctl":  "#!/bin/sh\nprintf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n'\n",
		"journalctl": "#!/bin/sh\nprintf 'app warning: test-ssh-secret\\n'\n",
		"docker":     "#!/bin/sh\ncase \"$1\" in\ninspect) printf '{\"Status\":\"running\"}\\n';;\nlogs) printf 'container warning: test-ssh-secret\\n' >&2;;\nesac\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		for _, secret := range []string{"test-ssh-secret", "192.0.2.123", "192.0.2.124", "private-key"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("model request exposed config value %q", secret)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			var req struct {
				Tools []struct {
					Function struct{ Name string } `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(body, &req); err != nil || len(req.Tools) != 28 {
				t.Errorf("tool definitions missing: %v, %d", err, len(req.Tools))
			}
			hasVMStat := false
			for _, tool := range req.Tools {
				if tool.Function.Name == "get_vmstat" {
					hasVMStat = true
				}
			}
			if !hasVMStat {
				t.Error("vmstat tool definition missing from model request")
			}
			io.WriteString(w, `{"id":"first","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"hosts","type":"function","function":{"name":"list_hosts","arguments":"{}"}},{"id":"la","type":"function","function":{"name":"get_load_average","arguments":"{}"}},{"id":"disk","type":"function","function":{"name":"get_disk_usage","arguments":"{\"path\":\"/\"}"}},{"id":"systemd","type":"function","function":{"name":"get_service_status","arguments":"{\"service\":\"nginx\"}"}},{"id":"docker","type":"function","function":{"name":"get_service_status","arguments":"{\"service\":\"web\",\"backend\":\"docker\"}"}},{"id":"journal","type":"function","function":{"name":"get_service_logs","arguments":"{}"}},{"id":"container_logs","type":"function","function":{"name":"get_service_logs","arguments":"{\"service\":\"web\",\"backend\":\"docker\"}"}},{"id":"processes","type":"function","function":{"name":"get_top_processes","arguments":"{\"sort_by\":\"cpu\",\"limit\":3}"}}]}}]}`)
			return
		}
		var req struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Error(err)
		}
		results := map[string]string{}
		for _, m := range req.Messages {
			if m.Role == "tool" {
				results[m.ToolCallID] = m.Content
				if strings.HasPrefix(m.Content, "error:") {
					t.Errorf("probe failed: %s", m.Content)
				}
			}
		}
		if len(results) != 8 || !strings.Contains(results["hosts"], "web-1") || !strings.Contains(results["disk"], "available=") || !strings.Contains(results["la"], "load average:") || !strings.Contains(results["journal"], "[redacted]") || !strings.Contains(results["container_logs"], "[redacted]") || !strings.Contains(results["processes"], "worker") || !strings.Contains(results["processes"], "lifetime average") {
			t.Errorf("probe results missing: %+v", results)
		}
		io.WriteString(w, `{"id":"final","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Checked"}}]}`)
	}))
	defer server.Close()
	client, err := agent.New(agent.Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	s := &session.Session{Turn: client.Chat, Tools: toolSpecs(), RunTool: toolRunner(func() config.Config { return cfg })}
	resp, err := s.Ask(context.Background(), "Check local")
	if err != nil || resp.Content != "Checked" || requests.Load() != 2 {
		t.Fatalf("agent/tools flow: response=%+v err=%v requests=%d", resp, err, requests.Load())
	}
}

func TestToolRunnerPinsConfigForRedaction(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte("#!/bin/sh\nprintf 'old-secret\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	cfg := config.Config{Hosts: map[string]config.Host{"old": {Password: "old-secret"}}}
	run := toolRunner(func() config.Config {
		snapshot := cfg
		cfg = config.Config{Hosts: map[string]config.Host{"new": {Password: "new-secret"}}}
		return snapshot
	})
	out, err := run(context.Background(), agent.ToolCall{Name: "get_service_logs", Arguments: "{}"})
	if err != nil || strings.Contains(out, "old-secret") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("tool did not redact with its original snapshot: %q, %v", out, err)
	}
	out, err = run(context.Background(), agent.ToolCall{Name: "list_hosts"})
	if err != nil || out != "local\nnew" {
		t.Fatalf("next call did not use new hosts: %q, %v", out, err)
	}
}

func TestAgentUsesFileTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(path, []byte("listen 80\nsetting=file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	readArgs, err := json.Marshal(map[string]any{"path": path, "lines": 2})
	if err != nil {
		t.Fatal(err)
	}
	searchArgs, err := json.Marshal(map[string]any{"path": path, "pattern": "setting"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Hosts: map[string]config.Host{"web": {Password: "file-secret"}}}
	calls := 0
	s := &session.Session{
		Tools: toolSpecs(), RunTool: toolRunner(func() config.Config { return cfg }),
		Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
			calls++
			if calls == 1 {
				names := map[string]bool{}
				for _, spec := range req.Tools {
					names[spec.Name] = true
				}
				if !names["read_file"] || !names["search_file"] {
					t.Fatal("file tool definitions did not reach the model")
				}
				return &agent.Response{ToolCalls: []agent.ToolCall{
					{ID: "read", Name: "read_file", Arguments: string(readArgs)},
					{ID: "search", Name: "search_file", Arguments: string(searchArgs)},
				}}, nil
			}
			results := map[string]string{}
			for _, message := range req.Messages {
				if message.Role == agent.RoleTool {
					results[message.ToolCallID] = message.Content
				}
			}
			if results["read"] != "listen 80\nsetting=[redacted]\n" || results["search"] != "2:setting=[redacted]\n" {
				t.Fatalf("file results missing or unredacted: %+v", results)
			}
			return &agent.Response{Content: "Checked"}, nil
		},
	}
	response, err := s.Ask(context.Background(), "Read the config and find setting")
	if err != nil || response.Content != "Checked" || calls != 2 {
		t.Fatalf("file tool session: response=%+v, error=%v, calls=%d", response, err, calls)
	}
}
