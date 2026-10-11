package remote

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"mini-sre/internal/config"
)

func TestLocalCommands(t *testing.T) {
	r := &Runner{}
	result, err := r.Run(context.Background(), "local", "printf", "%s", "a ' $HOME; echo nope")
	if err != nil || result.Stdout != "a ' $HOME; echo nope" || result.ExitCode != 0 {
		t.Fatalf("literal arguments: %+v, %v", result, err)
	}
	result, err = r.Run(context.Background(), "local", "sh", "-c", "printf failure >&2; exit 3")
	if err == nil || result.Stderr != "failure" || result.ExitCode != 3 {
		t.Fatalf("exit failure: %+v, %v", result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if result, err := r.Run(ctx, "local", "sleep", "10"); !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != -1 {
		t.Fatalf("cancel command: %v", err)
	}
	if result, err := r.Run(context.Background(), "missing", "true"); err == nil || result.ExitCode != -1 {
		t.Fatal("unknown host accepted")
	}
	if result, err := r.Run(context.Background(), "local", "/nonexistent/mini-sre-command"); err == nil || result.ExitCode != -1 {
		t.Fatalf("missing command: %+v, %v", result, err)
	}
}

func TestOutputLimitAndRedaction(t *testing.T) {
	r := &Runner{Config: config.Config{Hosts: map[string]config.Host{"web": {Password: "secret-password"}}}}
	result, err := r.Run(context.Background(), "local", "sh", "-c", "printf secret-password >&2; head -c 100000 /dev/zero")
	if err != nil || !result.Truncated || len(result.Stdout)+len(result.Stderr) > outputLimit {
		t.Fatalf("output limit: sizes %d/%d, truncated=%v, err=%v", len(result.Stdout), len(result.Stderr), result.Truncated, err)
	}
	if strings.Contains(r.Redact(result.Stderr), "secret-password") {
		t.Fatal("credential leaked")
	}
}

func TestSSHCommands(t *testing.T) {
	for _, useKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("key=%v", useKey), func(t *testing.T) {
			r := testSSH(t, useKey)
			for _, args := range [][]string{
				{"%s", "a ' $HOME; echo nope\nsecond line"},
				{"%s", ""},
			} {
				result, err := r.Run(context.Background(), "web", "printf", args...)
				if err != nil || result.Stdout != args[1] || result.ExitCode != 0 {
					t.Fatalf("SSH literal arguments: %+v, %v", result, err)
				}
			}
			for _, command := range [][]string{{"cat", "/proc/loadavg"}, {"df", "-B1", "--output=size,used,avail", "--", "/"}} {
				local, err := r.Run(context.Background(), "local", command[0], command[1:]...)
				if err != nil {
					t.Fatal(err)
				}
				remote, err := r.Run(context.Background(), "web", command[0], command[1:]...)
				if err != nil || len(strings.Fields(local.Stdout)) != len(strings.Fields(remote.Stdout)) {
					t.Fatalf("local/SSH metrics: %+v, %v", remote, err)
				}
			}
			processes, err := r.Run(context.Background(), "web", "ps", "-eo", "pid,user,pcpu,pmem,rss,comm", "--sort=-rss", "--cols=256")
			if err != nil || !strings.Contains(processes.Stdout, "PID") || !strings.Contains(processes.Stdout, "COMMAND") {
				t.Fatalf("SSH process report: %+v, %v", processes, err)
			}
			result, err := r.Run(context.Background(), "web", "sh", "-c", "printf failure >&2; exit 7")
			if err == nil || result.Stderr != "failure" || result.ExitCode != 7 {
				t.Fatalf("SSH exit failure: %+v, %v", result, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if result, err := r.Run(ctx, "web", "sleep", "10"); !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != -1 {
				t.Fatalf("SSH cancel command: %v", err)
			}
			if err := os.WriteFile(r.Config.KnownHosts, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(context.Background(), "web", "true"); err == nil || !strings.Contains(err.Error(), "host key") {
				t.Fatalf("unknown host key: %v", err)
			}
			_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			wrongSigner, err := ssh.NewSignerFromKey(wrongKey)
			if err != nil {
				t.Fatal(err)
			}
			h := r.Config.Hosts["web"]
			address := net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
			if err := os.WriteFile(r.Config.KnownHosts, []byte(knownhosts.Line([]string{address}, wrongSigner.PublicKey())+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(context.Background(), "web", "true"); err == nil || !strings.Contains(err.Error(), "host key") {
				t.Fatalf("changed host key accepted: %v", err)
			}
		})
	}
}

func TestSSHHandshakeCancellation(t *testing.T) {
	r := testSSH(t, false)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
	}()
	h := r.Config.Hosts["web"]
	h.Port = l.Addr().(*net.TCPAddr).Port
	r.Config.Hosts["web"] = h
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	if _, err := r.Run(ctx, "web", "true"); !errors.Is(err, context.Canceled) {
		t.Fatalf("handshake cancellation: %v", err)
	}
	<-done
}

func TestSSHKnownHostKeyPreference(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	_, edPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edSigner, err := ssh.NewSignerFromKey(edPrivate)
	if err != nil {
		t.Fatal(err)
	}
	rsaPrivate, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaPrivate)
	if err != nil {
		t.Fatal(err)
	}
	for _, keys := range [][]ssh.Signer{
		{edSigner, ecdsaSigner},
		{rsaSigner, ecdsaSigner},
		{ecdsaSigner, rsaSigner},
	} {
		for _, hashed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/hashed=%v", keys[0].PublicKey().Type(), hashed), func(t *testing.T) {
				r := testSSH(t, true, keys...)
				if hashed {
					h := r.Config.Hosts["web"]
					address := net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
					line := knownhosts.Line([]string{address}, keys[0].PublicKey())
					fields := strings.SplitN(line, " ", 2)
					line = knownhosts.HashHostname(fields[0]) + " " + fields[1] + "\n"
					if err := os.WriteFile(r.Config.KnownHosts, []byte(line), 0600); err != nil {
						t.Fatal(err)
					}
				}
				result, err := r.Run(context.Background(), "web", "printf", "%s", "connected")
				if err != nil || result.Stdout != "connected" {
					t.Fatalf("only first server key is trusted: %+v, %v", result, err)
				}
			})
		}
	}
}

