package lab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const simpleTopology = `name: demo
nodes:
  client:
    profile: alpine
    interfaces:
      eth1: [10.20.0.2/24]
    routes:
      - destination: 10.30.0.0/24
        via: 10.20.0.1
        interface: eth1
  router:
    profile: frr
    forwarding: true
    interfaces:
      eth1: [10.20.0.1/24]
    frr_config: |
      frr defaults traditional
      service integrated-vtysh-config
      router bgp 65001
       bgp router-id 10.20.0.1
       no bgp ebgp-requires-policy
       neighbor 10.20.0.2 remote-as 65002
       address-family ipv4 unicast
        network 10.20.0.0/24
       exit-address-family
      exit
      router ospf
       ospf router-id 10.20.0.1
       network 10.20.0.0/24 area 0
links:
  - [client:eth1, router:eth1]
`

func testConfig() Config {
	return Config{OwnerID: "test-owner", ManagementSubnet: "172.31.250.0/24", Images: map[string]string{"alpine": "registry.example/alpine@sha256:" + strings.Repeat("a", 64), "frr": "registry.example/frr@sha256:" + strings.Repeat("b", 64)}}
}

func TestTopologyValidationAndRender(t *testing.T) {
	topo, err := ParseTopology(simpleTopology)
	if err != nil {
		t.Fatal(err)
	}
	if topo.Name != "msre-lab-demo" {
		t.Fatal(topo.Name)
	}
	cases := []string{
		strings.Replace(simpleTopology, "name: demo", "name: ../escape", 1),
		strings.Replace(simpleTopology, "profile: alpine", "profile: alpine\n    image: attacker", 1),
		strings.Replace(simpleTopology, "profile: alpine", "profile: alpine\n    network-mode: host", 1),
		strings.Replace(simpleTopology, "profile: alpine", "profile: alpine\n    exec: [touch /tmp/escape]", 1),
		strings.Replace(simpleTopology, "profile: alpine", "profile: nokia_srlinux", 1),
		strings.Replace(simpleTopology, "client:eth1", "client:eth0", 1),
		strings.Replace(simpleTopology, "router:eth1", "missing:eth1", 1),
		strings.Replace(simpleTopology, "router:eth1", "client:eth1", 1),
		strings.Replace(simpleTopology, "eth1: [10.20.0.2/24]", "eth2: [10.20.0.2/24]", 1),
		strings.Replace(simpleTopology, "10.20.0.2/24", "10.20.0.2/24; id", 1),
		strings.Replace(simpleTopology, "router bgp 65001", "shell bash", 1),
		strings.Replace(simpleTopology, "router bgp 65001", "include /etc/passwd", 1),
		strings.Replace(simpleTopology, "router bgp 65001", "log file /etc/passwd", 1),
		"name: demo\nnodes: {}\n", "name: demo\nnodes: &alias {}\n", simpleTopology + "---\n{}\n",
		simpleTopology + "links: []\n",
	}
	for i, data := range cases {
		if _, err := ParseTopology(data); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	tooMany := topo
	tooMany.Nodes = make(map[string]Node)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		tooMany.Nodes[name] = Node{Profile: "alpine"}
	}
	data, _ := yaml.Marshal(tooMany)
	if _, err := ParseTopology(string(data)); err == nil {
		t.Fatal("node limit bypassed")
	}
	cfg := testConfig()
	cfg.Images["alpine"] = "mini-sre-alpine:local"
	cfg.Images["frr"] = "mini-sre-frr:local"
	rendered, err := renderTopology(topo, cfg, "/var/lib/mini-sre/labs/"+topo.Name)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Management struct {
			External bool   `yaml:"external-access"`
			Subnet   string `yaml:"ipv4-subnet"`
		} `yaml:"mgmt"`
		Topology struct {
			Nodes map[string]struct {
				Image      string            `yaml:"image"`
				Privileged bool              `yaml:"privileged"`
				Memory     string            `yaml:"memory"`
				CPU        float64           `yaml:"cpu"`
				Pull       string            `yaml:"image-pull-policy"`
				Labels     map[string]string `yaml:"labels"`
			} `yaml:"nodes"`
		} `yaml:"topology"`
	}
	if err := yaml.Unmarshal(rendered, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Management.External || decoded.Management.Subnet != testConfig().ManagementSubnet {
		t.Fatal("management safety settings lost")
	}
	for name, node := range decoded.Topology.Nodes {
		if node.Image != cfg.Images[topo.Nodes[name].Profile] {
			t.Fatalf("wrong image: %+v", node)
		}
		if node.Privileged || node.Memory != "256MiB" || node.CPU != 0.5 || node.Pull != "Never" || node.Labels["mini-sre.owner"] != "test-owner" {
			t.Fatalf("unsafe node: %+v", node)
		}
	}
}

