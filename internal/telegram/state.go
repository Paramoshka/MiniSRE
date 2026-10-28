package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

type stateError struct {
	err error
}

func (e *stateError) Error() string { return "telegram: state: " + e.err.Error() }
func (e *stateError) Unwrap() error { return e.err }

type savedState struct {
	Version       int                       `json:"version"`
	Offset        int64                     `json:"offset"`
	OffsetSavedAt time.Time                 `json:"offset_saved_at"`
	Chats         map[int64][]agent.Message `json:"chats"`
}

func DefaultStatePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("telegram: find state directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "mini-sre", "telegram.json"), nil
}

// UseState restores context and verifies that durable checkpoints can be written.
// Call it once before Run, with a state file dedicated to this bot.
func (b *Bot) UseState(path string) error {
	if path == "" {
		return &stateError{errors.New("file path is required")}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return &stateError{fmt.Errorf("resolve file: %w", err)}
	}
	var state savedState
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return &stateError{fmt.Errorf("read file: %w", err)}
	}
	if err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return &stateError{errors.New("invalid JSON")}
		}
		if state.Version != 1 || state.Chats == nil || state.Offset < 0 ||
			(state.Offset > 0 && state.OffsetSavedAt.IsZero()) {
			return &stateError{errors.New("invalid or unsupported file format")}
		}
	} else {
		state = savedState{Version: 1, Chats: make(map[int64][]agent.Message)}
	}
	sessions := make(map[int64]*session.Session, len(state.Chats))
	for chatID, history := range state.Chats {
		if chatID <= 0 {
			return &stateError{errors.New("invalid private chat ID")}
		}
		s := b.newSession()
		if err := s.Restore(history); err != nil {
			return &stateError{fmt.Errorf("invalid history for chat %d: %w", chatID, err)}
		}
		sessions[chatID] = s
	}
	if time.Since(state.OffsetSavedAt) >= 24*time.Hour {
		state.Offset = 0
		state.OffsetSavedAt = time.Time{}
	}
	b.sessions = sessions
	b.statePath = path
	if err := b.saveState(state.Offset, state.OffsetSavedAt); err != nil {
		return err
	}
	b.offset, b.offsetSavedAt = state.Offset, state.OffsetSavedAt
	return nil
}

func (b *Bot) saveState(offset int64, savedAt time.Time) error {
	if b.statePath == "" {
		return nil
	}
	state := savedState{Version: 1, Offset: offset, OffsetSavedAt: savedAt, Chats: make(map[int64][]agent.Message)}
	for id, s := range b.sessions {
		if len(s.History) > 1 {
			state.Chats[id] = s.History[1:]
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return &stateError{errors.New("cannot encode history")}
	}
	if err := writeState(b.statePath, data); err != nil {
		return &stateError{err}
	}
	return nil
}

// clearHistory backs up the durable state before clearing just one chat. Copying
// via writeState, rather than renaming the active file away, leaves a valid
// checkpoint in place even if the process stops between the two writes.
func (b *Bot) clearHistory(chatID, offset int64, savedAt time.Time) error {
	if b.statePath != "" {
		data, err := os.ReadFile(b.statePath)
		if err != nil {
			return &stateError{fmt.Errorf("read state for backup: %w", err)}
		}
		if err := writeState(b.statePath+".bak", data); err != nil {
			return &stateError{fmt.Errorf("back up state: %w", err)}
		}
	}
	previous := b.sessions[chatID]
	delete(b.sessions, chatID)
	if err := b.saveState(offset, savedAt); err != nil {
		if previous != nil {
			b.sessions[chatID] = previous
		}
		return err
	}
	return nil
}

func writeState(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".telegram-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		// Cleanup failures also matter: temporary files contain conversation data.
		if removeErr := os.Remove(f.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary file: %w", removeErr))
		}
	}()
	writeErr := f.Chmod(0600)
	if writeErr == nil {
		_, writeErr = f.Write(data)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	syncErr := d.Sync()
	if err := errors.Join(syncErr, d.Close()); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