func TestSSHHostCertificate(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	hostPrivate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &ssh.Certificate{
		Key: hostSigner.PublicKey(), CertType: ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1"}, ValidBefore: ssh.CertTimeInfinity,
	}
	if err := certificate.SignCert(rand.Reader, authority); err != nil {
		t.Fatal(err)
	}
	certificateSigner, err := ssh.NewCertSigner(certificate, hostSigner)
	if err != nil {
		t.Fatal(err)
	}
	r := testSSH(t, true, certificateSigner, hostSigner)
	h := r.Config.Hosts["web"]
	address := net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
	line := "@cert-authority " + knownhosts.Line([]string{address}, authority.PublicKey()) + "\n"
	if err := os.WriteFile(r.Config.KnownHosts, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "web", "true"); err != nil {
		t.Fatalf("trusted certificate with a different CA key type: %v", err)
	}
}

func testSSH(t *testing.T, useKey bool, hostKeys ...ssh.Signer) *Runner {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if string(password) != "secret-password" {
				return nil, errors.New("bad password")
			}
			return nil, nil
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(signer.PublicKey().Marshal()) {
				return nil, errors.New("bad key")
			}
			return nil, nil
		},
	}
	if len(hostKeys) == 0 {
		hostKeys = []ssh.Signer{signer}
	}
	for _, hostKey := range hostKeys {
		serverConfig.AddHostKey(hostKey)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { serveSSH(conn, serverConfig) })
		}
	})
	t.Cleanup(func() { l.Close(); wg.Wait() })
	dir := t.TempDir()
	knownFile := filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{l.Addr().String()}, hostKeys[0].PublicKey()) + "\n"
	if err := os.WriteFile(knownFile, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	host := config.Host{Address: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, User: "sre", Password: "secret-password"}
	if useKey {
		block, err := ssh.MarshalPrivateKey(private, "test")
		if err != nil {
			t.Fatal(err)
		}
		host.Password = ""
		host.KeyFile = filepath.Join(dir, "key")
		if err := os.WriteFile(host.KeyFile, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &Runner{Config: config.Config{KnownHosts: knownFile, Hosts: map[string]config.Host{"web": host}}}
}

func serveSSH(conn net.Conn, cfg *ssh.ServerConfig) {
	defer conn.Close()
	server, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		server.Wait()
		cancel()
	}()
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		channel, reqs, err := newChannel.Accept()
		if err != nil {
			return
		}
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			defer channel.Close()
			for req := range reqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if ssh.Unmarshal(req.Payload, &payload) != nil {
					req.Reply(false, nil)
					return
				}
				req.Reply(true, nil)
				cmd := exec.CommandContext(ctx, "sh", "-c", "exec env "+payload.Command)
				cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
				cmd.WaitDelay = time.Second
				err := cmd.Run()
				status := 0
				if err != nil {
					status = 1
					var exit *exec.ExitError
					if errors.As(err, &exit) {
						status = exit.ExitCode()
					}
				}
				channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				return
			}
		}()
		<-finished
	}
}

