package remote

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"

	"mini-sre/internal/config"
)

type poolTestSSH struct {
	runner *Runner
	server *testSSHHost
}

func newPoolTestSSH(t *testing.T) *poolTestSSH {
	t.Helper()
	hostKey, _ := newTestSigner(t)
	server := startTestSSH(t, hostKey, nil, "secret-password", nil)
	dir := t.TempDir()
	known := filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{server.listener.Addr().String()}, hostKey.PublicKey()) + "\n"
	if err := os.WriteFile(known, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	host := config.Host{
		Address: "127.0.0.1", Port: server.listener.Addr().(*net.TCPAddr).Port,
		User: "sre", Password: "secret-password",
	}
	return &poolTestSSH{
		runner: &Runner{
			Config: config.Config{KnownHosts: known, Hosts: map[string]config.Host{"web": host}},
			Pool:   newPool(time.Minute),
		},
		server: server,
	}
}

func TestPoolReusesSequentialSSHConnection(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	for i := 0; i < 3; i++ {
		result, err := setup.runner.Run(context.Background(), "web", "printf", "%s", "reused")
		if err != nil || result.Stdout != "reused" {
			t.Fatalf("command %d: %+v, %v", i, result, err)
		}
	}
	if got := setup.server.connections.Load(); got != 1 {
		t.Fatalf("sequential commands opened %d SSH connections, want 1", got)
	}
}

func TestPoolAllowsConcurrentSSHCommands(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	dir := t.TempDir()
	one := filepath.Join(dir, "one")
	two := filepath.Join(dir, "two")
	script := `touch "$1"; while [ ! -f "$2" ]; do sleep 0.01; done`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, args := range [][]string{{one, two}, {two, one}} {
		wg.Go(func() {
			_, err := setup.runner.Run(ctx, "web", "sh", "-c", script, "sh", args[0], args[1])
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := setup.server.connections.Load(); got < 2 {
		t.Fatalf("concurrent commands opened %d SSH connections, want at least 2", got)
	}
}

func TestPoolReusesConnectionAfterCommandExitError(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	if result, err := setup.runner.Run(context.Background(), "web", "sh", "-c", "exit 7"); err == nil || result.ExitCode != 7 {
		t.Fatalf("exit failure: %+v, %v", result, err)
	}
	if result, err := setup.runner.Run(context.Background(), "web", "true"); err != nil || result.ExitCode != 0 {
		t.Fatalf("command after exit failure: %+v, %v", result, err)
	}
	if got := setup.server.connections.Load(); got != 1 {
		t.Fatalf("exit error discarded a healthy connection; connections = %d, want 1", got)
	}
}

func waitForActiveSSH(t *testing.T, host *testSSHHost, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for host.active.Load() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := host.active.Load(); got != want {
		t.Fatalf("active SSH connections = %d, want %d", got, want)
	}
}

func TestPoolDiscardsCanceledSSHConnection(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := setup.runner.Run(ctx, "web", "sleep", "10"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled command: %v", err)
	}
	result, err := setup.runner.Run(context.Background(), "web", "printf", "%s", "after cancel")
	if err != nil || result.Stdout != "after cancel" {
		t.Fatalf("command after cancellation: %+v, %v", result, err)
	}
	if got := setup.server.connections.Load(); got != 2 {
		t.Fatalf("canceled connection was reused; connections = %d, want 2", got)
	}
}

func TestPoolDoesNotReuseConnectionAfterKnownHostsChange(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setup.runner.Config.KnownHosts, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("known_hosts change reused a checked connection: %v", err)
	}
	if got := setup.server.connections.Load(); got != 2 {
		t.Fatalf("known_hosts change opened %d connections, want 2", got)
	}
}

func TestPoolRetriesClosedIdleConnection(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
		t.Fatal(err)
	}
	setup.server.closeConnections()
	waitForActiveSSH(t, setup.server, 0)
	result, err := setup.runner.Run(context.Background(), "web", "printf", "%s", "fresh")
	if err != nil || result.Stdout != "fresh" {
		t.Fatalf("command after idle connection close: %+v, %v", result, err)
	}
	if got := setup.server.connections.Load(); got != 2 {
		t.Fatalf("closed idle connection was not replaced; connections = %d, want 2", got)
	}
}

func TestPoolCapsIdleConnectionsPerHost(t *testing.T) {
	setup := newPoolTestSSH(t)
	t.Cleanup(setup.runner.Pool.Close)
	for i := 0; i < 3; i++ {
		if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
			t.Fatal(err)
		}
		known, err := os.ReadFile(setup.runner.Config.KnownHosts)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(setup.runner.Config.KnownHosts, append(known, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	setup.runner.Pool.mu.Lock()
	idle := setup.runner.Pool.countHostLocked("web")
	setup.runner.Pool.mu.Unlock()
	if idle != maxIdlePerHost {
		t.Fatalf("idle connections for one host = %d, want %d", idle, maxIdlePerHost)
	}
	if got := setup.server.connections.Load(); got != 3 {
		t.Fatalf("configuration changes opened %d connections, want 3", got)
	}
}

func TestPoolReusesJumpChain(t *testing.T) {
	setup := testSSHJump(t, false)
	setup.runner.Pool = newPool(time.Minute)
	t.Cleanup(setup.runner.Pool.Close)
	for i := 0; i < 2; i++ {
		result, err := setup.runner.Run(context.Background(), "web", "true")
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("jump command %d: %+v, %v", i, result, err)
		}
	}
	if got := setup.bastion.connections.Load(); got != 1 {
		t.Fatalf("jump host opened %d connections, want 1", got)
	}
	if got := setup.target.connections.Load(); got != 1 {
		t.Fatalf("target host opened %d connections, want 1", got)
	}
}

func TestPoolExpiresIdleConnections(t *testing.T) {
	setup := newPoolTestSSH(t)
	setup.runner.Pool.Close()
	setup.runner.Pool = newPool(40 * time.Millisecond)
	t.Cleanup(setup.runner.Pool.Close)
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	waitForActiveSSH(t, setup.server, 0)
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
		t.Fatal(err)
	}
	if got := setup.server.connections.Load(); got != 2 {
		t.Fatalf("idle connection was reused; connections = %d, want 2", got)
	}
}

func TestPoolCloseReleasesIdleConnections(t *testing.T) {
	setup := newPoolTestSSH(t)
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
		t.Fatal(err)
	}
	setup.runner.Pool.Close()
	waitForActiveSSH(t, setup.server, 0)
	if _, err := setup.runner.Run(context.Background(), "web", "true"); err != nil {
		t.Fatal(err)
	}
	if got := setup.server.connections.Load(); got != 2 {
		t.Fatalf("closed pool was reused; connections = %d, want 2", got)
	}
}
