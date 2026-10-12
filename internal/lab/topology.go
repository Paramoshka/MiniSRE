package lab

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	OwnerID          string            `yaml:"owner_id"`
	ManagementSubnet string            `yaml:"mgmt_subnet"`
	Images           map[string]string `yaml:"images"`
}

type Topology struct {
	Name  string          `yaml:"name" json:"name"`
	Nodes map[string]Node `yaml:"nodes" json:"nodes"`
	Links [][2]string     `yaml:"links" json:"links"`
}

type Node struct {
	Profile    string              `yaml:"profile" json:"profile"`
	Interfaces map[string][]string `yaml:"interfaces,omitempty" json:"interfaces,omitempty"`
	Routes     []Route             `yaml:"routes,omitempty" json:"routes,omitempty"`
	Forwarding bool                `yaml:"forwarding,omitempty" json:"forwarding,omitempty"`
	FRRConfig  string              `yaml:"frr_config,omitempty" json:"frr_config,omitempty"`
}

type Route struct {
	Destination string `yaml:"destination" json:"destination"`
	Via         string `yaml:"via" json:"via"`
	Interface   string `yaml:"interface" json:"interface"`
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$`)
var interfacePattern = regexp.MustCompile(`^eth[1-9][0-9]?$`)
var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./:_-]*@sha256:[a-f0-9]{64}$`)
var localImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*:local$`)

func validName(name string) bool { return namePattern.MatchString(name) }

func decodeYAML(data string, out any) error {
	if len(data) > MaxTopologyBytes {
		return errors.New("lab: YAML exceeds 256 KiB")
	}
	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(data), &tree); err != nil {
		return errors.New("lab: invalid YAML")
	}
	if err := rejectAliases(&tree); err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return errors.New("lab: invalid YAML fields or types")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("lab: expected one YAML document")
	}
	return nil
}

func rejectAliases(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" || node.Value == "<<" {
		return errors.New("lab: YAML anchors and merges are not supported")
	}
	for _, child := range node.Content {
		if err := rejectAliases(child); err != nil {
			return err
		}
	}
	return nil
}

func ParseConfig(data string) (Config, error) {
	var cfg Config
	if err := decodeYAML(data, &cfg); err != nil {
		return cfg, err
	}
	subnet, err := netip.ParsePrefix(cfg.ManagementSubnet)
	if err != nil || !subnet.Addr().Is4() || !subnet.Addr().IsPrivate() || subnet.Bits() != 24 || subnet != subnet.Masked() || !validName(cfg.OwnerID) {
		return cfg, errors.New("lab: config requires owner_id and a private, canonical IPv4 /24 mgmt_subnet")
	}
	if len(cfg.Images) != 2 {
		return cfg, errors.New("lab: configure exactly frr and alpine images")
	}
	for _, profile := range []string{"frr", "alpine"} {
		image := cfg.Images[profile]
		if !imagePattern.MatchString(image) && !localImagePattern.MatchString(image) {
			return cfg, fmt.Errorf("lab: %s image must be pinned by sha256 digest or use a local name with :local tag", profile)
		}
	}
	return cfg, nil
}

func ParseTopology(data string) (Topology, error) {
	var topo Topology
	if err := decodeYAML(data, &topo); err != nil {
		return topo, err
	}
	if !strings.HasPrefix(topo.Name, "msre-lab-") {
		topo.Name = "msre-lab-" + topo.Name
	}
	if !validName(topo.Name) || topo.Name == "msre-lab-" || len(topo.Nodes) < 1 || len(topo.Nodes) > 8 || len(topo.Links) > 32 {
		return topo, errors.New("lab: valid name, 1..8 nodes and at most 32 links required")
	}
	endpoints := map[string]bool{}
	for _, link := range topo.Links {
		for _, endpoint := range link {
			parts := strings.Split(endpoint, ":")
			if len(parts) != 2 || !interfacePattern.MatchString(parts[1]) {
				return topo, errors.New("lab: links require node:eth1..eth99 endpoints")
			}
			if _, ok := topo.Nodes[parts[0]]; !ok || endpoints[endpoint] {
				return topo, errors.New("lab: unknown or reused link endpoint")
			}
			endpoints[endpoint] = true
		}
		if strings.Split(link[0], ":")[0] == strings.Split(link[1], ":")[0] {
			return topo, errors.New("lab: self links are not supported")
		}
	}
	for name, node := range topo.Nodes {
		if !validName(name) || len(name) > 24 || (node.Profile != "frr" && node.Profile != "alpine") || (node.Profile != "frr" && node.FRRConfig != "") || len(node.FRRConfig) > 64*1024 || strings.ContainsRune(node.FRRConfig, 0) {
			return topo, errors.New("lab: invalid node name, profile or FRR config")
		}
		if err := validateFRRConfig(node.FRRConfig); err != nil {
			return topo, err
		}
		if len(node.Interfaces) > 33 || len(node.Routes) > 64 {
			return topo, errors.New("lab: too many interfaces or routes")
		}
		for iface, addresses := range node.Interfaces {
			if iface != "lo" && !endpoints[name+":"+iface] {
				return topo, errors.New("lab: interface must be lo or a linked data interface")
			}
			if len(addresses) == 0 || len(addresses) > 8 {
				return topo, errors.New("lab: each interface requires 1..8 addresses")
			}
			for _, value := range addresses {
				prefix, err := netip.ParsePrefix(value)
				if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() {
					return topo, errors.New("lab: interface addresses must be IPv4 CIDRs")
				}
			}
		}
		for _, route := range node.Routes {
			dest, err := netip.ParsePrefix(route.Destination)
			via, viaErr := netip.ParseAddr(route.Via)
			if err != nil || !dest.Addr().Is4() || viaErr != nil || !via.Is4() || via.IsUnspecified() || via.IsMulticast() || !endpoints[name+":"+route.Interface] {
				return topo, errors.New("lab: routes require IPv4 destination CIDR, via address and linked interface")
			}
		}
	}
	return topo, nil
}

func renderTopology(topo Topology, cfg Config, dir string) ([]byte, error) {
	nodes := map[string]any{}
	for name, node := range topo.Nodes {
		capabilities := []string{"NET_ADMIN", "NET_RAW"}
		if node.Profile == "frr" {
			capabilities = append(capabilities, "SYS_ADMIN")
		}
		item := map[string]any{
			"kind": "linux", "image": cfg.Images[node.Profile], "image-pull-policy": "Never",
			"memory": "256MiB", "cpu": 0.5, "privileged": false, "cap-add": capabilities,
			"restart-policy": "no", "labels": map[string]string{"mini-sre.owner": cfg.OwnerID, "mini-sre.lab": topo.Name},
		}
		if node.Profile == "alpine" {
			item["cmd"] = "sleep infinity"
		}
		if node.Profile == "frr" {
			item["binds"] = []string{filepath.Join(dir, name+"-frr.conf") + ":/etc/frr/frr.conf:ro", filepath.Join(dir, name+"-daemons") + ":/etc/frr/daemons:ro"}
		}
		forwarding := "0"
		if node.Forwarding {
			forwarding = "1"
		}
		item["sysctls"] = map[string]string{"net.ipv4.ip_forward": forwarding}
		nodes[name] = item
	}
	links := make([]map[string]any, 0, len(topo.Links))
	for _, link := range topo.Links {
		links = append(links, map[string]any{"endpoints": link[:]})
	}
	return yaml.Marshal(map[string]any{
		"name":     topo.Name,
		"mgmt":     map[string]any{"network": topo.Name + "-mgmt", "ipv4-subnet": cfg.ManagementSubnet, "ipv6-subnet": "", "external-access": false},
		"topology": map[string]any{"nodes": nodes, "links": links},
	})
}

// A full vtysh configuration can include shell/file operations. Accept only the
// routing statements needed by this toolset, even inside an unprivileged node.
var frrStatements = []*regexp.Regexp{
	regexp.MustCompile(`^frr defaults (traditional|datacenter)$`),
	regexp.MustCompile(`^frr version [0-9.]+$`),
	regexp.MustCompile(`^hostname [a-zA-Z0-9_-]+$`),
	regexp.MustCompile(`^(service integrated-vtysh-config|log stdout|exit|exit-address-family|address-family ipv4 unicast)$`),
	regexp.MustCompile(`^router (bgp [0-9]+|ospf)$`),
	regexp.MustCompile(`^(bgp router-id|ospf router-id|router-id) [0-9.]+$`),
	regexp.MustCompile(`^neighbor [0-9.]+ (remote-as [0-9]+|activate)$`),
	regexp.MustCompile(`^no bgp (ebgp-requires-policy|network import-check)$`),
	regexp.MustCompile(`^network [0-9./]+( area [0-9.]+)?$`),
	regexp.MustCompile(`^redistribute (connected|static)$`),
	regexp.MustCompile(`^interface (lo|eth[1-9][0-9]?)$`),
	regexp.MustCompile(`^ip ospf area [0-9.]+$`),
	regexp.MustCompile(`^passive-interface (default|lo|eth[1-9][0-9]?)$`),
}

func validateFRRConfig(config string) error {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "#") {
			continue
		}
		allowed := false
		for _, pattern := range frrStatements {
			if pattern.MatchString(line) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("lab: unsupported FRR statement %q", line)
		}
	}
	return nil
}