func TestSSHAuthenticationFailure(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	r := testSSH(t, false)
	h := r.Config.Hosts["web"]
	h.Password = "wrong-secret"
	r.Config.Hosts["web"] = h
	_, err := r.Run(context.Background(), "web", "true")
	if err == nil || !strings.Contains(err.Error(), "authentication") || strings.Contains(err.Error(), h.Password) {
		t.Fatalf("authentication failure: %v", err)
	}
	if !strings.Contains(logs.String(), `host="web" phase=handshake`) ||
		!strings.Contains(logs.String(), "attempted methods [none password]") ||
		strings.Contains(logs.String(), h.Password) {
		t.Fatalf("authentication diagnostic: %s", logs.String())
	}
	if got := r.Hosts(); strings.Join(got, ",") != "local,web" {
		t.Fatal(got)
	}
}

func TestSSHLogsRedactConnectionErrors(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	r := testSSH(t, false)
	h := r.Config.Hosts["web"]
	// The dial error includes this invalid port, so it must be redacted in logs.
	h.Port = 65536
	h.Password = "65536"
	r.Config.Hosts["web"] = h
	_, err := r.Run(context.Background(), "web", "true")
	if err == nil || !strings.Contains(err.Error(), "cannot connect") {
		t.Fatalf("connection error: %v", err)
	}
	if !strings.Contains(logs.String(), "phase=connect") ||
		!strings.Contains(logs.String(), "[redacted]") ||
		strings.Contains(logs.String(), h.Password) {
		t.Fatalf("redacted connection diagnostic: %s", logs.String())
	}
}

func TestSSHKeyFailuresDoNotExposeKeyData(t *testing.T) {
	r := testSSH(t, true)
	h := r.Config.Hosts["web"]
	if err := os.WriteFile(h.KeyFile, []byte("invalid-private-key-data"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(context.Background(), "web", "true")
	if err == nil || strings.Contains(err.Error(), "invalid-private-key-data") || strings.Contains(err.Error(), h.KeyFile) {
		t.Fatalf("key read/parse error: %v", err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "test", []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.KeyFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), "web", "true")
	if err == nil || !strings.Contains(err.Error(), "unencrypted") || strings.Contains(err.Error(), "test-passphrase") {
		t.Fatalf("encrypted key error: %v", err)
	}
}
