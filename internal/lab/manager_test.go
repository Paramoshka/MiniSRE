package lab

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeHost struct {
	t                                         *testing.T
	manager                                   *Manager
	nodes                                     []container
	deployFails, destroyFails, networkRemains bool
	deploys, destroys                         int
	routes, addresses, networks               string
	probe                                     CommandResult
	ownedNetwork                              *managementNetwork
}

func testManager(t *testing.T) (*Manager, *fakeHost) {
	t.Helper()
	m := New(testConfig())
	m.dir = t.TempDir()
	now := time.Now().UTC()
	m.now = func() time.Time { return now }
	m.readMemory = func() ([]byte, error) { return []byte("MemAvailable: 4194304 kB\n"), nil }
	foreign := container{ID: "foreign"}
	foreign.Config.Labels = map[string]string{"containerlab": "existing", "clab-node-name": "r1"}
	foreign.State.Status = "running"
	fake := &fakeHost{t: t, manager: m, nodes: []container{foreign}, routes: `[{"dst":"default"},{"dst":"192.168.1.0/24"}]`, addresses: `[{"addr_info":[{"local":"192.168.1.10","prefixlen":24}]}]`, networks: `[{"Name":"bridge","IPAM":{"Config":[{"Subnet":"172.17.0.0/16"}]}}]`, probe: CommandResult{Stdout: "probe", ExitCode: 0}}
	m.run = fake.run
	return m, fake
}

func (f *fakeHost) run(ctx context.Context, dir, program string, args ...string) CommandResult {
	if ctx.Err() != nil {
		return CommandResult{ExitCode: -1, TimedOut: true}
	}
	encode := func(value any) CommandResult {
		data, err := json.Marshal(value)
		if err != nil {
			f.t.Fatal(err)
		}
		return CommandResult{Stdout: string(data)}
	}
	if program == "ip" {
		if strings.Contains(strings.Join(args, " "), "address") {
			return CommandResult{Stdout: f.addresses}
		}
		return CommandResult{Stdout: f.routes}
	}
	if program == "containerlab" {
		if args[0] == "deploy" {
			f.deploys++
			records, err := f.manager.records()
			if err != nil || len(records) != 1 {
				f.t.Fatalf("deployment preceded registry: %v", err)
			}
			if !records[0].ExpiresAt.Equal(f.manager.now().Add(125 * time.Second)) {
				f.t.Fatal("pending lab can be reclaimed before watchdog")
			}
			for name, node := range records[0].Topology.Nodes {
				item := container{ID: name}
				item.Config.Labels = map[string]string{"containerlab": records[0].Name, "clab-node-name": name, "mini-sre.owner": f.manager.cfg.OwnerID, "mini-sre.lab": records[0].Name}
				item.Config.Image = f.manager.cfg.Images[node.Profile]
				item.State.Status = "running"
				item.HostConfig.Memory = 256 * 1024 * 1024
				item.HostConfig.CPUQuota = 50000
				item.HostConfig.CPUPeriod = 100000
				item.HostConfig.NetworkMode = records[0].Name + "-mgmt"
				item.HostConfig.Capabilities = []string{"NET_ADMIN", "NET_RAW"}
				if node.Profile == "frr" {
					item.HostConfig.Capabilities = append(item.HostConfig.Capabilities, "SYS_ADMIN")
				}
				f.nodes = append(f.nodes, item)
			}
			if f.deployFails {
				return CommandResult{ExitCode: 1, Stderr: "partial deployment"}
			}
			return CommandResult{}
		}
		f.destroys++
		if f.destroyFails {
			return CommandResult{ExitCode: 1, Stderr: "Docker unavailable"}
		}
		if strings.Contains(strings.Join(args, " "), "--all") {
			f.t.Fatal("global deletion used")
		}
		kept := []container{}
		for _, node := range f.nodes {
			if node.Config.Labels["containerlab"] != "msre-lab-demo" {
				kept = append(kept, node)
			}
		}
		f.nodes = kept
		return CommandResult{}
	}
	if program == "docker" {
		switch args[0] {
		case "ps":
			filter := args[len(args)-1]
			var ids []string
			for _, node := range f.nodes {
				if filter == "label=containerlab" || filter == "label=containerlab="+node.Config.Labels["containerlab"] {
					ids = append(ids, node.ID)
				}
			}
			return CommandResult{Stdout: strings.Join(ids, "\n")}
		case "inspect":
			selected := []container{}
			for _, node := range f.nodes {
				for _, id := range args[2:] {
					if node.ID == id {
						selected = append(selected, node)
					}
				}
			}
			return encode(selected)
		case "image":
			return CommandResult{Stdout: `[]`}
		case "network":
			if args[1] == "create" {
				f.ownedNetwork = &managementNetwork{ID: "owned-network", Name: args[len(args)-1], Labels: map[string]string{"mini-sre.owner": f.manager.cfg.OwnerID, "mini-sre.lab": "msre-lab-demo"}, Containers: map[string]json.RawMessage{}}
				return CommandResult{Stdout: f.ownedNetwork.ID}
			}
			if args[1] == "rm" {
				if f.networkRemains {
					return CommandResult{ExitCode: 1, Stderr: "network removal failed"}
				}
				f.ownedNetwork = nil
				return CommandResult{}
			}
			if args[1] == "inspect" {
				if f.ownedNetwork != nil && args[len(args)-1] == f.ownedNetwork.ID {
					return encode([]managementNetwork{*f.ownedNetwork})
				}
				return CommandResult{Stdout: f.networks}
			}
			if len(args) > 3 {
				if f.ownedNetwork != nil {
					return CommandResult{Stdout: f.ownedNetwork.ID}
				}
				return CommandResult{}
			}
			return CommandResult{Stdout: "bridge"}
		case "exec":
			if args[1] == "foreign" {
				return f.probe
			}
			return CommandResult{Stdout: `[]`}
		}
	}
	f.t.Fatalf("unexpected command: %s %v", program, args)
	return CommandResult{ExitCode: 1}
}

