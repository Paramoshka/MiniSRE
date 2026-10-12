package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Manager struct {
	cfg        Config
	dir        string
	run        func(context.Context, string, string, ...string) CommandResult
	readMemory func() ([]byte, error)
	now        func() time.Time
}

func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, dir: StateDir, run: execute, readMemory: readMemory, now: time.Now}
}

func (m *Manager) Handle(ctx context.Context, req Request) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := ValidateRequest(req); err != nil {
		return nil, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	records, err := m.records()
	if err != nil {
		return nil, err
	}
	switch req.Action {
	case "deploy":
		return m.deploy(ctx, req, records)
	case "inspect":
		return m.inspect(ctx, req.Name, records)
	case "run":
		return m.runChecks(ctx, req)
	case "destroy":
		removed := []string{}
		for _, record := range records {
			if !req.AllMine && record.Name != req.Name {
				continue
			}
			if err := m.destroy(ctx, record); err != nil {
				return nil, err
			}
			removed = append(removed, record.Name)
		}
		if !req.AllMine && len(removed) == 0 {
			return nil, errors.New("lab: lab is not registered to this helper")
		}
		return map[string]any{"removed": removed}, nil
	}
	return nil, errors.New("lab: unknown action")
}

func (m *Manager) GC(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := m.cleanupStaging(); err != nil {
		return nil, err
	}
	records, err := m.records()
	if err != nil {
		return nil, err
	}
	removed := []string{}
	var failures []error
	for _, record := range records {
		if m.now().Before(record.ExpiresAt) {
			continue
		}
		if err := m.destroy(ctx, record); err != nil {
			failures = append(failures, err)
			continue
		}
		removed = append(removed, record.Name)
	}
	return removed, errors.Join(failures...)
}

func (m *Manager) deploy(ctx context.Context, req Request, records []Record) (any, error) {
	if len(records) != 0 {
		return nil, errors.New("lab: one active lab per host; destroy or clean up the registered lab first")
	}
	topo, err := ParseTopology(req.Topology)
	if err != nil {
		return nil, err
	}
	if err := m.preflight(ctx, topo); err != nil {
		return nil, err
	}
	dir := filepath.Join(m.dir, topo.Name)
	staging, err := os.MkdirTemp(m.dir, "pending-")
	if err != nil {
		return nil, err
	}
	// Wait past the host watchdog before GC can reclaim an interrupted deployment.
	record := Record{Name: topo.Name, Topology: topo, Config: m.cfg, ExpiresAt: m.now().UTC().Add(125 * time.Second)}
	if err := saveRecord(staging, record); err != nil {
		return nil, errors.Join(err, os.RemoveAll(staging))
	}
	if err := os.Rename(staging, dir); err != nil {
		return nil, errors.Join(err, os.RemoveAll(staging))
	}
	if err := syncDir(m.dir); err != nil {
		return nil, err
	}
	if err := prepare(dir, topo, m.cfg); err != nil {
		return nil, m.rollback(ctx, record, err)
	}
	networkID, err := m.command(ctx, dir, "docker", "network", "create", "--driver", "bridge", "--ipv6=false", "--subnet", m.cfg.ManagementSubnet,
		"--label", "containerlab="+topo.Name, "--label", "mini-sre.owner="+m.cfg.OwnerID, "--label", "mini-sre.lab="+topo.Name, topo.Name+"-mgmt")
	if err != nil {
		return nil, m.rollback(ctx, record, err)
	}
	record.NetworkID = strings.TrimSpace(networkID)
	if record.NetworkID == "" {
		return nil, m.rollback(ctx, record, errors.New("lab: Docker returned an empty network ID"))
	}
	if err := saveRecord(dir, record); err != nil {
		return nil, m.rollback(ctx, record, err)
	}
	if _, err := m.command(ctx, dir, "containerlab", "deploy", "--topo", filepath.Join(dir, "topo.clab.yml"), "--max-workers", "1"); err != nil {
		return nil, m.rollback(ctx, record, err)
	}
	if err := m.configure(ctx, topo); err != nil {
		return nil, m.rollback(ctx, record, err)
	}
	ttl := req.TTLMinutes
	if ttl == 0 {
		ttl = 60
	}
	record.ExpiresAt = m.now().UTC().Add(time.Duration(ttl) * time.Minute)
	if err := saveRecord(dir, record); err != nil {
		return nil, m.rollback(ctx, record, err)
	}
	return m.inspect(ctx, topo.Name, []Record{record})
}

