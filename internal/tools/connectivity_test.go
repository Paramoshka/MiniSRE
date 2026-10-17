package tools

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestCheckTCPPortLocal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout is not available")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen locally: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	raw, err := json.Marshal(map[string]any{"destination": "127.0.0.1", "port": port})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Registry(&remote.Runner{})["check_tcp_port"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "connected") || !strings.Contains(out, "TCP handshake") {
		t.Fatalf("successful TCP check = %q, %v", out, err)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	raw, err = json.Marshal(map[string]any{"destination": "127.0.0.1", "port": closedPort})
	if err != nil {
		t.Fatal(err)
	}
	out, err = Registry(&remote.Runner{})["check_tcp_port"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "failed") || !strings.Contains(out, "exit code 1") {
		t.Fatalf("refused TCP check = %q, %v", out, err)
	}
}

func TestCheckTCPPortPassesArgumentsLiterally(t *testing.T) {
	argsFile := fakeCommand(t, "timeout", "connected\n", "", "0")
	raw := json.RawMessage(`{"destination":"example.com","port":443}`)
	out, err := Registry(&remote.Runner{})["check_tcp_port"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "connected") {
		t.Fatalf("TCP check = %q, %v", out, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "5\nbash\n--noprofile\n--norc\n-c\n" + tcpConnectScript + "\nmini-sre-tcp\nexample.com\n443\n"
	if string(args) != want {
		t.Fatalf("timeout arguments = %q, want %q", args, want)
	}
}

func TestCheckTCPPortUnavailable(t *testing.T) {
	fakeCommand(t, "timeout", "", "timeout: not found\n", "127")
	raw := json.RawMessage(`{"destination":"127.0.0.1","port":80}`)
	if _, err := Registry(&remote.Runner{})["check_tcp_port"](context.Background(), raw); err == nil {
		t.Fatal("missing probe command was reported as a diagnostic result")
	}
}

func TestCheckTCPPortRejectsArgumentsBeforeExecution(t *testing.T) {
	argsFile := fakeCommand(t, "timeout", "", "", "0")
	call := Registry(&remote.Runner{})["check_tcp_port"]
	for _, raw := range []string{
		`{}`, `{"destination":"127.0.0.1"}`, `{"port":80}`,
		`{"destination":"127.0.0.1","port":0}`, `{"destination":"127.0.0.1","port":65536}`,
		`{"destination":"127.0.0.1","port":-1}`, `{"destination":"127.0.0.1","port":"80"}`,
		`{"destination":"127.0.0.1","port":1.5}`, `{"destination":"bad/name","port":80}`,
		`{"destination":"fe80::1%eth0","port":80}`, `{"destination":"-bad","port":80}`,
		`{"destination":"host_name","port":80}`, `{"destination":"a..b","port":80}`,
		`{"destination":"` + strings.Repeat("a", 64) + `.com","port":80}`,
		`{"destination":"` + strings.Repeat("a", 254) + `","port":80}`,
		`{"destination":"127.0.0.1","port":80,"command":"id"}`, `null`, `[]`, `{bad`,
	} {
		if _, err := call(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("timeout ran before argument validation")
	}
}

func TestPipelineTCPSource(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout is not available")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen locally: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	raw := json.RawMessage(`{"source":{"tool":"check_tcp_port","args":{"destination":"127.0.0.1","port":` + strconv.Itoa(port) + `}}}`)
	out, err := Registry(&remote.Runner{})["run_pipeline"](context.Background(), raw)
	if err != nil || !strings.Contains(out, "stage[0]=check_tcp_port exit_code=0") || !strings.Contains(out, "connected") {
		t.Fatalf("TCP pipeline source = %q, %v", out, err)
	}
}

func TestPipelineTCPStatusClassification(t *testing.T) {
	for code, want := range map[int]bool{0: true, 1: true, 124: true, 2: false, 125: false} {
		if got := pipelineStageSucceeded("check_tcp_port", nil, 0, code); got != want {
			t.Errorf("check_tcp_port exit %d succeeded = %t, want %t", code, got, want)
		}
	}
	if pipelineStageSucceeded("ping_host", nil, 0, 124) {
		t.Error("ping timeout should remain a pipeline failure")
	}
}
