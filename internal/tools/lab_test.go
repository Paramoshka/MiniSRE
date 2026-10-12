package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-sre/internal/config"
	"mini-sre/internal/lab"
	"mini-sre/internal/remote"
)

func TestLabHelperTransportAndValidation(t *testing.T) {
	dir := t.TempDir()
	argsPath, inputPath := filepath.Join(dir, "args"), filepath.Join(dir, "request")
	t.Setenv("MINISRE_TEST_ARGS", argsPath)
	t.Setenv("MINISRE_TEST_INPUT", inputPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$MINISRE_TEST_ARGS\"\ncat >\"$MINISRE_TEST_INPUT\"\nprintf '[]\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	registry := Registry(&remote.Runner{Config: config.Config{LabHosts: []string{"local"}}})
	if _, err := registry["lab_run"](context.Background(), json.RawMessage(`{"host_id":"local","name":"demo","node":"r1","checks":[{"type":"ping","destination":"10.0.0.1"}]}`)); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || string(args) != "-n\n--\n/usr/local/sbin/mini-sre-lab\nrequest\n" {
		t.Fatalf("unexpected command %q %v", args, err)
	}
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	req, err := lab.DecodeRequest(strings.NewReader(string(input)))
	if err != nil || req.Action != "run" || req.Checks[0].Destination != "10.0.0.1" {
		t.Fatalf("invalid stdin %s %v", input, err)
	}
	for _, data := range []string{
		`{"name":"demo"}`, `{"host_id":"other","name":"demo"}`, `{"host_id":"local","name":"../escape"}`,
		`{"host_id":"local","name":"demo","action":"destroy"}`, `{"host_id":"local","topo_yaml":"name: demo","ttl_minutes":0}`,
	} {
		if _, err := registry["lab_inspect"](context.Background(), json.RawMessage(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if _, err := Registry(&remote.Runner{})["lab_inspect"](context.Background(), json.RawMessage(`{"host_id":"local"}`)); err == nil {
		t.Fatal("disabled lab host accepted")
	}
	for _, name := range []string{"lab_deploy", "lab_inspect", "lab_run", "lab_destroy"} {
		if _, err := pipelineSourceArgs(name, json.RawMessage(`{}`)); err == nil {
			t.Fatal("lab accepted as pipeline source")
		}
	}
	for _, spec := range Specs() {
		if spec.Name == "run_pipeline" && strings.Contains(string(spec.Parameters), `lab_deploy`) {
			t.Fatal("pipeline advertises deployment")
		}
	}
}