func TestConfigAndRequestValidation(t *testing.T) {
	data, _ := yaml.Marshal(testConfig())
	if _, err := ParseConfig(string(data)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{strings.Replace(string(data), "172.31.250.0/24", "0.0.0.0/0", 1), strings.Replace(string(data), "172.31.250.0/24", "172.31.250.1/24", 1), strings.Replace(string(data), strings.Repeat("a", 64), "bad", 1), string(data) + "unknown: true\n"} {
		if _, err := ParseConfig(invalid); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	for _, req := range []Request{{Action: "inspect"}, {Action: "deploy", Topology: simpleTopology}, {Action: "destroy", Name: "msre-lab-demo"}, {Action: "destroy", AllMine: true}, {Action: "run", Name: "other", Node: "r1", Checks: []Check{{Type: "ping", Destination: "10.0.0.1"}}}} {
		encoded, _ := json.Marshal(req)
		if _, err := DecodeRequest(strings.NewReader(string(encoded))); err != nil {
			t.Fatal(err)
		}
	}
	for _, req := range []Request{{Action: "destroy"}, {Action: "destroy", Name: "other"}, {Action: "destroy", Name: "msre-lab-demo", AllMine: true}, {Action: "deploy", Topology: simpleTopology, TTLMinutes: 121}, {Action: "inspect", Topology: simpleTopology}, {Action: "run", Name: "a", Node: "b", Checks: []Check{{Type: "ping", Destination: "8.8.8.8; id"}}}, {Action: "run", Name: "a", Node: "b", Checks: []Check{{Type: "routes", Destination: "8.8.8.8"}}}} {
		if err := ValidateRequest(req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	for _, data := range []string{`null`, `{"action":"inspect","argv":"id"}`, `{"action":"inspect"} {}`, strings.Repeat("x", MaxRequestBytes+1)} {
		if _, err := DecodeRequest(strings.NewReader(data)); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestConfigImageReferences(t *testing.T) {
	cases := []struct {
		image string
		valid bool
	}{
		{"registry.example/image@sha256:" + strings.Repeat("a", 64), true},
		{"mini-sre-alpine:local", true},
		{"mini-sre-frr:local", true},
		{"", false},
		{"alpine", false},
		{"alpine:latest", false},
		{"registry.example/alpine:local", false},
		{"mini-sre-frr:local@sha256:bad", false},
		{"sha256:" + strings.Repeat("a", 64), false},
		{"image@sha256:" + strings.Repeat("a", 63), false},
		{"image@sha256:" + strings.Repeat("A", 64), false},
		{"-image:local", false},
		{"image:local; id", false},
		{"image:local\n", false},
	}
	for _, profile := range []string{"alpine", "frr"} {
		for _, tc := range cases {
			t.Run(profile+"/"+tc.image, func(t *testing.T) {
				cfg := testConfig()
				cfg.Images[profile] = tc.image
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				_, err = ParseConfig(string(data))
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%v, error=%v", tc.valid, err)
				}
			})
		}
	}
}

func TestLocalImageDeployAndInspect(t *testing.T) {
	data, err := os.ReadFile("../../examples/mini-sre-lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(string(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		name := "preloaded"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			m, fake := testManager(t)
			m.cfg = cfg
			m.run = func(ctx context.Context, dir, program string, args ...string) CommandResult {
				if program == "docker" && args[0] == "image" {
					if len(args) != 3 || args[1] != "inspect" || (args[2] != cfg.Images["alpine"] && args[2] != cfg.Images["frr"]) {
						t.Fatalf("unexpected image command: %v", args)
					}
					if missing {
						return CommandResult{ExitCode: 1, Stderr: "No such image"}
					}
				}
				return fake.run(ctx, dir, program, args...)
			}
			_, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology})
			if missing {
				if err == nil || !strings.Contains(err.Error(), "preload approved image") || fake.deploys != 0 {
					t.Fatalf("missing image: deploys=%d, error=%v", fake.deploys, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response, err := m.Handle(context.Background(), Request{Action: "inspect", Name: "msre-lab-demo"})
			if err != nil {
				t.Fatal(err)
			}
			labs := response.([]LabInfo)
			if len(labs) != 1 || len(labs[0].Nodes) != 2 {
				t.Fatalf("unexpected labs: %+v", labs)
			}
			for _, node := range labs[0].Nodes {
				profile := "alpine"
				if node.Name == "router" {
					profile = "frr"
				}
				if node.Image != cfg.Images[profile] {
					t.Fatalf("unexpected image: %+v", node)
				}
			}
		})
	}
}

func TestProfileCapabilities(t *testing.T) {
	if !validCapabilities("alpine", []string{"CAP_NET_ADMIN", "CAP_NET_RAW"}) || !validCapabilities("frr", []string{"NET_ADMIN", "NET_RAW", "SYS_ADMIN"}) {
		t.Fatal("required capabilities rejected")
	}
	if validCapabilities("alpine", []string{"NET_ADMIN", "NET_RAW", "SYS_ADMIN"}) || validCapabilities("frr", []string{"NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_PTRACE"}) || validCapabilities("frr", []string{"NET_ADMIN", "NET_ADMIN", "SYS_ADMIN"}) {
		t.Fatal("excess or duplicate capabilities accepted")
	}
	data, err := ParseTopology(simpleTopology)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderTopology(data, testConfig(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Topology struct {
			Nodes map[string]struct {
				Capabilities []string `yaml:"cap-add"`
			} `yaml:"nodes"`
		} `yaml:"topology"`
	}
	if err := yaml.Unmarshal(rendered, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, node := range data.Nodes {
		if !validCapabilities(node.Profile, decoded.Topology.Nodes[name].Capabilities) {
			t.Fatal("wrong generated capabilities")
		}
	}
}

func TestDockerCPULimitRepresentations(t *testing.T) {
	node := container{}
	if validCPULimit(node) {
		t.Fatal("unlimited CPU accepted")
	}
	node.HostConfig.CPUQuota, node.HostConfig.CPUPeriod = 50000, 100000
	if !validCPULimit(node) {
		t.Fatal("containerlab CPU quota rejected")
	}
	node.HostConfig.CPUQuota = -1
	if validCPULimit(node) {
		t.Fatal("unlimited quota accepted")
	}
	node.HostConfig.NanoCPUs = 500000000
	if !validCPULimit(node) {
		t.Fatal("Docker NanoCPUs limit rejected")
	}
	node.HostConfig.NanoCPUs = 1000000000
	if validCPULimit(node) {
		t.Fatal("excess CPU accepted")
	}
}

func TestHostCommandBoundsAndEnvironment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	t.Setenv("DOCKER_HOST", "tcp://attacker.example:2375")
	result := execute(ctx, t.TempDir(), "env")
	if result.ExitCode != 0 || strings.Contains(result.Stdout, "DOCKER_HOST") {
		t.Fatalf("unsafe environment %+v", result)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result = execute(ctx, t.TempDir(), "sleep", "10")
	if !result.TimedOut {
		t.Fatalf("command not bounded: %+v", result)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result = execute(ctx, t.TempDir(), "yes")
	if !result.Truncated || len(result.Stdout) > 64*1024 {
		t.Fatal("capture limit lost")
	}
}

func TestTrustedPaths(t *testing.T) {
	info, err := os.Stat("/etc")
	if err != nil {
		t.Fatal(err)
	}
	// Some filesystem sandboxes mask root ownership as nobody.
	if info.Sys().(*syscall.Stat_t).Uid == 0 {
		if err := CheckTrustedPath("/etc", true); err != nil {
			t.Fatal(err)
		}
	} else if err := CheckTrustedPath("/etc", true); err == nil {
		t.Fatal("non-root directory accepted")
	}
	if err := CheckTrustedPath("/tmp", true); err == nil {
		t.Fatal("writable parent accepted")
	}
	dir := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrustedPath(filepath.Join(dir, "link"), true); err == nil {
		t.Fatal("symlink accepted")
	}
}