func TestLifecycleAndTTLWithoutBot(t *testing.T) {
	m, fake := testManager(t)
	response, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology})
	if err != nil {
		t.Fatal(err)
	}
	labs := response.([]LabInfo)
	if len(labs) != 1 || len(labs[0].Nodes) != 2 || !labs[0].ExpiresAt.Equal(m.now().Add(time.Hour)) {
		t.Fatalf("deployment response %+v", response)
	}
	if _, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology}); err == nil || fake.deploys != 1 {
		t.Fatal("second lab deployed")
	}
	if _, err := m.Handle(context.Background(), Request{Action: "destroy", Name: "msre-lab-foreign"}); err == nil || fake.destroys != 0 {
		t.Fatal("unregistered lab deleted")
	}
	if err := os.Remove(filepath.Join(m.dir, "msre-lab-demo", "topo.clab.yml")); err != nil {
		t.Fatal(err)
	}
	collector := New(m.cfg)
	collector.dir, collector.run = m.dir, m.run
	collector.now = func() time.Time { return m.now().Add(time.Hour + time.Minute) }
	removed, err := collector.GC(context.Background())
	if err != nil || strings.Join(removed, "") != "msre-lab-demo" {
		t.Fatalf("GC failed: %v %v", removed, err)
	}
	if len(fake.nodes) != 1 || fake.nodes[0].ID != "foreign" {
		t.Fatal("foreign lab affected")
	}
	records, err := m.records()
	if err != nil || len(records) != 0 {
		t.Fatal("registry not cleaned")
	}
}

func TestPartialDeploymentAndRetryCleanup(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		m, fake := testManager(t)
		fake.deployFails, fake.destroyFails = true, cleanupFails
		if _, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology}); err == nil {
			t.Fatal("partial deployment succeeded")
		}
		records, err := m.records()
		if err != nil || (len(records) != 0) != cleanupFails {
			t.Fatalf("registry after rollback: %v %v", records, err)
		}
		if cleanupFails {
			fake.destroyFails = false
			now := m.now().Add(3 * time.Minute)
			m.now = func() time.Time { return now }
			if _, err := m.GC(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if len(fake.nodes) != 1 || fake.nodes[0].ID != "foreign" {
			t.Fatal("cleanup left nodes or removed foreign lab")
		}
	}
}

