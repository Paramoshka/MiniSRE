package remote

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalAndSSHInput(t *testing.T) {
	for _, host := range []string{"local", "web"} {
		t.Run(host, func(t *testing.T) {
			r := &Runner{}
			if host == "web" {
				r = testSSH(t, false)
			}
			payload := "topology: '$HOME `id` $(id)'\nsecond line\n"
			result, err := r.runInput(context.Background(), host, false, time.Second, strings.NewReader(payload), "cat")
			if err != nil || result.Stdout != payload {
				t.Fatalf("stdin corrupted: %+v %v", result, err)
			}
		})
	}
}
