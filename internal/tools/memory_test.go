package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

const meminfoFixture = `MemTotal:       16384000 kB
MemFree:         3600000 kB
MemAvailable:   10485760 kB
Buffers:          100000 kB
Cached:          5000000 kB
SwapCached:            0 kB
AnonPages:       2000000 kB
Mapped:           300000 kB
Shmem:            150000 kB
SReclaimable:     200000 kB
Slab:             400000 kB
PageTables:        50000 kB
CommitLimit:     8388608 kB
Committed_AS:    4194304 kB
Dirty:              2048 kB
Writeback:             0 kB
Unevictable:        1000 kB
SwapTotal:        524288 kB
SwapFree:              0 kB
`

const psifixture = `/proc/pressure/cpu:some avg10=0.00 avg60=0.10 avg300=0.20 total=1234567
/proc/pressure/memory:some avg10=3.25 avg60=1.00 avg300=0.50 total=5000000
/proc/pressure/memory:full avg10=0.50 avg60=0.20 avg300=0.10 total=1000000
/proc/pressure/io:some avg10=1.00 avg60=0.50 avg300=0.25 total=2500000
`

const vmstatFixture = `nr_free_pages 12345
oom_kill 2
pswpout 100
pgmajfault 200
pgscan_direct 300
pgsteal_direct 400
`

func TestFormatMemoryReport(t *testing.T) {
	values, err := parseMeminfo(meminfoFixture)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	sample := memorySample{
		host:   "web",
		values: values,
		psi:    parsePSI(psifixture),
		vm:     parseVMStat(vmstatFixture),
	}
	out := formatMemoryReport(sample)
	for _, want := range []string{
		"Memory on web:",
		"MemTotal: 15.6 GiB",
		"MemFree: 3.4 GiB",
		"MemAvailable: 10.0 GiB (64.0% of MemTotal)",
		"Committed_AS: 4.0 GiB (50.0% of CommitLimit 8.0 GiB)",
		"Swap: total=512.0 MiB used=512.0 MiB free=0 B (100.0% used)",
		"memory full: avg10=0.50% avg60=0.20% avg300=0.10% total=1.0s",
		"oom_kill=2",
		"Notes: MemAvailable is an estimate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("memory report missing %q:\n%s", want, out)
		}
	}
}

func TestFormatMemoryFallback(t *testing.T) {
	values, err := parseMeminfo(strings.Replace(meminfoFixture, "MemAvailable:   10485760 kB\n", "", 1))
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	out := formatMemoryReport(memorySample{host: "local", values: values})
	if !strings.Contains(out, "MemAvailable:") || !strings.Contains(out, "fallback estimate: MemFree+Buffers+Cached") {
		t.Fatalf("fallback estimate missing: %q", out)
	}
	if strings.Contains(out, "PSI stall time") || !strings.Contains(out, "PSI: unavailable") {
		t.Fatalf("unexpected PSI report: %q", out)
	}
}

func TestMemoryUnavailableSources(t *testing.T) {
	values, err := parseMeminfo(meminfoFixture)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	out := formatMemoryReport(memorySample{
		host:   "web",
		values: values,
		psiErr: "/proc/pressure/io: No such file or directory",
		vmErr:  "no relevant counters found",
	})
	if !strings.Contains(out, "PSI: unavailable: /proc/pressure/io") ||
		!strings.Contains(out, "OOM and reclaim counters: unavailable: no relevant counters found") {
		t.Fatalf("unavailable sources not reported: %q", out)
	}
}

func TestParseMemoryRejectsInvalidData(t *testing.T) {
	for _, raw := range []string{"", "MemFree: 100 kB\n", "MemTotal: 0 kB\n"} {
		if _, err := parseMeminfo(raw); err == nil {
			t.Errorf("parseMeminfo(%q) = nil error, want error", raw)
		}
	}
}

func TestParsePSI(t *testing.T) {
	psi := parsePSI(psifixture)
	memory := psi["memory"]
	if memory["some"].avg10 != 3.25 || memory["some"].total != 5000000 {
		t.Fatalf("memory some = %+v", memory["some"])
	}
	if memory["full"].avg300 != 0.10 || memory["full"].total != 1000000 {
		t.Fatalf("memory full = %+v", memory["full"])
	}
	if _, ok := psi["cpu"]["full"]; ok {
		t.Fatal("cpu full should be absent")
	}
	if len(psi["bogus"]) != 0 {
		t.Fatal("unknown pressure resource accepted")
	}
}

func TestParseVMStat(t *testing.T) {
	vm := parseVMStat(vmstatFixture)
	if vm["oom_kill"] != 2 || vm["pgsteal_direct"] != 400 {
		t.Fatalf("vmstat counters = %+v", vm)
	}
	if _, ok := vm["nr_free_pages"]; ok {
		t.Fatal("unrelated vmstat counter returned")
	}
}

func TestMemoryLocal(t *testing.T) {
	if _, err := os.Stat(meminfoPath); err != nil {
		t.Skipf("%s unavailable: %v", meminfoPath, err)
	}
	out, err := (&Tools{Runner: &remote.Runner{}}).Memory(context.Background(), "local")
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if !strings.Contains(out, "MemTotal:") || !strings.Contains(out, "MemAvailable:") {
		t.Fatalf("unexpected memory report: %q", out)
	}
}
