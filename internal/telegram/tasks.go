package telegram

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxScheduledTasks = 5

var (
	errCheckInterval  = errors.New("The check interval must be at least one minute, for example 5m or 1h.")
	errReportInterval = errors.New("The report interval must be at least the check interval.")
)

type scheduledTask struct {
	ID          string    `json:"id"`
	OwnerID     int64     `json:"owner_id"`
	ChatID      int64     `json:"chat_id"`
	Prompt      string    `json:"prompt"`
	Every       string    `json:"every"`
	ReportEvery string    `json:"report_every"`
	Enabled     bool      `json:"enabled"`
	State       taskState `json:"state"`
	revision    uint64
}

type taskState struct {
	Running       bool        `json:"running"`
	LastStarted   time.Time   `json:"last_started,omitempty"`
	LastFinished  time.Time   `json:"last_finished,omitempty"`
	NextRun       time.Time   `json:"next_run"`
	NextReport    time.Time   `json:"next_report"`
	PeriodStart   time.Time   `json:"period_start"`
	LastResult    taskOutcome `json:"last_result"`
	Confirmed     string      `json:"confirmed_status,omitempty"`
	Counts        taskCounts  `json:"counts"`
	PendingNotice string      `json:"pending_notice,omitempty"`
}

type taskOutcome struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

type taskCounts struct {
	OK          int `json:"ok"`
	Problem     int `json:"problem"`
	Unknown     int `json:"unknown"`
	Interrupted int `json:"interrupted"`
	Skipped     int `json:"skipped"`
}

type taskCommandCheckpoint struct {
	UpdateID int64     `json:"update_id"`
	OwnerID  int64     `json:"owner_id"`
	Input    string    `json:"input"`
	Reply    string    `json:"reply"`
	At       time.Time `json:"at"`
}

type taskFile struct {
	Version     int                   `json:"version"`
	Tasks       []*scheduledTask      `json:"tasks"`
	LastCommand taskCommandCheckpoint `json:"last_command"`
}

type taskScheduler struct {
	bot      *Bot
	path     string
	file     taskFile
	lastData []byte
	revision uint64
	commands chan taskCommand
	now      func() time.Time
	blocked  bool
}

// UseTasks restores schedules before Run. One process must own each tasks file.
func (b *Bot) UseTasks(path string) error {
	if path == "" {
		return &stateError{errors.New("tasks file path is required")}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return &stateError{fmt.Errorf("resolve tasks file: %w", err)}
	}
	if abs == b.statePath {
		return &stateError{errors.New("tasks and chat state must use different files")}
	}
	s := &taskScheduler{bot: b, path: abs, commands: make(chan taskCommand), now: time.Now}
	data, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		s.file = taskFile{Version: 1, Tasks: []*scheduledTask{}}
	} else if err != nil {
		return &stateError{fmt.Errorf("read tasks: %w", err)}
	} else {
		s.file, err = decodeTaskFile(data)
		if err != nil {
			return &stateError{err}
		}
		s.lastData = data
	}
	now := s.now().UTC()
	for _, task := range s.file.Tasks {
		s.revision++
		task.revision = s.revision
		if task.State.Running {
			task.State.Running = false
			task.State.LastResult = taskOutcome{"unknown", b.text("Предыдущая проверка была прервана.", "The previous check was interrupted.")}
			task.State.Counts.Interrupted++
			if task.State.NextRun.After(now) {
				task.State.NextRun = now
			}
		}
	}
	if err := s.save(); err != nil {
		return err
	}
	b.tasks = s
	return nil
}

