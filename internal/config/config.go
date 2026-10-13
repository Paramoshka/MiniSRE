package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Host struct {
	Address  string `yaml:"address"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	KeyFile  string `yaml:"key_file"`
	Password string `yaml:"password"`
}

type Config struct {
	Language   string          `yaml:"language"`
	KnownHosts string          `yaml:"known_hosts"`
	Hosts      map[string]Host `yaml:"hosts"`
	LabHosts   []string        `yaml:"lab_hosts"`
	Telegram   struct {
		AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
	} `yaml:"telegram"`
}

func Load(path string) (Config, error) {
	cfg := Config{Language: "ru"}
	if path == "" {
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("config: open file: %w", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		// YAML errors can include the value of a password, so do not echo them.
		return Config{}, errors.New("config: invalid YAML; check syntax, field names and value types")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("config: expected one YAML document")
	}
	if cfg.Language != "ru" && cfg.Language != "en" {
		return Config{}, errors.New("config: language must be ru or en")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("config: resolve directory: %w", err)
	}
	if cfg.KnownHosts == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("config: find home directory: %w", err)
		}
		cfg.KnownHosts = filepath.Join(home, ".ssh", "known_hosts")
	} else {
		cfg.KnownHosts = resolvePath(base, cfg.KnownHosts)
	}
	for id, host := range cfg.Hosts {
		if id == "" || id == "local" || strings.TrimSpace(id) != id {
			return Config{}, errors.New("config: host IDs must be nonempty, trimmed and different from local")
		}
		if strings.TrimSpace(host.Address) == "" || strings.TrimSpace(host.User) == "" {
			return Config{}, fmt.Errorf("config: host %q requires address and user", id)
		}
		if (host.KeyFile == "") == (host.Password == "") {
			return Config{}, fmt.Errorf("config: host %q requires exactly one of key_file or password", id)
		}
		if host.Port == 0 {
			host.Port = 22
		}
		if host.Port < 1 || host.Port > 65535 {
			return Config{}, fmt.Errorf("config: host %q has invalid port", id)
		}
		if host.KeyFile != "" {
			host.KeyFile = resolvePath(base, host.KeyFile)
		}
		cfg.Hosts[id] = host
	}
	for _, id := range cfg.Telegram.AllowedUserIDs {
		if id <= 0 {
			return Config{}, errors.New("config: Telegram user IDs must be positive")
		}
	}
	seenLabHosts := make(map[string]bool)
	for _, id := range cfg.LabHosts {
		if _, exists := cfg.Hosts[id]; (id != "local" && !exists) || seenLabHosts[id] {
			return Config{}, errors.New("config: lab_hosts must contain distinct configured host IDs or local")
		}
		seenLabHosts[id] = true
	}
	return cfg, nil
}

func resolvePath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}
