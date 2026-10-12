package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

func (m *Manager) preflight(ctx context.Context, topo Topology) error {
	nodes, err := m.containers(ctx, topo.Name)
	if err != nil {
		return err
	}
	if len(nodes) != 0 {
		return errors.New("lab: lab name already exists")
	}
	for _, image := range m.cfg.Images {
		if _, err := m.command(ctx, m.dir, "docker", "image", "inspect", image); err != nil {
			return fmt.Errorf("lab: preload approved image: %w", err)
		}
	}
	data, err := m.readMemory()
	if err != nil {
		return err
	}
	available, err := availableMemory(string(data))
	if err != nil {
		return err
	}
	if available < uint64(len(topo.Nodes)*256+512)*1024*1024 {
		return errors.New("lab: insufficient MemAvailable for node limits plus 512 MiB reserve")
	}
	wanted, err := netip.ParsePrefix(m.cfg.ManagementSubnet)
	if err != nil {
		return err
	}
	for _, node := range topo.Nodes {
		for _, addresses := range node.Interfaces {
			for _, address := range addresses {
				prefix, err := netip.ParsePrefix(address)
				if err != nil {
					return err
				}
				if wanted.Overlaps(prefix) {
					return errors.New("lab: data interfaces overlap the management subnet")
				}
			}
		}
	}
	addresses, err := m.command(ctx, m.dir, "ip", "-j", "-4", "address", "show")
	if err != nil {
		return err
	}
	var interfaces []struct {
		Addresses []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(addresses), &interfaces); err != nil {
		return errors.New("lab: invalid ip address JSON")
	}
	for _, iface := range interfaces {
		for _, address := range iface.Addresses {
			prefix, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", address.Local, address.PrefixLen))
			if err != nil {
				return err
			}
			if wanted.Overlaps(prefix) {
				return fmt.Errorf("lab: management subnet overlaps host address %s", prefix)
			}
		}
	}
	routes, err := m.command(ctx, m.dir, "ip", "-j", "-4", "route", "show", "table", "all")
	if err != nil {
		return err
	}
	var routeList []struct {
		Destination string `json:"dst"`
	}
	if err := json.Unmarshal([]byte(routes), &routeList); err != nil {
		return errors.New("lab: invalid ip route JSON")
	}
	for _, route := range routeList {
		if route.Destination == "default" || route.Destination == "0.0.0.0/0" {
			continue
		}
		value := route.Destination
		if !strings.Contains(value, "/") {
			value += "/32"
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("lab: cannot validate route %q", route.Destination)
		}
		if wanted.Overlaps(prefix) {
			return fmt.Errorf("lab: management subnet overlaps host route %s", prefix)
		}
	}
	ids, err := m.command(ctx, m.dir, "docker", "network", "ls", "-q")
	if err != nil {
		return err
	}
	fields := strings.Fields(ids)
	if len(fields) == 0 {
		return nil
	}
	if len(fields) > 256 {
		return errors.New("lab: too many Docker networks to validate")
	}
	networks, err := m.command(ctx, m.dir, "docker", append([]string{"network", "inspect", "--"}, fields...)...)
	if err != nil {
		return err
	}
	var networkList []struct {
		Name string `json:"Name"`
		IPAM struct {
			Config []struct {
				Subnet string `json:"Subnet"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := json.Unmarshal([]byte(networks), &networkList); err != nil {
		return errors.New("lab: invalid Docker network JSON")
	}
	for _, network := range networkList {
		if network.Name == topo.Name+"-mgmt" {
			return errors.New("lab: management network name already exists")
		}
		for _, item := range network.IPAM.Config {
			if item.Subnet == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(item.Subnet)
			if err != nil {
				return err
			}
			if wanted.Overlaps(prefix) {
				return fmt.Errorf("lab: management subnet overlaps Docker network %s", prefix)
			}
		}
	}
	return nil
}

func availableMemory(data string) (uint64, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || value > ^uint64(0)/1024 {
				return 0, errors.New("lab: invalid MemAvailable")
			}
			return value * 1024, nil
		}
	}
	return 0, errors.New("lab: MemAvailable missing")
}

func readMemory() ([]byte, error) { return os.ReadFile("/proc/meminfo") }