func decodeTaskFile(data []byte) (taskFile, error) {
	var file taskFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return file, errors.New("tasks: invalid JSON or unknown fields")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || file.Version != 1 || file.Tasks == nil {
		return file, errors.New("tasks: invalid or unsupported file format")
	}
	if len(file.Tasks) > maxScheduledTasks {
		return file, fmt.Errorf("tasks: MVP supports at most %d tasks, including paused tasks", maxScheduledTasks)
	}
	ids := make(map[string]bool)
	for _, task := range file.Tasks {
		if task == nil || task.ID == "" || ids[task.ID] || task.OwnerID <= 0 ||
			task.ChatID != task.OwnerID || strings.TrimSpace(task.Prompt) == "" {
			return file, errors.New("tasks: invalid task ID, owner, private chat or prompt")
		}
		if _, _, err := taskIntervals(task.Every, task.ReportEvery); err != nil {
			return file, err
		}
		state := task.State
		if state.NextRun.IsZero() || state.NextReport.IsZero() || state.PeriodStart.IsZero() ||
			(state.Running && state.LastStarted.IsZero()) ||
			!validTaskStatus(state.LastResult.Status, true) ||
			(state.Confirmed != "" && state.Confirmed != "ok" && state.Confirmed != "problem") ||
			state.Counts.OK < 0 || state.Counts.Problem < 0 || state.Counts.Unknown < 0 ||
			state.Counts.Interrupted < 0 || state.Counts.Skipped < 0 {
			return file, errors.New("tasks: invalid task state")
		}
		ids[task.ID] = true
	}
	return file, nil
}

func taskIntervals(every, report string) (time.Duration, time.Duration, error) {
	check, err := time.ParseDuration(every)
	if err != nil || check < time.Minute {
		return 0, 0, errCheckInterval
	}
	reports, err := time.ParseDuration(report)
	if err != nil || reports < check {
		return 0, 0, errReportInterval
	}
	return check, reports, nil
}

func validTaskStatus(status string, empty bool) bool {
	return status == "ok" || status == "problem" || status == "unknown" || (empty && status == "")
}

func (s *taskScheduler) find(id string) *scheduledTask {
	for _, task := range s.file.Tasks {
		if task.ID == id {
			return task
		}
	}
	return nil
}

func (s *taskScheduler) save() error {
	data, err := json.MarshalIndent(s.file, "", "  ")
	if err != nil {
		return &stateError{fmt.Errorf("encode tasks: %w", err)}
	}
	data = append(data, '\n')
	if err := writeState(s.path, data); err != nil {
		return &stateError{fmt.Errorf("save tasks: %w", err)}
	}
	s.lastData = data
	return nil
}

// Runtime fields belong to the process. External edits change task definitions only.
func (s *taskScheduler) reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.blocked = true
		return fmt.Errorf("tasks: reload rejected: %w", err)
	}
	if bytes.Equal(data, s.lastData) {
		s.blocked = false
		return nil
	}
	file, err := decodeTaskFile(data)
	if err != nil {
		s.blocked = true
		return fmt.Errorf("tasks: reload rejected: %w", err)
	}
	now := s.now().UTC()
	for _, task := range file.Tasks {
		old := s.find(task.ID)
		if old != nil {
			if task.OwnerID != old.OwnerID || task.ChatID != old.ChatID {
				s.blocked = true
				return errors.New("tasks: reload rejected: existing task owner cannot be changed")
			}
			task.State = old.State
			task.revision = old.revision
			if task.Prompt != old.Prompt || task.Every != old.Every ||
				task.ReportEvery != old.ReportEvery || task.Enabled != old.Enabled {
				s.revision++
				task.revision = s.revision
				task.State.Running = false
				task.State.PendingNotice = ""
				check, report, _ := taskIntervals(task.Every, task.ReportEvery)
				task.State.NextRun = now.Add(check)
				task.State.NextReport = now.Add(report)
				if task.Prompt != old.Prompt {
					task.State.Confirmed = ""
				}
			}
		} else {
			s.revision++
			task.revision = s.revision
			check, report, _ := taskIntervals(task.Every, task.ReportEvery)
			task.State = taskState{NextRun: now.Add(check), NextReport: now.Add(report), PeriodStart: now}
		}
	}
	file.LastCommand = s.file.LastCommand
	s.file, s.lastData, s.blocked = file, data, false
	return nil
}

func newTaskID() string {
	return rand.Text()[:12]
}
