package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

func TestPipelineLogsFiltersAndArtifacts(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			fakeCommand(t, "journalctl", strings.Repeat("noise "+strings.Repeat("x", 200)+"\n", 450)+"ERROR late secret\nsecond error\n", "stderr secret\n", "0")
			runner := &remote.Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret"}}}}
			if host == "web" {
				runner = fileSSHRunner(t)
			}
			args := map[string]any{"host_id": host, "source": map[string]any{"tool": "get_service_logs", "args": map[string]any{"lines": 500}}, "filters": []any{
				map[string]any{"type": "grep", "pattern": "error", "ignore_case": true}, map[string]any{"type": "tail", "lines": 1},
			}}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			registry := Registry(runner)
			out, err := registry["run_pipeline"](context.Background(), raw)
			if err != nil || !strings.Contains(out, "second error") || strings.Contains(out, "ERROR late") || !strings.Contains(out, "stage[2]=tail exit_code=0") {
				t.Fatalf("pipeline = %q, %v", out, err)
			}
			if host == "local" && (strings.Contains(out, "secret") || !strings.Contains(out, "[redacted]")) {
				t.Fatalf("credential leaked: %q", out)
			}
			args["stdout"], args["stderr"] = "file", "file"
			raw, err = json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			out, err = registry["run_pipeline"](context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			path := pipelineArtifactPath(t, out, "stdout_file=")
			t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })
			read, err := registry["read_file"](context.Background(), json.RawMessage(`{"host_id":"`+host+`","path":"`+path+`"}`))
			if err != nil || read != "second error\n" {
				t.Fatalf("read artifact = %q, %v", read, err)
			}
		})
	}
}

func pipelineArtifactPath(t *testing.T, output, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.SplitN(strings.TrimPrefix(line, prefix), " bytes=", 2)[0]
		}
	}
	t.Fatalf("missing artifact in %q", output)
	return ""
}

func TestPipelineErrorsAndStreamModes(t *testing.T) {
	for _, tc := range []struct {
		exit, stderr, mode, want string
		failure                  bool
	}{
		{"0", "warning\n", "inline", "stderr:\nwarning", false},
		{"0", "warning\n", "merge", "stdout:\nwarning", false},
		{"0", "warning\n", "discard", "stage[1]=grep exit_code=1", false},
		{"2", "permission denied\n", "discard", "stage[0]=get_service_logs exit_code=2", true},
	} {
		t.Run(tc.exit+tc.mode, func(t *testing.T) {
			fakeCommand(t, "journalctl", "", tc.stderr, tc.exit)
			out, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), json.RawMessage(`{"source":{"tool":"get_service_logs"},"filters":[{"type":"grep","pattern":"warning"}],"stderr":"`+tc.mode+`"}`))
			if tc.failure {
				if err == nil {
					t.Fatal("source failure was hidden")
				}
				out = err.Error()
			} else if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("output = %q", out)
			}
		})
	}
}

func TestProcessSearchPipelineLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	for _, host := range []string{"local", "web"} {
		for _, tc := range []struct {
			name, stdout, exit string
			wantErr            bool
		}{
			{"matches", "1 worker\n2 worker\n", "0", false},
			{"no matches", "", "1", false},
			{"command failure", "", "2", true},
		} {
			t.Run(host+"/"+tc.name, func(t *testing.T) {
				fakeCommand(t, "pgrep", tc.stdout, "", tc.exit)
				raw, err := json.Marshal(map[string]any{
					"host_id": host,
					"source":  map[string]any{"tool": "search_processes", "args": map[string]any{"pattern": "worker", "limit": 1}},
				})
				if err != nil {
					t.Fatal(err)
				}
				out, err := Registry(runner)["run_pipeline"](context.Background(), raw)
				if (err != nil) != tc.wantErr {
					t.Fatalf("out=%q err=%v", out, err)
				}
				if err != nil {
					out = err.Error()
				}
				if !strings.Contains(out, "stage[0]=search_processes exit_code="+tc.exit) || strings.Contains(out, "2 worker") {
					t.Fatalf("search status or match limit lost: %q", out)
				}
				if tc.exit == "0" && !strings.Contains(out, "1 worker") {
					t.Fatalf("matching process missing: %q", out)
				}
			})
		}
	}
}

