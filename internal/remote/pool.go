package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"mini-sre/internal/config"
)

const (
	defaultIdleTimeout = 60 * time.Second
	maxIdlePerHost     = 2
	maxIdleConnections = 32
)

// Pool keeps a bounded set of idle SSH connection chains. A connection serves
// one command at a time; concurrent commands use separate connections.
type Pool struct {
	mu          sync.Mutex
	idle        map[string][]*idleConn
	count       int
	closed      bool
	idleTimeout time.Duration
	done        chan struct{}
	closeOnce   sync.Once
}

type idleConn struct {
	client *sshClient
	host   string
	idleAt time.Time
}

// NewPool creates a pool that expires idle connections after 60 seconds.
func NewPool() *Pool {
	return newPool(defaultIdleTimeout)
}

func newPool(idleTimeout time.Duration) *Pool {
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeout
	}
	p := &Pool{
		idle:        make(map[string][]*idleConn),
		idleTimeout: idleTimeout,
		done:        make(chan struct{}),
	}
	go p.janitor()
	return p
}

func (p *Pool) take(key string) *sshClient {
	now := time.Now()
	var stale []*sshClient
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	list := p.idle[key]
	for len(list) > 0 {
		entry := list[len(list)-1]
		list = list[:len(list)-1]
		p.count--
		if now.Sub(entry.idleAt) >= p.idleTimeout {
			stale = append(stale, entry.client)
			continue
		}
		if len(list) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = list
		}
		p.mu.Unlock()
		for _, client := range stale {
			client.Close()
		}
		return entry.client
	}
	delete(p.idle, key)
	p.mu.Unlock()
	for _, client := range stale {
		client.Close()
	}
	return nil
}

func (p *Pool) put(key, host string, client *sshClient) {
	var closeList []*sshClient
	p.mu.Lock()
	if p.closed {
		closeList = append(closeList, client)
	} else {
		for p.countHostLocked(host) >= maxIdlePerHost {
			oldKey, old := p.oldestHostLocked(host)
			if old == nil {
				break
			}
			p.removeLocked(oldKey, old)
			closeList = append(closeList, old.client)
		}
		if p.count >= maxIdleConnections {
			if oldKey, old := p.oldestLocked(); old != nil {
				p.removeLocked(oldKey, old)
				closeList = append(closeList, old.client)
			}
		}
		p.idle[key] = append(p.idle[key], &idleConn{client: client, host: host, idleAt: time.Now()})
		p.count++
	}
	p.mu.Unlock()
	for _, client := range closeList {
		client.Close()
	}
}

func (p *Pool) removeLocked(key string, target *idleConn) {
	list := p.idle[key]
	for i, entry := range list {
		if entry == target {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(p.idle, key)
	} else {
		p.idle[key] = list
	}
	p.count--
}

func (p *Pool) countHostLocked(host string) int {
	count := 0
	for _, list := range p.idle {
		for _, entry := range list {
			if entry.host == host {
				count++
			}
		}
	}
	return count
}

func (p *Pool) oldestHostLocked(host string) (string, *idleConn) {
	var oldest *idleConn
	var oldestKey string
	for key, list := range p.idle {
		for _, entry := range list {
			if entry.host != host {
				continue
			}
			if oldest == nil || entry.idleAt.Before(oldest.idleAt) {
				oldest = entry
				oldestKey = key
			}
		}
	}
	return oldestKey, oldest
}

func (p *Pool) oldestLocked() (string, *idleConn) {
	var oldest *idleConn
	var oldestKey string
	for key, list := range p.idle {
		for _, entry := range list {
			if oldest == nil || entry.idleAt.Before(oldest.idleAt) {
				oldest = entry
				oldestKey = key
			}
		}
	}
	return oldestKey, oldest
}

func (p *Pool) janitor() {
	interval := p.idleTimeout / 2
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case now := <-ticker.C:
			p.expire(now)
		}
	}
}

func (p *Pool) expire(now time.Time) {
	var stale []*sshClient
	p.mu.Lock()
	for key, list := range p.idle {
		var kept []*idleConn
		for _, entry := range list {
			if now.Sub(entry.idleAt) >= p.idleTimeout {
				stale = append(stale, entry.client)
				p.count--
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(p.idle, key)
		} else {
			p.idle[key] = kept
		}
	}
	p.mu.Unlock()
	for _, client := range stale {
		client.Close()
	}
}

// Close stops the cleanup loop, closes every idle connection and prevents new
// connections from being retained.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.mu.Lock()
		p.closed = true
		var clients []*sshClient
		for _, list := range p.idle {
			for _, entry := range list {
				clients = append(clients, entry.client)
			}
		}
		p.idle = nil
		p.count = 0
		p.mu.Unlock()
		for _, client := range clients {
			client.Close()
		}
	})
}

// connectionKey identifies a connection chain together with every credential
// and trust input used to establish it. Changed files or host settings produce
// a new key, so pooled connections are never reused under stale configuration.
func connectionKey(cfg config.Config, hostID string) (string, bool) {
	h := sha256.New()
	write := func(values ...string) {
		for _, value := range values {
			fmt.Fprintf(h, "%d:", len(value))
			h.Write([]byte(value))
		}
	}
	write("known_hosts", cfg.KnownHosts)
	knownHosts, err := os.ReadFile(cfg.KnownHosts)
	if err != nil {
		return "", false
	}
	write(string(knownHosts))
	id := hostID
	for depth := 0; depth <= maxJumpDepth; depth++ {
		host, ok := cfg.Hosts[id]
		if !ok {
			return "", false
		}
		port := host.Port
		if port == 0 {
			port = 22
		}
		write("host", id, host.Address, strconv.Itoa(port), host.User, host.KeyFile, host.Password, host.JumpHost)
		if host.KeyFile != "" {
			key, err := os.ReadFile(host.KeyFile)
			if err != nil {
				return "", false
			}
			write(string(key))
		}
		if host.JumpHost == "" {
			return hex.EncodeToString(h.Sum(nil)), true
		}
		id = host.JumpHost
	}
	return "", false
}
