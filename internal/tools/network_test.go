package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestNetworkCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		name, program, raw, want string
		run                      func(*Tools, context.Context, toolArgs) (string, error)
	}{
		{"rules", "ip", `{}`, "-4\nrule\nshow\n", (*Tools).IPRules},
		{"rules table", "ip", `{"family":"ipv6","table":"default"}`, "-6\nrule\nshow\ntable\ndefault\n", (*Tools).IPRules},
		{"routes", "ip", `{}`, "-4\nroute\nshow\ntable\nmain\n", (*Tools).IPRoutes},
		{"all routes", "ip", `{"family":"ipv6","table":"all","interface":"eth0"}`, "-6\nroute\nshow\ntable\nall\ndev\neth0\n", (*Tools).IPRoutes},
		{"custom table", "ip", `{"table":"100"}`, "-4\nroute\nshow\ntable\n100\n", (*Tools).IPRoutes},
		{"named table", "ip", `{"table":"vpn"}`, "-4\nroute\nshow\ntable\nvpn\n", (*Tools).IPRoutes},
		{"route lookup", "ip", `{"destination":"192.0.2.5"}`, "-4\nroute\nget\n192.0.2.5\n", (*Tools).RouteTo},
		{"policy lookup", "ip", `{"destination":"2001:db8::5","source":"2001:db8::1","mark":42,"input_interface":"eth0","output_interface":"eth1"}`, "-6\nroute\nget\n2001:db8::5\nfrom\n2001:db8::1\nmark\n42\niif\neth0\noif\neth1\n", (*Tools).RouteTo},
		{"zero mark", "ip", `{"destination":"127.0.0.1","mark":0}`, "-4\nroute\nget\n127.0.0.1\nmark\n0\n", (*Tools).RouteTo},
		{"neighbors", "ip", `{}`, "-4\nneigh\nshow\n", (*Tools).IPNeighbors},
		{"ipv6 neighbors", "ip", `{"family":"ipv6","interface":"eth0"}`, "-6\nneigh\nshow\ndev\neth0\n", (*Tools).IPNeighbors},
		{"ping defaults", "ping", `{"destination":"example.com"}`, "-n\n-c\n3\n-W\n2\n-w\n8\n--\nexample.com\n", (*Tools).PingHost},
		{"ping ipv6", "ping", `{"destination":"fe80::1","family":"ipv6","interface":"eth0","count":5}`, "-n\n-c\n5\n-W\n2\n-w\n8\n-6\n-I\neth0\n--\nfe80::1\n", (*Tools).PingHost},
		{"listeners", "ss", `{}`, "-n\n-p\n-t\n-u\n-l\n", (*Tools).NetworkSockets},
		{"udp", "ss", `{"protocol":"udp","family":"ipv6","state":"all"}`, "-n\n-p\n-u\n-6\n-a\n", (*Tools).NetworkSockets},
		{"tcp state and port", "ss", `{"protocol":"tcp","family":"ipv4","state":"close-wait","port":443}`, "-n\n-p\n-t\n-4\n-a\nstate\nclose-wait\n( sport = :443 or dport = :443 )\n", (*Tools).NetworkSockets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := fakeCommand(t, tc.program, "diagnostic output\n", "", "0")
			args, err := decodeArgs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			out, err := tc.run(&Tools{Runner: &remote.Runner{}}, context.Background(), args)
			if err != nil || out != "diagnostic output\n" {
				t.Fatalf("probe = %q, %v", out, err)
			}
			flags, err := os.ReadFile(argsFile)
			if err != nil || string(flags) != tc.want {
				t.Fatalf("arguments = %q, %v; want %q", flags, err, tc.want)
			}
		})
	}
}

func TestNetworkRejectsArgumentsBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name, program string
		invalid       []string
		run           func(*Tools, context.Context, toolArgs) (string, error)
	}{
		{"rules", "ip", []string{`{"family":"all"}`, `{"table":"all"}`, `{"table":"-details"}`, `{"table":"main; flush"}`, `{"table":"0"}`, `{"table":"00100"}`, `{"table":"4294967296"}`}, (*Tools).IPRules},
		{"routes", "ip", []string{`{"family":"inet"}`, `{"table":"/etc/file"}`, `{"interface":"-batch"}`, `{"interface":"eth0;flush"}`}, (*Tools).IPRoutes},
		{"lookup", "ip", []string{`{}`, `{"destination":"example.com"}`, `{"destination":"192.0.2.0/24"}`, `{"destination":"::1%lo"}`, `{"destination":"::1","source":"127.0.0.1"}`, `{"destination":"127.0.0.1","source":"x"}`, `{"destination":"127.0.0.1","mark":-1}`, `{"destination":"127.0.0.1","mark":4294967296}`, `{"destination":"127.0.0.1","input_interface":"eth0; echo"}`, `{"destination":"127.0.0.1","output_interface":"1234567890123456"}`}, (*Tools).RouteTo},
		{"ping", "ping", []string{`{}`, `{"destination":"-f"}`, `{"destination":"a; echo injected"}`, `{"destination":"$(true)"}`, `{"destination":"::1","family":"ipv4"}`, `{"destination":"127.0.0.1","count":0}`, `{"destination":"127.0.0.1","count":6}`, `{"destination":"127.0.0.1","count":1.5}`, `{"destination":"127.0.0.1","interface":"-f"}`}, (*Tools).PingHost},
		{"sockets", "ss", []string{`{"protocol":"unix"}`, `{"state":"-K"}`, `{"state":"established"}`, `{"protocol":"udp","state":"time-wait"}`, `{"port":0}`, `{"port":65536}`, `{"port":"22 or dst any"}`, `{"command":"ss -K"}`}, (*Tools).NetworkSockets},
		{"interfaces", "ip", []string{`{"family":"inet"}`, `{"interface":"eth0\u0000x"}`}, (*Tools).NetworkInterfaces},
		{"neighbors", "ip", []string{`{"family":"all"}`, `{"interface":"eth0 flush"}`}, (*Tools).IPNeighbors},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := fakeCommand(t, tc.program, "", "", "0")
			for _, raw := range tc.invalid {
				args, err := decodeArgs(json.RawMessage(raw))
				if err == nil {
					_, err = tc.run(&Tools{Runner: &remote.Runner{}}, context.Background(), args)
				}
				if err == nil {
					t.Fatalf("invalid arguments accepted: %s", raw)
				}
			}
			if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
				t.Fatal("command ran before validation")
			}
		})
	}
}

func TestNetworkInterfacesCommands(t *testing.T) {
	dir := t.TempDir()
	argsFile := fakeCommand(t, "ip", "", "", "0")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$MINISRE_TEST_ARGS\"\ncase \"$*\" in\n*address*) printf 'address output\\n';;\n*) printf 'link counters\\n';;\nesac\n"
	if err := os.WriteFile(dir+"/ip", []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	out, err := (&Tools{Runner: &remote.Runner{}}).NetworkInterfaces(context.Background(), toolArgs{Family: "ipv6", Interface: "eth0"})
	if err != nil || out != "Addresses:\naddress output\n\nLink statistics:\nlink counters\n" {
		t.Fatalf("interfaces = %q, %v", out, err)
	}
	flags, err := os.ReadFile(argsFile)
	if err != nil || string(flags) != "-6\naddress\nshow\ndev\neth0\n-s\nlink\nshow\ndev\neth0\n" {
		t.Fatalf("interface commands = %q, %v", flags, err)
	}
}

func TestPingResults(t *testing.T) {
	for _, tc := range []struct {
		name, output, exit string
		wantErr            bool
	}{
		{"healthy", "3 received, 0% packet loss\n", "0", false},
		{"no replies", "0 received, 100% packet loss\n", "1", false},
		{"partial replies", "1 received, 66% packet loss\n", "1", false},
		{"execution error", "cannot create ICMP socket\n", "2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "ping", tc.output, "", tc.exit)
			out, err := (&Tools{Runner: &remote.Runner{}}).PingHost(context.Background(), toolArgs{Destination: "127.0.0.1"})
			if (err != nil) != tc.wantErr || (!tc.wantErr && !strings.Contains(out, tc.output)) {
				t.Fatalf("ping = %q, %v", out, err)
			}
			if tc.exit == "1" && !strings.Contains(out, "did not receive all requested replies") {
				t.Fatal("packet loss was not marked as an observed diagnostic failure")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Tools{Runner: &remote.Runner{}}).PingHost(ctx, toolArgs{Destination: "127.0.0.1"}); err == nil {
		t.Fatal("cancelled ping succeeded")
	}
}
