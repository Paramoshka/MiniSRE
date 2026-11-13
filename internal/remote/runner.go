package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"mini-sre/internal/config"
)

const (
	commandTimeout        = 15 * time.Second
	sshSessionOpenTimeout = 5 * time.Second
	outputLimit           = 64 * 1024
	maxJumpDepth          = 1
)

type Runner struct {
	Config config.Config
	// Pool reuses idle SSH connections between commands. Nil disables reuse.
	Pool *Pool
}

type Result struct {
	Stdout    string
	Stderr    string
	Truncated bool
	ExitCode  int // -1 when no process exit status is available.
}

func (r *Runner) Hosts() []string {
	ids := make([]string, 0, len(r.Config.Hosts)+1)
	ids = append(ids, "local")
	for id := range r.Config.Hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids[1:])
	return ids
}

func (r *Runner) Run(ctx context.Context, hostID, program string, args ...string) (Result, error) {
	return r.run(ctx, hostID, false, program, args...)
}

// RunBounded runs one command with an explicit deadline instead of the
// standard command timeout. It is intended for probes that must outlive the
// 15-second budget, such as a temporary throughput server. Cancellation stops
// the local process group or signals the timeout wrapper; over SSH it also
// closes the transport, and an independent remote watchdog bounds a child that
// survives the disconnect.
func (r *Runner) RunBounded(ctx context.Context, hostID string, timeout time.Duration, program string, args ...string) (Result, error) {
	if timeout <= 0 {
		timeout = commandTimeout
	}
	return r.runInput(ctx, hostID, true, timeout, nil, program, args...)
}

func (r *Runner) run(ctx context.Context, hostID string, processGroup bool, program string, args ...string) (Result, error) {
	return r.runInput(ctx, hostID, processGroup, commandTimeout, nil, program, args...)
}

// RunLab invokes only the installed helper; it does not expose stdin or timeouts to the model.
func (r *Runner) RunLab(ctx context.Context, hostID string, request []byte) (Result, error) {
	return r.runInput(ctx, hostID, true, 125*time.Second, bytes.NewReader(request), "sudo", "-n", "--", "/usr/local/sbin/mini-sre-lab", "request")
}

func (r *Runner) runInput(ctx context.Context, hostID string, processGroup bool, timeout time.Duration, input io.Reader, program string, args ...string) (Result, error) {
	if hostID == "" {
		hostID = "local"
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	budget := &outputBudget{remaining: outputLimit}
	stdout := &limitedBuffer{budget: budget}
	stderr := &limitedBuffer{budget: budget}
	var err error
	if hostID == "local" {
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_COLORS=0")
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.Stdin = input
		cmd.WaitDelay = time.Second
		if processGroup {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				if program == "timeout" {
					// The watchdog handles deadlines. Sending TERM at the same instant can
					// interrupt its shell's cleanup; only forward explicit cancellation.
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						return nil
					}
					return cmd.Process.Signal(syscall.SIGTERM)
				}
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			}
		}
		err = cmd.Run()
	} else {
		err = r.runSSH(ctx, hostID, processGroup, input, program, args, stdout, stderr)
	}
	result := Result{
		Stdout: stdout.String(), Stderr: stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		ExitCode:  -1,
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("command on %q: %w", hostID, ctx.Err())
	}
	if err != nil {
		// The socket deadline may fire before the context timer is scheduled.
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return result, fmt.Errorf("command on %q: %w", hostID, context.DeadlineExceeded)
		}
		var localExit *exec.ExitError
		var remoteExit *ssh.ExitError
		if errors.As(err, &localExit) {
			result.ExitCode = localExit.ExitCode()
		} else if errors.As(err, &remoteExit) {
			result.ExitCode = remoteExit.ExitStatus()
		}
		return result, fmt.Errorf("command %q on %q: %s", program, hostID, r.Redact(err.Error()))
	}
	result.ExitCode = 0
	return result, nil
}

