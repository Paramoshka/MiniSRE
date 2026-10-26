package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mini-sre/internal/remote"
)

func TestDiskInodes(t *testing.T) {
	for _, tc := range []struct {
		name, row, want string
		wantErr         bool
	}{
		{"exhausted with free bytes", "1024 512 512 100 100 0", "inodes: total=100 used=100 available=0 use=100.0%", false},
		{"unsupported", "1024 512 512 - - -", "inodes: unavailable", false},
		{"zero total", "1024 512 512 0 0 0", "inodes: unavailable", false},
		{"partially unavailable", "1024 512 512 100 - 50", "inodes: unavailable", false},
		{"bad inode", "1024 512 512 100 broken 50", "", true},
		{"bad bytes", "1024 broken 512 100 50 50", "", true},
		{"missing columns", "1024 512 512", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCommand(t, "df", "Size Used Avail Inodes IUsed IFree\n"+tc.row+"\n", "", "0")
			out, err := Registry(&remote.Runner{})["get_disk_usage"](context.Background(), nil)
			if (err != nil) != tc.wantErr || (!tc.wantErr && (!strings.Contains(out, tc.want) || !strings.Contains(out, "available=512 B"))) {
				t.Fatalf("disk = %q, %v", out, err)
			}
		})
	}
}

func TestDiskInodesLocalAndSSH(t *testing.T) {
	runner := fileSSHRunner(t)
	fakeCommand(t, "df", "Size Used Avail Inodes IUsed IFree\n1024 512 512 100 100 0\n", "", "0")
	for _, host := range []string{"local", "web"} {
		out, err := Registry(runner)["get_disk_usage"](context.Background(), json.RawMessage(`{"host_id":"`+host+`"}`))
		if err != nil || !strings.Contains(out, "inodes: total=100") {
			t.Fatalf("%s: %q, %v", host, out, err)
		}
		out, err = Registry(runner)["run_pipeline"](context.Background(), json.RawMessage(`{"host_id":"`+host+`","source":{"tool":"get_disk_usage"}}`))
		if err != nil || !strings.Contains(out, "1024 512 512 100 100 0") {
			t.Fatalf("pipeline %s: %q, %v", host, out, err)
		}
	}
}
