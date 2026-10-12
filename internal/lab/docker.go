package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type container struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode  string   `json:"NetworkMode"`
		Memory       int64    `json:"Memory"`
		NanoCPUs     int64    `json:"NanoCpus"`
		CPUQuota     int64    `json:"CpuQuota"`
		CPUPeriod    int64    `json:"CpuPeriod"`
		Privileged   bool     `json:"Privileged"`
		Capabilities []string `json:"CapAdd"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress         string `json:"IPAddress"`
			GlobalIPv6Address string `json:"GlobalIPv6Address"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type NodeInfo struct {
	Name          string   `json:"name"`
	ContainerID   string   `json:"container_id"`
	Image         string   `json:"image"`
	Status        string   `json:"status"`
	ManagementIPs []string `json:"management_ips"`
	NetworkMode   string   `json:"network_mode"`
}

type LabInfo struct {
	Name      string      `json:"name"`
	Managed   bool        `json:"managed"`
	ExpiresAt *time.Time  `json:"expires_at,omitempty"`
	Links     [][2]string `json:"links,omitempty"`
	Nodes     []NodeInfo  `json:"nodes"`
}

type managementNetwork struct {
	ID         string                     `json:"Id"`
	Name       string                     `json:"Name"`
	Labels     map[string]string          `json:"Labels"`
	Containers map[string]json.RawMessage `json:"Containers"`
}

func (m *Manager) managementNetwork(ctx context.Context, record Record) (*managementNetwork, error) {
	ids, err := m.command(ctx, m.dir, "docker", "network", "ls", "-q", "--filter", "name=^"+record.Name+"-mgmt$")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(ids)
	if len(fields) == 0 {
		return nil, nil
	}
	if len(fields) != 1 {
		return nil, errors.New("lab: ambiguous management network")
	}
	data, err := m.command(ctx, m.dir, "docker", "network", "inspect", "--", fields[0])
	if err != nil {
		return nil, err
	}
	var networks []managementNetwork
	if err := json.Unmarshal([]byte(data), &networks); err != nil || len(networks) != 1 {
		return nil, errors.New("lab: invalid management network JSON")
	}
	network := networks[0]
	if network.Name != record.Name+"-mgmt" || network.Labels["mini-sre.owner"] != record.Config.OwnerID || network.Labels["mini-sre.lab"] != record.Name || (record.NetworkID != "" && network.ID != record.NetworkID) {
		return nil, errors.New("lab: foreign or replaced management network; refusing deletion")
	}
	return &network, nil
}

func (m *Manager) containers(ctx context.Context, name string) ([]container, error) {
	filter := "label=containerlab"
	if name != "" {
		filter += "=" + name
	}
	ids, err := m.command(ctx, m.dir, "docker", "ps", "-aq", "--filter", filter)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(ids)
	if len(fields) == 0 {
		return []container{}, nil
	}
	if len(fields) > 256 {
		return nil, errors.New("lab: too many containerlab nodes to inspect")
	}
	nodes := make([]container, 0, len(fields))
	for _, id := range fields {
		data, err := m.command(ctx, m.dir, "docker", "inspect", "--", id)
		if err != nil {
			return nil, err
		}
		var inspected []container
		if err := json.Unmarshal([]byte(data), &inspected); err != nil || len(inspected) != 1 {
			return nil, errors.New("lab: invalid Docker inspect JSON")
		}
		nodes = append(nodes, inspected[0])
	}
	return nodes, nil
}

func (m *Manager) inspect(ctx context.Context, name string, records []Record) ([]LabInfo, error) {
	nodes, err := m.containers(ctx, name)
	if err != nil {
		return nil, err
	}
	labs := []LabInfo{}
	indexes := map[string]int{}
	for _, record := range records {
		if name != "" && record.Name != name {
			continue
		}
		indexes[record.Name] = len(labs)
		expires := record.ExpiresAt
		labs = append(labs, LabInfo{Name: record.Name, Managed: true, ExpiresAt: &expires, Links: record.Topology.Links, Nodes: []NodeInfo{}})
	}
	for _, node := range nodes {
		labName := node.Config.Labels["containerlab"]
		index, ok := indexes[labName]
		if !ok {
			index = len(labs)
			indexes[labName] = index
			labs = append(labs, LabInfo{Name: labName, Nodes: []NodeInfo{}})
		}
		info := NodeInfo{Name: node.Config.Labels["clab-node-name"], ContainerID: node.ID, Image: node.Config.Image, Status: node.State.Status, NetworkMode: node.HostConfig.NetworkMode, ManagementIPs: []string{}}
		for _, network := range node.NetworkSettings.Networks {
			if network.IPAddress != "" {
				info.ManagementIPs = append(info.ManagementIPs, network.IPAddress)
			}
			if network.GlobalIPv6Address != "" {
				info.ManagementIPs = append(info.ManagementIPs, network.GlobalIPv6Address)
			}
		}
		labs[index].Nodes = append(labs[index].Nodes, info)
	}
	return labs, nil
}

func (m *Manager) runChecks(ctx context.Context, req Request) ([]CheckResult, error) {
	nodes, err := m.containers(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	var target *container
	for i := range nodes {
		if nodes[i].Config.Labels["clab-node-name"] == req.Node {
			if target != nil {
				return nil, errors.New("lab: ambiguous node")
			}
			target = &nodes[i]
		}
	}
	if target == nil {
		return nil, errors.New("lab: node not found")
	}
	if target.HostConfig.NetworkMode == "host" || strings.HasPrefix(target.HostConfig.NetworkMode, "container:") {
		return nil, errors.New("lab: diagnostics forbid host/shared container networking")
	}
	results := make([]CheckResult, 0, len(req.Checks))
	for _, check := range req.Checks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		command, err := checkCommand(check)
		if err != nil {
			return nil, err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		args := append([]string{"exec", target.ID, "timeout", "-k", "1", "14"}, command...)
		result := m.run(checkCtx, m.dir, "docker", args...)
		cancel()
		if result.ExitCode == 124 || result.ExitCode == 137 {
			result.TimedOut = true
		}
		// Bound the aggregate JSON response, including four escaped previews.
		if len(result.Stdout) > 1024 {
			result.Stdout = result.Stdout[:1024]
			result.Truncated = true
		}
		if len(result.Stderr) > 1024 {
			result.Stderr = result.Stderr[:1024]
			result.Truncated = true
		}
		results = append(results, CheckResult{Check: check, CommandResult: result})
	}
	return results, nil
}

func (m *Manager) verifyOwned(ctx context.Context, record Record) error {
	nodes, err := m.containers(ctx, record.Name)
	if err != nil {
		return err
	}
	ownedIDs := map[string]bool{}
	for _, node := range nodes {
		name := node.Config.Labels["clab-node-name"]
		if _, ok := record.Topology.Nodes[name]; !ok || node.Config.Labels["mini-sre.owner"] != record.Config.OwnerID || node.Config.Labels["mini-sre.lab"] != record.Name {
			return fmt.Errorf("lab: foreign node in %s; refusing removal", record.Name)
		}
		ownedIDs[node.ID] = true
	}
	network, err := m.managementNetwork(ctx, record)
	if err != nil {
		return err
	}
	if network != nil {
		for id := range network.Containers {
			if !ownedIDs[id] {
				return errors.New("lab: foreign container attached to management network; refusing deletion")
			}
		}
	}
	return nil
}
