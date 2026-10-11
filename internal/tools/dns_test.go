package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestDNSCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		program, raw, want string
		run                func(*Tools, context.Context, toolArgs) (string, error)
	}{
		{"dig", `{"query":"example.com"}`, "-r\n+timeout=2\n+tries=1\n+nosearch\n-q\nexample.com\n-t\nA\n", (*Tools).DigDNS},
		{"dig", `{"query":"_sip._tcp.example.com.","record_type":"srv","server":"::1","port":5353}`, "-r\n+timeout=2\n+tries=1\n+nosearch\n@::1\n-p\n5353\n-q\n_sip._tcp.example.com.\n-t\nSRV\n", (*Tools).DigDNS},
		{"dig", `{"query":"192.0.2.1"}`, "-r\n+timeout=2\n+tries=1\n+nosearch\n-x\n192.0.2.1\n", (*Tools).DigDNS},
		{"nslookup", `{"query":"example.com"}`, "-timeout=2\n-retry=1\n-nosearch\n-type=A\nexample.com\n", (*Tools).NslookupDNS},
		{"nslookup", `{"query":".","record_type":"ns","server":"127.0.0.1","port":65535}`, "-timeout=2\n-retry=1\n-nosearch\n-type=NS\n-port=65535\n.\n127.0.0.1\n", (*Tools).NslookupDNS},
		{"nslookup", `{"query":"2001:db8::1","record_type":"ptr"}`, "-timeout=2\n-retry=1\n-nosearch\n-type=PTR\n2001:db8::1\n", (*Tools).NslookupDNS},
	} {
		t.Run(tc.program+tc.raw, func(t *testing.T) {
			argsFile := fakeCommand(t, tc.program, "DNS output\n", "", "0")
			args, err := decodeArgs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			out, err := tc.run(&Tools{Runner: &remote.Runner{}}, context.Background(), args)
			if err != nil || out != "DNS output\n" {
				t.Fatalf("query = %q, %v", out, err)
			}
			flags, err := os.ReadFile(argsFile)
			if err != nil || string(flags) != tc.want {
				t.Fatalf("arguments = %q, %v; want %q", flags, err, tc.want)
			}
		})
	}
}

func TestDNSRejectsArgumentsBeforeExecution(t *testing.T) {
	invalid := []toolArgs{
		{}, {Query: "-"}, {Query: "-debug"}, {Query: "+trace"}, {Query: "@127.0.0.1"},
		{Query: "a; echo injected"}, {Query: "a\x00b"}, {Query: "a..b"}, {Query: ".."},
		{Query: strings.Repeat("a", 64) + ".com"}, {Query: strings.Repeat("a.", 127) + "a"},
		{Query: "::1%lo"}, {Query: "example.com", RecordType: "AXFR"},
		{Query: "example.com", RecordType: "IXFR=1"}, {Query: "127.0.0.1", RecordType: "A"},
		{Query: "example.com", Server: "dns.example.com"}, {Query: "example.com", Server: "::1%lo"},
		{Query: "example.com", Server: "127.0.0.1; id"},
	}
	for _, port := range []int{0, 65536} {
		invalid = append(invalid, toolArgs{Query: "example.com", Port: &port})
	}
	for _, tc := range []struct {
		program string
		run     func(*Tools, context.Context, toolArgs) (string, error)
	}{{"dig", (*Tools).DigDNS}, {"nslookup", (*Tools).NslookupDNS}} {
		t.Run(tc.program, func(t *testing.T) {
			argsFile := fakeCommand(t, tc.program, "", "", "0")
			for _, args := range invalid {
				if _, err := tc.run(&Tools{Runner: &remote.Runner{}}, context.Background(), args); err == nil {
					t.Fatalf("accepted %+v", args)
				}
			}
			if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
				t.Fatal("command ran before validation")
			}
		})
	}
}

func TestDNSOverSSH(t *testing.T) {
	for _, tc := range []struct {
		program string
		run     func(*Tools, context.Context, toolArgs) (string, error)
	}{{"dig", (*Tools).DigDNS}, {"nslookup", (*Tools).NslookupDNS}} {
		t.Run(tc.program, func(t *testing.T) {
			fakeCommand(t, tc.program, "DNS answer\n", "", "0")
			out, err := tc.run(&Tools{Runner: fileSSHRunner(t)}, context.Background(), toolArgs{HostID: "web", Query: "example.com"})
			if err != nil || out != "DNS answer\n" {
				t.Fatalf("SSH query = %q, %v", out, err)
			}
		})
	}
}

// Serve an A answer or NXDOMAIN on loopback, without external DNS dependencies.
func dnsTestServer(t *testing.T, negative bool) int {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 17 {
				continue
			}
			end := 12
			for end < n && buf[end] != 0 {
				end += int(buf[end]) + 1
			}
			end += 5 // root label, question type and class
			if end > n {
				continue
			}
			response := append([]byte(nil), buf[:end]...)
			binary.BigEndian.PutUint16(response[2:4], 0x8180)
			for i := 6; i < 12; i++ {
				response[i] = 0
			}
			if negative {
				response[3] |= 3
			} else {
				binary.BigEndian.PutUint16(response[6:8], 1)
				response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1)
			}
			if _, err := conn.WriteTo(response, peer); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { conn.Close(); <-done })
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func TestDNSLocalResponses(t *testing.T) {
	for _, tc := range []struct {
		program string
		run     func(*Tools, context.Context, toolArgs) (string, error)
	}{{"dig", (*Tools).DigDNS}, {"nslookup", (*Tools).NslookupDNS}} {
		for _, negative := range []bool{false, true} {
			t.Run(tc.program+"/negative="+strconv.FormatBool(negative), func(t *testing.T) {
				if _, err := exec.LookPath(tc.program); err != nil {
					t.Skip(err)
				}
				port := dnsTestServer(t, negative)
				out, err := tc.run(&Tools{Runner: &remote.Runner{}}, context.Background(), toolArgs{Query: "example.test.", Server: "127.0.0.1", Port: &port})
				if negative {
					if tc.program == "nslookup" {
						if err == nil {
							t.Fatal("nslookup negative response must retain exit error")
						}
						out = err.Error()
					} else if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(out, "NXDOMAIN") {
						t.Fatalf("missing DNS status: %q", out)
					}
				} else if err != nil || !strings.Contains(out, "192.0.2.1") {
					t.Fatalf("answer = %q, %v", out, err)
				}
			})
		}
	}
}