func (r *Runner) runSSH(ctx context.Context, id string, processGroup bool, input io.Reader, program string, args []string, stdout, stderr *limitedBuffer) (err error) {
	key := ""
	if r.Pool != nil {
		if value, ok := connectionKey(r.Config, id); ok {
			key = value
		}
	}
	var conn *sshClient
	var session *ssh.Session
	for attempt := 0; ; attempt++ {
		var fromPool bool
		conn, fromPool, err = r.acquireSSH(ctx, id, key, attempt > 0)
		if err != nil {
			return err
		}
		openCtx := ctx
		var cancel context.CancelFunc
		if fromPool {
			openCtx, cancel = context.WithTimeout(ctx, sshSessionOpenTimeout)
		}
		session, err = newSSHSession(openCtx, conn)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			break
		}
		r.logSSH(id, "session", err)
		conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A pooled connection may have been closed by the server while idle.
		// Retry with a fresh connection only before a command has started.
		if !fromPool || attempt >= 1 {
			return errors.New("cannot open SSH session")
		}
	}
	var signalSession atomic.Pointer[ssh.Session]
	var aborted atomic.Bool
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		aborted.Store(true)
		if session := signalSession.Load(); session != nil {
			// Best effort; the independent remote watchdog handles lost connections.
			_ = conn.transport.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			session.Signal(ssh.SIGTERM)
		}
		conn.transport.Close()
		close(callbackDone)
	})
	reusable := false
	defer func() {
		if !stop() {
			<-callbackDone
		}
		if reusable && !aborted.Load() && ctx.Err() == nil && r.Pool != nil && conn.poolKey != "" {
			// Do not pool a connection when credentials or trust files changed
			// while it was being established or used.
			if current, ok := connectionKey(r.Config, id); ok && current == conn.poolKey {
				r.Pool.put(conn.poolKey, id, conn)
				return
			}
		}
		conn.Close()
	}()
	defer session.Close()
	if processGroup {
		signalSession.Store(session)
	}
	session.Stdout, session.Stderr = stdout, stderr
	session.Stdin = input
	command := "LC_ALL=C SYSTEMD_COLORS=0 " + shellQuote(program)
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	err = session.Run(command)
	var exit *ssh.ExitError
	reusable = err == nil || errors.As(err, &exit)
	return err
}

// newSSHSession opens a session but keeps the connection under the command
// context while waiting, so a dead idle connection cannot hang the probe.
func newSSHSession(ctx context.Context, conn *sshClient) (*ssh.Session, error) {
	var aborted atomic.Bool
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		aborted.Store(true)
		_ = conn.transport.Close()
		close(done)
	})
	session, err := conn.client.NewSession()
	if !stop() {
		<-done
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if session != nil {
			session.Close()
		}
		return nil, ctxErr
	}
	if aborted.Load() {
		if session != nil {
			session.Close()
		}
		return nil, errors.New("SSH session canceled")
	}
	if err != nil {
		return nil, err
	}
	return session, nil
}

// acquireSSH takes an idle connection for this exact host configuration, or
// dials a new one. Pool keys include credentials, keys and known_hosts contents,
// so a configuration change never reuses a connection checked under old trust.
func (r *Runner) acquireSSH(ctx context.Context, id, key string, bypassPool bool) (*sshClient, bool, error) {
	if r.Pool != nil && key != "" && !bypassPool {
		if client := r.Pool.take(key); client != nil {
			client.poolKey = key
			return client, true, nil
		}
	}
	client, err := r.dialSSH(ctx, id, 0)
	if err != nil {
		return nil, false, err
	}
	if r.Pool != nil && key != "" {
		client.poolKey = key
	}
	return client, false, nil
}

type sshClient struct {
	client    *ssh.Client
	transport net.Conn
	parent    *sshClient
	poolKey   string
	closeOnce sync.Once
}

func (c *sshClient) Close() {
	c.closeOnce.Do(func() {
		_ = c.client.Close()
		_ = c.transport.Close()
		if c.parent != nil {
			c.parent.Close()
		}
	})
}

