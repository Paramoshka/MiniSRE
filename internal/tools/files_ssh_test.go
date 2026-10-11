package tools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"mini-sre/internal/config"
	"mini-sre/internal/remote"
)

func fileSSHRunner(t *testing.T) *remote.Runner {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if string(password) != "ssh-test-password" {
			return nil, errors.New("invalid password")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("accept SSH connection: %v", err)
				}
				return
			}
			wg.Go(func() { serveFileSSH(t, ctx, conn, serverConfig) })
		}
	})
	t.Cleanup(func() { cancel(); listener.Close(); wg.Wait() })
	knownFile := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownFile, []byte(knownhosts.Line([]string{listener.Addr().String()}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &remote.Runner{Config: config.Config{KnownHosts: knownFile, Hosts: map[string]config.Host{
		"web": {Address: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "sre", Password: "ssh-test-password"},
	}}}
}

func serveFileSSH(t *testing.T, ctx context.Context, conn net.Conn, cfg *ssh.ServerConfig) {
	t.Helper()
	defer conn.Close()
	server, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		t.Errorf("SSH handshake: %v", err)
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			incoming.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		channel, requests, err := incoming.Accept()
		if err != nil {
			t.Errorf("accept SSH session: %v", err)
			return
		}
		defer channel.Close()
		for request := range requests {
			var payload struct{ Command string }
			if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
				request.Reply(false, nil)
				continue
			}
			request.Reply(true, nil)
			cmd := exec.CommandContext(ctx, "sh", "-c", payload.Command)
			cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
			status := uint32(0)
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Errorf("execute SSH command: %v", err)
					return
				}
				status = uint32(exit.ExitCode())
			}
			channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			return
		}
	}
}

func TestFileToolsSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	path := fileFixture(t, "config ' $HOME; $(printf injected)\nnewline", "first\nLISTEN 443\na ' $HOME; $(printf injected)\n")
	for _, tc := range []struct {
		name, tool, raw, want string
		wantErr               bool
	}{
		{"range", "read_file", `{"start_line":2,"lines":1}`, "LISTEN 443\n", false},
		{"tail", "read_file", `{"tail":true,"lines":1}`, "a ' $HOME; $(printf injected)\n", false},
		{"regex", "search_file", `{"pattern":"^listen [0-9]+$","regex":true,"ignore_case":true}`, "2:LISTEN 443\n", false},
		{"literal", "search_file", `{"pattern":"a ' $HOME; $(printf injected)"}`, "3:a ' $HOME; $(printf injected)\n", false},
		{"no matches", "search_file", `{"pattern":"missing"}`, "No matching lines.", false},
		{"invalid regex", "search_file", `{"pattern":"[","regex":true}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &args); err != nil {
				t.Fatal(err)
			}
			args["host_id"], args["path"] = "web", path
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Registry(runner)[tc.tool](context.Background(), raw)
			if (err != nil) != tc.wantErr || out != tc.want {
				t.Fatalf("SSH %s = %q, %v; want %q, error=%v", tc.tool, out, err, tc.want, tc.wantErr)
			}
		})
	}
	hidden := fileFixture(t, ".env", "secret\n")
	link := filepath.Join(t.TempDir(), "config")
	if err := os.Symlink(hidden, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read_file", "search_file"} {
		raw, err := json.Marshal(map[string]any{"host_id": "web", "path": link, "pattern": "secret"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Registry(runner)[name](context.Background(), raw); err == nil || !strings.Contains(err.Error(), "hidden") {
			t.Fatalf("SSH hidden symlink accepted: %v", err)
		}
	}
}