func TestForeignOwnershipAndRemainingNetworkKeepRegistry(t *testing.T) {
	for _, problem := range []string{"owner", "network", "network owner", "network replacement", "foreign attachment"} {
		m, fake := testManager(t)
		if _, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology}); err != nil {
			t.Fatal(err)
		}
		switch problem {
		case "owner":
			fake.nodes[1].Config.Labels["mini-sre.owner"] = "other"
		case "network":
			fake.networkRemains = true
		case "network owner":
			fake.ownedNetwork.Labels["mini-sre.owner"] = "other"
		case "network replacement":
			fake.ownedNetwork.ID = "replacement-network"
		case "foreign attachment":
			fake.ownedNetwork.Containers["foreign"] = json.RawMessage(`{}`)
		}
		if _, err := m.Handle(context.Background(), Request{Action: "destroy", AllMine: true}); err == nil {
			t.Fatal("unsafe cleanup accepted")
		}
		if problem != "network" && fake.destroys != 0 {
			t.Fatal("foreign resources touched")
		}
		records, err := m.records()
		if err != nil || len(records) != 1 {
			t.Fatal("cleanup lost registry")
		}
	}
}

func TestPreflightFailuresHaveNoSideEffects(t *testing.T) {
	for _, problem := range []string{"memory", "address", "route", "network", "network name", "bad JSON", "data network"} {
		m, fake := testManager(t)
		topo := simpleTopology
		switch problem {
		case "memory":
			m.readMemory = func() ([]byte, error) { return []byte("MemAvailable: 1 kB\n"), nil }
		case "address":
			fake.addresses = `[{"addr_info":[{"local":"172.31.250.2","prefixlen":24}]}]`
		case "route":
			fake.routes = `[{"dst":"172.31.0.0/16"}]`
		case "network":
			fake.networks = `[{"IPAM":{"Config":[{"Subnet":"172.31.250.0/24"}]}}]`
		case "network name":
			fake.networks = `[{"Name":"msre-lab-demo-mgmt","IPAM":{"Config":[]}}]`
		case "bad JSON":
			fake.routes = `invalid`
		case "data network":
			topo = strings.ReplaceAll(topo, "10.20.0.", "172.31.250.")
		}
		if _, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: topo}); err == nil {
			t.Fatalf("preflight %s accepted", problem)
		}
		records, err := m.records()
		if err != nil || len(records) != 0 || fake.deploys != 0 {
			t.Fatal("preflight created resources")
		}
	}
}

func TestChecksNamespacesTimeoutsAndTruncation(t *testing.T) {
	m, fake := testManager(t)
	req := Request{Action: "run", Name: "existing", Node: "r1", Checks: []Check{{Type: "tcpdump"}}}
	fake.probe = CommandResult{ExitCode: 124, Stdout: strings.Repeat("\x00", 4000)}
	response, err := m.Handle(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	checks := response.([]CheckResult)
	if len(checks) != 1 || !checks[0].TimedOut || !checks[0].Truncated || checks[0].ExitCode != 124 {
		t.Fatalf("missing probe status %+v", response)
	}
	if _, err := EncodeResponse(checks); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"host", "container:foreign"} {
		fake.nodes[0].HostConfig.NetworkMode = mode
		if _, err := m.Handle(context.Background(), req); err == nil {
			t.Fatal("host/shared namespace allowed")
		}
	}
}

func TestLockCancellationAndStagingRecovery(t *testing.T) {
	m, _ := testManager(t)
	unlock, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Handle(ctx, Request{Action: "inspect"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock ignored cancellation: %v", err)
	}
	unlock()
	stage := filepath.Join(m.dir, "pending-interrupted")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Handle(context.Background(), Request{Action: "inspect"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging remains")
	}
}

func TestConcurrentDeploymentRespectsHostLimit(t *testing.T) {
	m, fake := testManager(t)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology})
			results <- err
		}()
	}
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || fake.deploys != 1 {
		t.Fatalf("concurrent deployments bypassed limit: %v %v deploys=%d", first, second, fake.deploys)
	}
}

func TestDamagedRegistryRefusesDeletion(t *testing.T) {
	m, fake := testManager(t)
	if _, err := m.Handle(context.Background(), Request{Action: "deploy", Topology: simpleTopology}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "msre-lab-demo", "record.json"), []byte(`{"name":"../escape"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GC(context.Background()); err == nil || fake.destroys != 0 {
		t.Fatal("damaged record reached deletion")
	}
}
