package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"mini-sre/internal/agent"
)

func TestTelegramClearCheckpointFailureAfterBackupKeepsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telegram.json")
	b := stateBot(t, "prompt", func(_ context.Context, req agent.Request) (*agent.Response, error) {
		if req.Messages[len(req.Messages)-1].Content == "continue" && (len(req.Messages) != 4 || req.Messages[1].Content != "keep this") {
			t.Error("checkpoint failure discarded the in-memory history")
		}
		return &agent.Response{Content: "answer"}, nil
	})
	if err := b.UseState(path); err != nil {
		t.Fatal(err)
	}
	_, err := pollHistoryUpdates(t, b, []update{{ID: 0, Message: privateMessage(t, 1, "keep this")}}, nil)
	requireHistoryPollStopped(t, err)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}

	// The writer opens only once the bot opens the FIFO to read its old state.
	// Replace the path with a directory before closing the writer: ReadFile sees
	// the complete old JSON and EOF, so the backup succeeds, but replacing the
	// active checkpoint must fail. This avoids timing races or private test hooks.
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- func() (err error) {
			f, err := os.OpenFile(path, os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, f.Close()) }()
			if _, err := f.Write(before); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0700)
		}()
	}()
	replies, err := pollHistoryUpdates(t, b, []update{{ID: 1, Message: privateMessage(t, 1, "/clear")}}, nil)
	var stateErr *stateError
	if !errors.As(err, &stateErr) || len(replies) != 0 {
		t.Fatalf("checkpoint failure was ignored or confirmed: replies=%v err=%v", replies, err)
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("state reader did not consume the fault-injection FIFO")
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != string(before) {
		t.Fatal("checkpoint failure did not leave a complete pre-clear backup")
	}
	leaks, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".telegram-*.tmp"))
	if err != nil || len(leaks) != 0 {
		t.Fatal("checkpoint failure leaked temporary conversation data")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".previous", path); err != nil {
		t.Fatal(err)
	}
	_, err = pollHistoryUpdates(t, b, []update{{ID: 1, Message: privateMessage(t, 1, "continue")}}, nil)
	requireHistoryPollStopped(t, err)
	state := readState(t, path)
	if state.Offset != 2 || len(state.Chats[1]) != 4 || state.Chats[1][0].Content != "keep this" {
		t.Fatal("failed clear changed the polling offset or lost the previous conversation")
	}
}