func prepare(dir string, topo Topology, cfg Config) error {
	data, err := renderTopology(topo, cfg, dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "topo.clab.yml"), data, 0600); err != nil {
		return err
	}
	for name, node := range topo.Nodes {
		if node.Profile != "frr" {
			continue
		}
		config := node.FRRConfig
		if config == "" {
			config = "frr defaults traditional\nservice integrated-vtysh-config\n"
		}
		// frr drops privileges inside the container and must be able to read these files.
		if err := os.WriteFile(filepath.Join(dir, name+"-frr.conf"), []byte(config), 0644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-daemons"), []byte("zebra=yes\nbgpd=yes\nospfd=yes\nstaticd=yes\nvtysh_enable=yes\nzebra_options=\" -A 127.0.0.1 -s 90000000\"\nbgpd_options=\" -A 127.0.0.1\"\nospfd_options=\" -A 127.0.0.1\"\nstaticd_options=\" -A 127.0.0.1\"\n"), 0644); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) configure(ctx context.Context, topo Topology) error {
	nodes, err := m.containers(ctx, topo.Name)
	if err != nil {
		return err
	}
	if len(nodes) != len(topo.Nodes) {
		return errors.New("lab: deployment returned an incomplete node set")
	}
	for _, container := range nodes {
		name := container.Config.Labels["clab-node-name"]
		node, ok := topo.Nodes[name]
		if !ok || container.Config.Labels["mini-sre.owner"] != m.cfg.OwnerID || container.Config.Labels["mini-sre.lab"] != topo.Name || container.Config.Image != m.cfg.Images[node.Profile] || container.HostConfig.NetworkMode != topo.Name+"-mgmt" || container.HostConfig.Privileged || container.HostConfig.Memory != 256*1024*1024 || !validCPULimit(container) || container.State.Status != "running" || !validCapabilities(node.Profile, container.HostConfig.Capabilities) {
			return errors.New("lab: deployed node failed ownership, state or resource checks")
		}
		// Verify the image provides the bounded diagnostic toolset before claiming readiness.
		// This script is constant; no request text is passed to a shell.
		if err := m.execNode(ctx, container.ID, "/bin/sh", "-c", "command -v ip && command -v ping && command -v traceroute && command -v tcpdump && ip -j address show"); err != nil {
			return fmt.Errorf("lab: node %s lacks compatible diagnostics: %w", name, err)
		}
		if node.Profile == "frr" {
			if err := m.waitFRR(ctx, container.ID); err != nil {
				return err
			}
			if err := m.execNode(ctx, container.ID, "vtysh", "-b"); err != nil {
				return err
			}
		}
		interfaces := make([]string, 0, len(node.Interfaces))
		for iface := range node.Interfaces {
			interfaces = append(interfaces, iface)
		}
		sort.Strings(interfaces)
		for _, iface := range interfaces {
			if err := m.execNode(ctx, container.ID, "ip", "link", "set", "dev", iface, "up"); err != nil {
				return err
			}
			for _, address := range node.Interfaces[iface] {
				if err := m.execNode(ctx, container.ID, "ip", "address", "add", address, "dev", iface); err != nil {
					return err
				}
			}
		}
		for _, route := range node.Routes {
			if err := m.execNode(ctx, container.ID, "ip", "route", "add", route.Destination, "via", route.Via, "dev", route.Interface); err != nil {
				return err
			}
		}
	}
	return nil
}

func validCPULimit(node container) bool {
	limits := node.HostConfig
	if limits.NanoCPUs != 0 {
		return limits.NanoCPUs == 500000000
	}
	return limits.CPUPeriod > 0 && limits.CPUPeriod%2 == 0 && limits.CPUQuota == limits.CPUPeriod/2
}

func validCapabilities(profile string, capabilities []string) bool {
	expected := map[string]bool{"NET_ADMIN": true, "NET_RAW": true}
	if profile == "frr" {
		expected["SYS_ADMIN"] = true
	}
	if len(capabilities) != len(expected) {
		return false
	}
	for _, capability := range capabilities {
		capability = strings.TrimPrefix(capability, "CAP_")
		if !expected[capability] {
			return false
		}
		delete(expected, capability)
	}
	return len(expected) == 0
}

func (m *Manager) execNode(ctx context.Context, id string, command ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := m.command(ctx, m.dir, "docker", append([]string{"exec", id, "timeout", "-k", "1", "14"}, command...)...)
	return err
}

func (m *Manager) rollback(ctx context.Context, record Record, cause error) error {
	if err := m.destroy(ctx, record); err != nil {
		return errors.Join(cause, fmt.Errorf("lab: cleanup pending; TTL collector will retry: %w", err))
	}
	return cause
}

func (m *Manager) destroy(ctx context.Context, record Record) error {
	if err := m.verifyOwned(ctx, record); err != nil {
		return err
	}
	dir := filepath.Join(m.dir, record.Name)
	// Regenerate from the trusted record if a topology file was lost.
	if err := prepare(dir, record.Topology, record.Config); err != nil {
		return err
	}
	if _, err := m.command(ctx, dir, "containerlab", "destroy", "--topo", filepath.Join(dir, "topo.clab.yml"), "--cleanup"); err != nil {
		return err
	}
	remaining, err := m.containers(ctx, record.Name)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("lab: containers remain after destroy; keeping registry")
	}
	network, err := m.managementNetwork(ctx, record)
	if err != nil {
		return err
	}
	if network != nil {
		if len(network.Containers) != 0 {
			return errors.New("lab: management network has attached containers; keeping registry")
		}
		if _, err := m.command(ctx, dir, "docker", "network", "rm", "--", network.ID); err != nil {
			return err
		}
		if network, err := m.managementNetwork(ctx, record); err != nil || network != nil {
			return errors.Join(err, errors.New("lab: management network removal unconfirmed; keeping registry"))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(m.dir)
}

func (m *Manager) waitFRR(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		err := m.execNode(ctx, id, "vtysh", "-c", "show version")
		if err == nil {
			return nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("lab: FRR not ready: %w", err)
		case <-timer.C:
		}
	}
}

func EncodeResponse(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > 60*1024 {
		return nil, errors.New("lab: response exceeds 60 KiB; inspect one lab at a time")
	}
	return data, nil
}
