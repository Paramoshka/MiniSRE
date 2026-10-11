package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"mini-sre/internal/config"
)

const (
	commandTimeout = 15 * time.Second
	outputLimit    = 64 * 1024
)

type Runner struct {
	Config config.Config
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
	if hostID == "" {
		hostID = "local"
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	budget := &outputBudget{remaining: outputLimit}
	stdout := &limitedBuffer{budget: budget}
	stderr := &limitedBuffer{budget: budget}
	var err error
	if hostID == "local" {
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_COLORS=0")
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.WaitDelay = time.Second
		err = cmd.Run()
	} else {
		err = r.runSSH(ctx, hostID, program, args, stdout, stderr)
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

func (r *Runner) runSSH(ctx context.Context, id, program string, args []string, stdout, stderr *limitedBuffer) error {
	host, ok := r.Config.Hosts[id]
	if !ok {
		return errors.New("unknown host")
	}
	var auth ssh.AuthMethod
	if host.KeyFile != "" {
		key, err := os.ReadFile(host.KeyFile)
		if err != nil {
			log.Printf("SSH host=%q phase=key_file error=%q", r.Redact(id), r.Redact(err.Error()))
			return errors.New("cannot read SSH key file")
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return errors.New("SSH key must be a valid unencrypted private key")
		}
		auth = ssh.PublicKeys(signer)
	} else {
		auth = ssh.Password(host.Password)
	}
	verify, err := knownhosts.New(r.Config.KnownHosts)
	if err != nil {
		log.Printf("SSH host=%q phase=known_hosts error=%q", r.Redact(id), r.Redact(err.Error()))
		return errors.New("cannot load known_hosts file")
	}
	port := host.Port
	if port == 0 {
		port = 22
	}
	address := net.JoinHostPort(host.Address, strconv.Itoa(port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		log.Printf("SSH host=%q phase=connect error=%q", r.Redact(id), r.Redact(err.Error()))
		return errors.New("cannot connect to SSH server")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return errors.New("cannot set SSH deadline")
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User: host.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: verify,
		HostKeyAlgorithms: knownHostAlgorithms(verify, address, conn.RemoteAddr()),
	})
	if err != nil {
		log.Printf("SSH host=%q phase=handshake error=%q", r.Redact(id), r.Redact(err.Error()))
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				return errors.New("SSH host key is unknown in known_hosts")
			}
			return errors.New("SSH host key does not match known_hosts")
		}
		return errors.New("SSH handshake or authentication failed")
	}
	client := ssh.NewClient(clientConn, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		log.Printf("SSH host=%q phase=session error=%q", r.Redact(id), r.Redact(err.Error()))
		return errors.New("cannot open SSH session")
	}
	defer session.Close()
	session.Stdout, session.Stderr = stdout, stderr
	command := "LC_ALL=C SYSTEMD_COLORS=0 " + shellQuote(program)
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	return session.Run(command)
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
	for _, host := range r.Config.Hosts {
		if host.Password != "" {
			value = strings.ReplaceAll(value, host.Password, "[redacted]")
		}
	}
	return value
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
