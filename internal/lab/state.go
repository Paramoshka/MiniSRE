package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// CheckTrustedPath refuses symlinks and writable/non-root-owned parents before sudo writes.
func CheckTrustedPath(path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("lab: trusted path must be absolute and clean")
	}
	current := path
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("lab: %s must be root-owned, non-symlink and not writable by group/others", current)
		}
		if current != path || directory {
			if !info.IsDir() {
				return errors.New("lab: expected trusted directory")
			}
		} else if !info.Mode().IsRegular() {
			return errors.New("lab: expected trusted regular file")
		}
		parent := filepath.Dir(current)
		if current == parent {
			break
		}
		current = parent
	}
	return nil
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	file, err := os.OpenFile(filepath.Join(m.dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { file.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK {
			file.Close()
			return nil, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) records() ([]Record, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	records := []Record{}
	for _, entry := range entries {
		if entry.Name() == "lock" {
			continue
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "pending-") {
			continue
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "msre-lab-") || !validName(entry.Name()) {
			return nil, errors.New("lab: unexpected entry in state directory")
		}
		data, err := os.ReadFile(filepath.Join(m.dir, entry.Name(), "record.json"))
		if err != nil {
			return nil, err
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil || record.Name != entry.Name() || record.Topology.Name != record.Name || record.ExpiresAt.IsZero() {
			return nil, errors.New("lab: invalid registry record; refusing cleanup")
		}
		topologyYAML, err := yaml.Marshal(record.Topology)
		if err != nil {
			return nil, err
		}
		if _, err := ParseTopology(string(topologyYAML)); err != nil {
			return nil, err
		}
		configYAML, err := yaml.Marshal(record.Config)
		if err != nil {
			return nil, err
		}
		if _, err := ParseConfig(string(configYAML)); err != nil || record.Config.OwnerID != m.cfg.OwnerID {
			return nil, errors.New("lab: invalid registry config or owner mismatch")
		}
		records = append(records, record)
	}
	return records, nil
}

func (m *Manager) cleanupStaging() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "pending-") {
			if err := os.RemoveAll(filepath.Join(m.dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return syncDir(m.dir)
}

func saveRecord(dir string, record Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "record.tmp"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(filepath.Join(dir, "record.tmp"), filepath.Join(dir, "record.json")); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