func (r *Runner) dialSSH(ctx context.Context, id string, depth int) (*sshClient, error) {
	if depth > maxJumpDepth {
		return nil, errors.New("SSH jump_host chain is too deep")
	}
	host, ok := r.Config.Hosts[id]
	if !ok {
		return nil, errors.New("unknown host")
	}
	auth, err := r.sshAuth(id, host)
	if err != nil {
		return nil, err
	}
	verify, err := knownhosts.New(r.Config.KnownHosts)
	if err != nil {
		r.logSSH(id, "known_hosts", err)
		return nil, errors.New("cannot load known_hosts file")
	}
	port := host.Port
	if port == 0 {
		port = 22
	}
	address := net.JoinHostPort(host.Address, strconv.Itoa(port))
	var parent *sshClient
	var transport net.Conn
	deadlineSet := false
	if host.JumpHost != "" {
		parent, err = r.dialSSH(ctx, host.JumpHost, depth+1)
		if err != nil {
			return nil, fmt.Errorf("jump host %q: %w", host.JumpHost, err)
		}
		transport, err = parent.client.DialContext(ctx, "tcp", address)
		if err != nil {
			parent.Close()
			r.logSSH(id, "jump_connect", err)
			return nil, errors.New("cannot connect through jump host")
		}
	} else {
		transport, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			r.logSSH(id, "connect", err)
			return nil, errors.New("cannot connect to SSH server")
		}
		if deadline, ok := ctx.Deadline(); ok {
			if err := transport.SetDeadline(deadline); err != nil {
				_ = transport.Close()
				return nil, errors.New("cannot set SSH deadline")
			}
			deadlineSet = true
		}
	}
	var handshakeAborted atomic.Bool
	handshakeDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		handshakeAborted.Store(true)
		_ = transport.Close()
		close(handshakeDone)
	})
	clientConn, channels, requests, err := ssh.NewClientConn(transport, address, &ssh.ClientConfig{
		User: host.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: verify,
		HostKeyAlgorithms: knownHostAlgorithms(verify, address, transport.RemoteAddr()),
	})
	if !stop() {
		<-handshakeDone
	}
	if err != nil {
		_ = transport.Close()
		if parent != nil {
			parent.Close()
		}
		r.logSSH(id, "handshake", err)
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				return nil, errors.New("SSH host key is unknown in known_hosts")
			}
			return nil, errors.New("SSH host key does not match known_hosts")
		}
		return nil, errors.New("SSH handshake or authentication failed")
	}
	if handshakeAborted.Load() || ctx.Err() != nil {
		_ = transport.Close()
		if parent != nil {
			parent.Close()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("SSH handshake canceled")
	}
	if deadlineSet {
		if err := transport.SetDeadline(time.Time{}); err != nil {
			_ = transport.Close()
			if parent != nil {
				parent.Close()
			}
			return nil, errors.New("cannot reset SSH deadline")
		}
	}
	client := ssh.NewClient(clientConn, channels, requests)
	return &sshClient{client: client, transport: transport, parent: parent}, nil
}

func (r *Runner) sshAuth(id string, host config.Host) (ssh.AuthMethod, error) {
	if host.KeyFile != "" {
		key, err := os.ReadFile(host.KeyFile)
		if err != nil {
			r.logSSH(id, "key_file", err)
			return nil, errors.New("cannot read SSH key file")
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, errors.New("SSH key must be a valid unencrypted private key")
		}
		return ssh.PublicKeys(signer), nil
	}
	return ssh.Password(host.Password), nil
}

func (r *Runner) logSSH(id, phase string, err error) {
	if host, ok := r.Config.Hosts[id]; ok && host.JumpHost != "" {
		log.Printf("SSH host=%q via=%q phase=%s error=%q", r.Redact(id), r.Redact(host.JumpHost), phase, r.Redact(err.Error()))
		return
	}
	log.Printf("SSH host=%q phase=%s error=%q", r.Redact(id), phase, r.Redact(err.Error()))
}

func knownHostAlgorithms(verify ssh.HostKeyCallback, address string, remote net.Addr) []string {
	// A deliberately invalid key makes knownhosts return all keys for this address.
	// Reuse its matching rules for hashed hosts, aliases, and nonstandard ports.
	var keyErr *knownhosts.KeyError
	if !errors.As(verify(address, remote, hostKeyProbe{}), &keyErr) || len(keyErr.Want) == 0 {
		return nil
	}
	knownTypes := make(map[string]bool)
	for _, key := range keyErr.Want {
		knownTypes[key.Key.Type()] = true
	}
	if knownTypes[ssh.KeyAlgoRSA] {
		knownTypes[ssh.KeyAlgoRSASHA256] = true
		knownTypes[ssh.KeyAlgoRSASHA512] = true
	}
	algorithms := append(ssh.SupportedAlgorithms().HostKeys, ssh.InsecureAlgorithms().HostKeys...)
	priority := func(algorithm string) int {
		// Preserve certificate negotiation for @cert-authority entries.
		if strings.Contains(algorithm, "-cert-") {
			return 0
		}
		if knownTypes[algorithm] {
			return 1
		}
		return 2
	}
	sort.SliceStable(algorithms, func(i, j int) bool {
		return priority(algorithms[i]) < priority(algorithms[j])
	})
	return algorithms
}

type hostKeyProbe struct{}

func (hostKeyProbe) Type() string    { return "" }
func (hostKeyProbe) Marshal() []byte { return nil }
func (hostKeyProbe) Verify([]byte, *ssh.Signature) error {
	return errors.New("host key probe cannot verify signatures")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (r *Runner) Redact(value string) string {
	var passwords []string
	for _, host := range r.Config.Hosts {
		passwords = append(passwords, host.Password)
	}
	return RedactRuntimeSecrets(value, passwords...)
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	budget    *outputBudget
	truncated bool
}

type outputBudget struct {
	sync.Mutex
	remaining int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.budget.Lock()
	defer b.budget.Unlock()
	if n > b.budget.remaining {
		p = p[:b.budget.remaining]
		b.truncated = true
	}
	b.budget.remaining -= len(p)
	_, err := b.buffer.Write(p)
	return n, err
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}
