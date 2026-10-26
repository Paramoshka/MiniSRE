package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestTCPInfoLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	argsFile := fakeCommand(t, "ss", "ESTAB 0 0 192.0.2.1:40000 192.0.2.2:443\n rtt:20/3 retrans:0/2\n", "", "0")
	for _, host := range []string{"local", "web"} {
		for _, pipeline := range []bool{false, true} {
			name := "get_network_sockets"
			raw := `{"host_id":"` + host + `","protocol":"tcp","state":"established","port":443,"tcp_info":true}`
			if pipeline {
				name = "run_pipeline"
				raw = `{"host_id":"` + host + `","source":{"tool":"get_network_sockets","args":{"protocol":"tcp","state":"established","port":443,"tcp_info":true}}}`
			}
			out, err := Registry(runner)[name](context.Background(), json.RawMessage(raw))
			if err != nil || !strings.Contains(out, "rtt:20/3") {
				t.Fatalf("%s/%s = %q, %v", host, name, out, err)
			}
			flags, err := os.ReadFile(argsFile)
			if err != nil || string(flags) != "-n\n-p\n-i\n-t\n-a\nstate\nestablished\n( sport = :443 or dport = :443 )\n" {
				t.Fatalf("arguments = %q, %v", flags, err)
			}
		}
	}
}

func TestTCPInfoValidation(t *testing.T) {
	argsFile := fakeCommand(t, "ss", "", "", "0")
	for _, raw := range []string{`{"tcp_info":true}`, `{"tcp_info":true,"protocol":"both"}`, `{"tcp_info":true,"protocol":"udp"}`, `{"tcp_info":"true","protocol":"tcp"}`} {
		for _, pipeline := range []bool{false, true} {
			name, args := "get_network_sockets", raw
			if pipeline {
				name, args = "run_pipeline", `{"source":{"tool":"get_network_sockets","args":`+raw+`}}`
			}
			if _, err := Registry(&remote.Runner{})[name](context.Background(), json.RawMessage(args)); err == nil {
				t.Fatalf("%s accepted %s", name, raw)
			}
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatal("ss ran before validation")
	}
}