func TestPipelineSourcesUseExistingPreparation(t *testing.T) {
	fixture := fileFixture(t, "source.conf", "one\nerror\n")
	for _, tc := range []struct{ name, program, raw string }{
		{"get_disk_usage", "df", `{}`}, {"get_top_processes", "ps", `{"limit":1}`},
		{"search_processes", "pgrep", `{"pattern":"worker"}`},
		{"get_service_status", "systemctl", `{"service":"nginx"}`}, {"get_service_logs", "journalctl", `{}`},
		{"list_failed_services", "systemctl", `{}`}, {"list_containers", "docker", `{}`},
		{"get_ip_rules", "ip", `{}`}, {"get_ip_routes", "ip", `{"table":"default"}`},
		{"get_route_to", "ip", `{"destination":"127.0.0.1","mark":42}`}, {"ping_host", "ping", `{"destination":"127.0.0.1"}`},
		{"get_network_sockets", "ss", `{}`}, {"get_network_interfaces", "ip", `{}`}, {"get_ip_neighbors", "ip", `{}`},
		{"dig_dns", "dig", `{"query":"example.com"}`}, {"nslookup_dns", "nslookup", `{"query":"192.0.2.1"}`},
		{"read_file", "sed", `{"path":"` + fixture + `"}`}, {"search_file", "grep", `{"path":"` + fixture + `","pattern":"error"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, tc.program, "raw source\n", "", "0")
			raw := json.RawMessage(`{"source":{"tool":"` + tc.name + `","args":` + tc.raw + `}}`)
			out, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
			if err != nil || !strings.Contains(out, "raw source") {
				t.Fatalf("source = %q, %v", out, err)
			}
		})
	}
	stage, err := (&Tools{}).sourceStage(context.Background(), "get_load_average", toolArgs{})
	if err != nil || stage.Commands[0].Program != "cat" || stage.Commands[0].Args[0] != "/proc/loadavg" {
		t.Fatalf("load source = %+v, %v", stage, err)
	}
}

func TestPipelineRejectsUnsafeInputBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "journalctl", "", "", "0")
	for _, raw := range []string{
		`{}`, `null`, `{"source":{"tool":"sh","args":{"command":"true"}}}`,
		`{"source":{"tool":"list_hosts"}}`, `{"source":{"tool":"run_pipeline"}}`, `{"source":{"tool":"get_memory"}}`,
		`{"source":{"tool":"list_directory","args":{"path":"/"}}}`, `{"source":{"tool":"check_tls_certificate","args":{"destination":"example.com"}}}`,
		`{"source":{"tool":"get_service_logs","args":{"host_id":"web"}}}`,
		`{"source":{"tool":"get_service_logs","args":{"destination":"127.0.0.1"}}}`,
		`{"source":{"tool":"get_service_logs","args":{"service":"x; id"}}}`,
		`{"source":{"tool":"get_service_logs"},"stdout":"/tmp/file"}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"sed","script":"w /etc/file"}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"grep","pattern":"x","path":"/etc/passwd"}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"grep","pattern":"x","invert":"yes"}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"head","invert":true}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"sed","invert":true}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","pattern":"x"}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","regex":true}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","invert":true}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","lines":1}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","start_line":1}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","limit":0}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","limit":501}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","limit":1.5}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","limit":"1"}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","command":"id"}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"grep","pattern":""}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"head","lines":501}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"sed","start_line":0}]}`,
		`{"source":{"tool":"get_service_logs"},"filters":[{"type":"head"},{"type":"head"},{"type":"head"},{"type":"head"},{"type":"head"}]}`,
	} {
		if _, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("source ran before validation")
	}
}

func TestPipelineInvert(t *testing.T) {
	fakeCommand(t, "journalctl", "error one\ninfo two\nERROR three\n", "", "0")
	raw := json.RawMessage(`{"source":{"tool":"get_service_logs"},"filters":[{"type":"grep","pattern":"error","ignore_case":true,"invert":true}]}`)
	out, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "info two") || strings.Contains(out, "error one") || strings.Contains(out, "ERROR three") {
		t.Fatalf("inverted grep = %q, %v", out, err)
	}
}

func TestPipelineCount(t *testing.T) {
	fakeCommand(t, "journalctl", "error A\nerror A\nwarning B\n", "", "0")
	raw := json.RawMessage(`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count"}]}`)
	out, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "2 error A") || !strings.Contains(out, "1 warning B") ||
		!strings.Contains(out, "stage[1]=count exit_code=0") {
		t.Fatalf("count = %q, %v", out, err)
	}

	raw = json.RawMessage(`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","limit":1}]}`)
	out, err = Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "2 error A") || strings.Contains(out, "warning B") {
		t.Fatalf("count limit = %q, %v", out, err)
	}

	fakeCommand(t, "journalctl", "error A\nerror a\nwarning B\n", "", "0")
	raw = json.RawMessage(`{"source":{"tool":"get_service_logs"},"filters":[{"type":"count","ignore_case":true}]}`)
	out, err = Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "2 error A") || !strings.Contains(out, "1 warning B") {
		t.Fatalf("count ignore_case = %q, %v", out, err)
	}
}

func TestPipelinePatternIsLiteralShellData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unexpected")
	pattern := "$(touch " + path + ")"
	fakeCommand(t, "journalctl", pattern+"\n", "", "0")
	raw, err := json.Marshal(map[string]any{"source": map[string]any{"tool": "get_service_logs"}, "filters": []any{map[string]any{"type": "grep", "pattern": pattern}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
	if err != nil || !strings.Contains(out, pattern) {
		t.Fatalf("literal pattern = %q, %v", out, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pattern executed as shell")
	}
}
