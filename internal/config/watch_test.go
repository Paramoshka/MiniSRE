package config

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

type watchLog struct {
	sync.Mutex
	bytes.Buffer
}

func (l *watchLog) Write(p []byte) (int, error) {
	l.Lock()
	defer l.Unlock()
	return l.Buffer.Write(p)
}

func (l *watchLog) text() string {
	l.Lock()
	defer l.Unlock()
	return l.Buffer.String()
}

func await(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for config watcher")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatchReloadsWholeConfigAndKeepsSnapshots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yaml")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	initial := "language: ru\nknown_hosts: old_keys\nhosts:\n  old:\n    address: localhost\n    user: sre\n    password: old-secret\ntelegram:\n  allowed_user_ids: [1]\n"
	updated := "language: en\nknown_hosts: new_keys\nhosts:\n  new:\n    address: localhost\n    user: sre\n    password: new-secret\ntelegram:\n  allowed_user_ids: [2]\n"
	write(path, initial)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var logs watchLog
	live, err := Watch(ctx, path, true, &logs, cancel)
	if err != nil {
		t.Fatal(err)
	}
	old := live.Current()

	// Read snapshots concurrently with reloads; the race detector checks publication.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			cfg := live.Current()
			for _, host := range cfg.Hosts {
				_ = host.Password
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() { cancel(nil); <-done }()

	write(path, updated)
	await(t, func() bool { return live.Current().Telegram.AllowedUserIDs[0] == 2 })
	cfg := live.Current()
	if cfg.Language != "en" || cfg.KnownHosts != filepath.Join(dir, "new_keys") || cfg.Hosts["new"].Password != "new-secret" || len(cfg.Hosts) != 1 {
		t.Fatalf("partial reload: %+v", cfg)
	}
	if old.Language != "ru" || old.Hosts["old"].Password != "old-secret" || old.Telegram.AllowedUserIDs[0] != 1 {
		t.Fatal("old snapshot was mutated")
	}

	// Editors often replace the file rather than writing its existing inode.
	tmp := filepath.Join(dir, "replacement")
	write(tmp, initial)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return live.Current().Telegram.AllowedUserIDs[0] == 1 })

	write(path, "hosts: [private-password")
	await(t, func() bool { return strings.Count(logs.text(), "reload rejected") == 1 })
	if live.Current().Hosts["old"].Password != "old-secret" || strings.Contains(logs.text(), "private-password") {
		t.Fatal("bad YAML replaced config or leaked its value")
	}
	write(path, "hosts: {}\ntelegram:\n  allowed_user_ids: []\n")
	await(t, func() bool { return strings.Count(logs.text(), "reload rejected") == 2 })
	if live.Current().Telegram.AllowedUserIDs[0] != 1 {
		t.Fatal("empty allowlist replaced the active Telegram config")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return strings.Count(logs.text(), "reload rejected") == 3 })
	write(path, updated)
	await(t, func() bool { return live.Current().Telegram.AllowedUserIDs[0] == 2 })
	write(path, strings.Replace(updated, "language: en", "language: private-invalid-language", 1))
	await(t, func() bool { return strings.Count(logs.text(), "reload rejected") == 4 })
	if live.Current().Language != "en" || strings.Contains(logs.text(), "private-invalid-language") {
		t.Fatal("invalid language replaced the active config or leaked its value")
	}
	if ctx.Err() != nil {
		t.Fatalf("recoverable config error stopped watcher: %v", context.Cause(ctx))
	}
}

func TestWatchStartupAndFatalClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, err := Watch(ctx, "", false, io.Discard, func(error) { t.Error("unexpected fatal error") })
	if err != nil || len(live.Current().Hosts) != 0 {
		t.Fatalf("local-only config: %v", err)
	}
	if _, err := Watch(ctx, "", true, io.Discard, func(error) {}); err == nil {
		t.Fatal("empty Telegram config accepted")
	}
	if _, err := Watch(ctx, filepath.Join(t.TempDir(), "missing.yaml"), false, io.Discard, func(error) {}); err == nil {
		t.Fatal("missing startup config accepted")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := live.watch(ctx, watcher, "/unused/hosts.yaml", false, io.Discard); err == nil {
		t.Fatal("unexpected watcher closure was not fatal")
	}
	cancel()
	if err := live.watch(ctx, watcher, "/unused/hosts.yaml", false, io.Discard); err != nil {
		t.Fatalf("normal shutdown was fatal: %v", err)
	}
}
