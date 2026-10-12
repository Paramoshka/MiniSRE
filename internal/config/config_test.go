package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yaml")
	content := "known_hosts: known_hosts\nhosts:\n  web:\n    address: localhost\n    user: sre\n    key_file: id_ed25519\n  db:\n    address: 127.0.0.1\n    port: 2222\n    user: sre\n    password: secret\ntelegram:\n  allowed_user_ids: [123]\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hosts["web"].Port != 22 || cfg.Hosts["db"].Port != 2222 || cfg.Hosts["db"].Password != "secret" {
		t.Fatal("host defaults or credentials were not loaded")
	}
	if cfg.KnownHosts != filepath.Join(dir, "known_hosts") || cfg.Hosts["web"].KeyFile != filepath.Join(dir, "id_ed25519") {
		t.Fatal("relative paths were not resolved against config directory")
	}
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestLoadRejectsInvalidConfigWithoutSecrets(t *testing.T) {
	valid := "hosts:\n  web:\n    address: localhost\n    user: sre\n    password: secret\n"
	tests := []string{
		"password: secret", "hosts: secret", "hosts: [secret", "---\n{}\n---\n{}",
		strings.Replace(valid, "web:", "local:", 1),
		strings.Replace(valid, "password: secret", "key_file: key\n    password: secret", 1),
		strings.Replace(valid, "password: secret", "port: 65536", 1),
		valid + "telegram:\n  allowed_user_ids: [-1]\n",
		valid + "lab_hosts: [missing]\n",
		valid + "lab_hosts: [web, web]\n",
	}
	for _, content := range tests {
		path := filepath.Join(t.TempDir(), "hosts.yaml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatalf("invalid config accepted: %q", content)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("config error leaked secret: %v", err)
		}
	}
}
