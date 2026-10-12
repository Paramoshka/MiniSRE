package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

func testTasks(t *testing.T) (*Bot, *taskScheduler) {
	t.Helper()
	b, err := New("token", func() []int64 { return []int64{1, 2} }, func() *session.Session {
		return &session.Session{}
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.UseTasks(filepath.Join(t.TempDir(), "tasks.json")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	b.tasks.now = func() time.Time { return now }
	return b, b.tasks
}

func addTestTask(t *testing.T, s *taskScheduler, every, report string) *scheduledTask {
	t.Helper()
	reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, updateID: 10, input: "/task add " + every + " " + report + " Check nginx\non web-1"})
	if reply.err != nil || len(s.file.Tasks) != 1 {
		t.Fatalf("add: %+v; tasks: %+v", reply, s.file.Tasks)
	}
	return s.file.Tasks[0]
}

func editTaskFile(t *testing.T, s *taskScheduler, edit func(*taskFile)) {
	t.Helper()
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := decodeTaskFile(data)
	if err != nil {
		t.Fatal(err)
	}
	edit(&file)
	data, err = json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	// Editors commonly replace a file rather than writing the original inode.
	if err := writeState(s.path, data); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCommandsOwnershipAndDuplicateUpdate(t *testing.T) {
	_, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	if task.Prompt != "Check nginx\non web-1" || task.State.NextRun.Sub(s.now()) != 5*time.Minute {
		t.Fatalf("wrong prompt or deadline: %+v", task)
	}
	first := s.file.LastCommand.Reply
	reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, updateID: 10, input: s.file.LastCommand.Input})
	if reply.text != first || len(s.file.Tasks) != 1 {
		t.Fatal("redelivered update created a second task")
	}
	for _, verb := range []string{"show", "pause", "resume", "delete"} {
		reply := s.handleCommand(taskCommand{ownerID: 2, chatID: 2, input: "/task " + verb + " " + task.ID})
		if reply.err != nil || reply.text != "Task not found." || !task.Enabled {
			t.Fatalf("other owner accessed task: %s %+v", verb, reply)
		}
	}
	for _, input := range []string{"/task add 30s 1h test", "/task add 1h 5m test", "/task add bad 1h test", "/task add 5m 1h", "/task unknown"} {
		reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: input})
		if reply.err != nil || len(s.file.Tasks) != 1 {
			t.Fatalf("invalid command mutated tasks: %s %+v", input, reply)
		}
	}
	for _, verb := range []string{"pause", "resume", "show", "list"} {
		input := "/task " + verb
		if verb != "list" {
			input += " " + task.ID
		}
		reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: input})
		if reply.err != nil || reply.text == "Task not found." {
			t.Fatalf("command: %s %+v", verb, reply)
		}
		if verb == "pause" && task.Enabled || verb == "resume" && !task.Enabled {
			t.Fatal("pause/resume did not change enabled")
		}
	}
	if reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: "/task delete " + task.ID}); reply.err != nil || len(s.file.Tasks) != 0 {
		t.Fatalf("delete: %+v", reply)
	}
}

func TestTaskIntervalsAndHourlySummary(t *testing.T) {
	_, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	now := s.now()
	s.now = func() time.Time { return now }
	jobs := make(chan scheduledTask, 1)
	outgoing := make(chan taskDelivery, 1)
	checking, sending := false, true
	if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil || len(jobs) != 0 {
		t.Fatal("task ran before its first interval")
	}
	for i := 1; i <= 12; i++ {
		now = now.Add(5 * time.Minute)
		if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil {
			t.Fatal(err)
		}
		job := <-jobs
		if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil || len(jobs) != 0 {
			t.Fatal("overlapping task started")
		}
		status := "ok"
		if i >= 4 && i <= 6 {
			status = "problem"
		}
		if err := s.finish(taskCompletion{job, taskOutcome{status, "checked"}}); err != nil {
			t.Fatal(err)
		}
		checking = false
	}
	if task.State.Counts.OK != 9 || task.State.Counts.Problem != 3 || task.State.Running {
		t.Fatalf("wrong hourly counts: %+v", task.State)
	}
	task.State.PendingNotice = ""
	sending = false
	if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil {
		t.Fatal(err)
	}
	report := <-outgoing
	if !report.report || !strings.Contains(report.text, "Completed: 12; healthy: 9; problems: 3") {
		t.Fatalf("wrong report: %+v", report)
	}
	report.err = errors.New("delivery failed")
	if err := s.delivered(report); err != nil || task.State.Counts.Problem != 3 {
		t.Fatal("delivery failure lost statistics")
	}
	report.err = nil
	// A check completed while the snapshot was being sent.
	task.State.Counts.OK++
	if err := s.delivered(report); err != nil || task.State.Counts.OK != 1 || task.State.Counts.Problem != 0 {
		t.Fatalf("delivery lost newer result: %+v", task.State.Counts)
	}
}

func TestTaskIncidentsUnknownAndRecovery(t *testing.T) {
	_, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	for _, scenario := range []struct {
		status string
		notice string
	}{
		{"problem", "problem"}, {"problem", ""}, {"unknown", ""},
		{"ok", "recovery"}, {"ok", ""}, {"problem", "problem"},
	} {
		task.State.PendingNotice = ""
		if err := s.finish(taskCompletion{*task, taskOutcome{scenario.status, "test"}}); err != nil {
			t.Fatal(err)
		}
		if scenario.notice == "" && task.State.PendingNotice != "" ||
			scenario.notice != "" && !strings.Contains(task.State.PendingNotice, scenario.notice) {
			t.Fatalf("status %s: notice %q", scenario.status, task.State.PendingNotice)
		}
	}
}

func TestTaskReloadPreservesRuntimeAndRejectsStaleResults(t *testing.T) {
	_, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	task.State.Counts.Problem = 3
	task.State.Running = true
	task.State.LastStarted = s.now()
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	job := *task
	editTaskFile(t, s, func(file *taskFile) {
		file.Tasks[0].Prompt = "Check another service"
		file.Tasks[0].State.Counts.Problem = 999
	})
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	task = s.find(task.ID)
	if task.State.Counts.Problem != 3 || task.State.Running || task.revision == job.revision {
		t.Fatalf("external edit lost state: %+v", task)
	}
	if err := s.finish(taskCompletion{job, taskOutcome{"ok", "old result"}}); err != nil || task.State.Counts.OK != 0 {
		t.Fatal("stale completion was applied")
	}
	editTaskFile(t, s, func(file *taskFile) { file.Tasks[0].OwnerID = 2; file.Tasks[0].ChatID = 2 })
	if err := s.reload(); err == nil || !s.blocked || s.find(task.ID).OwnerID != 1 {
		t.Fatal("external edit reassigned ownership")
	}
	editTaskFile(t, s, func(file *taskFile) { file.Tasks = []*scheduledTask{} })
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(taskCompletion{job, taskOutcome{"ok", "deleted result"}}); err != nil || len(s.file.Tasks) != 0 {
		t.Fatal("deleted task was restored by completion")
	}
}

func TestTaskCorruptFileIsNotOverwritten(t *testing.T) {
	b, s := testTasks(t)
	addTestTask(t, s, "5m", "1h")
	corrupt := []byte(`{"version":1,"tasks":`)
	if err := os.WriteFile(s.path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err == nil || !s.blocked || len(s.file.Tasks) != 1 {
		t.Fatal("bad file replaced valid schedules")
	}
	reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: "/task delete " + s.file.Tasks[0].ID})
	if reply.err != nil || !strings.Contains(reply.text, "invalid") {
		t.Fatalf("corrupt file command: %+v", reply)
	}
	if err := b.UseTasks(s.path); err == nil {
		t.Fatal("corrupt startup accepted")
	}
	data, err := os.ReadFile(s.path)
	if err != nil || string(data) != string(corrupt) {
		t.Fatal("corrupt file overwritten")
	}
}

func TestTaskRestoreAndMissedIntervals(t *testing.T) {
	b, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	task.State.Running = true
	task.State.LastStarted = time.Now().UTC().Add(-time.Hour)
	task.State.NextRun = time.Now().UTC().Add(-55 * time.Minute)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if err := b.UseTasks(s.path); err != nil {
		t.Fatal(err)
	}
	s = b.tasks
	task = s.file.Tasks[0]
	if task.State.Running || task.State.Counts.Interrupted != 1 || task.State.LastResult.Status != "unknown" {
		t.Fatalf("interrupted check not restored: %+v", task.State)
	}
	jobs := make(chan scheduledTask, 1)
	outgoing := make(chan taskDelivery, 1)
	checking, sending := false, true
	if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil || len(jobs) != 1 || task.State.Counts.Skipped < 10 {
		t.Fatalf("missed intervals replayed or hidden: %+v", task.State)
	}
	if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil || len(jobs) != 1 {
		t.Fatal("more than one catch-up check scheduled")
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("tasks file is not private")
	}
}

func TestTaskModelVerdictRequiresDiagnostics(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		output string
		tool   bool
		err    bool
		want   string
	}{
		{"healthy", `{"status":"ok","summary":"healthy"}`, true, false, "ok"},
		{"unhealthy", `{"status":"problem","summary":"inactive"}`, true, false, "problem"},
		{"no tools", `{"status":"ok","summary":"healthy"}`, false, false, "unknown"},
		{"probe error", `{"status":"ok","summary":"healthy"}`, true, true, "unknown"},
		{"invalid JSON", "everything is fine", true, false, "unknown"},
		{"invalid status", `{"status":"failed","summary":"bad"}`, true, false, "unknown"},
		{"trailing data", `{"status":"ok","summary":"healthy"} {}`, true, false, "unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			b, _ := testTasks(t)
			b.newSession = func() *session.Session {
				calls := 0
				return &session.Session{
					Turn: func(_ context.Context, req agent.Request) (*agent.Response, error) {
						calls++
						if calls == 1 {
							if len(req.Messages) != 2 || !strings.Contains(req.Messages[0].Content, "scheduled diagnostic") {
								t.Fatal("scheduled session reused chat history or omitted instructions")
							}
							if scenario.tool {
								return &agent.Response{ToolCalls: []agent.ToolCall{{ID: "check", Name: "get_service_status"}}}, nil
							}
						}
						return &agent.Response{Content: scenario.output}, nil
					},
					RunTool: func(context.Context, agent.ToolCall) (string, error) {
						if scenario.err {
							return "", errors.New("SSH unavailable")
						}
						return "active", nil
					},
				}
			}
			for range 2 {
				result := b.checkTask(context.Background(), scheduledTask{OwnerID: 1, Prompt: "check"})
				if result.Status != scenario.want {
					t.Fatalf("result: %+v", result)
				}
			}
		})
	}
}

func TestTaskRevokedAccessDoesNotCallModel(t *testing.T) {
	b, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	b.users = func() []int64 { return []int64{2} }
	b.newSession = func() *session.Session { t.Fatal("revoked task reached model"); return nil }
	if result := b.checkTask(context.Background(), *task); result.Status != "unknown" {
		t.Fatal("revoked check accepted")
	}
	now := s.now().Add(time.Hour)
	s.now = func() time.Time { return now }
	jobs := make(chan scheduledTask, 1)
	outgoing := make(chan taskDelivery, 1)
	checking, sending := false, false
	if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil || len(jobs) != 0 || len(outgoing) != 0 {
		t.Fatal("revoked owner received scheduled work")
	}
}

func TestTaskCommandCheckpointSurvivesRestart(t *testing.T) {
	b, s := testTasks(t)
	task := addTestTask(t, s, "5m", "1h")
	checkpoint := s.file.LastCommand
	if err := b.UseTasks(s.path); err != nil {
		t.Fatal(err)
	}
	s = b.tasks
	reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, updateID: checkpoint.UpdateID, input: checkpoint.Input})
	if reply.err != nil || reply.text != checkpoint.Reply || len(s.file.Tasks) != 1 || s.file.Tasks[0].ID != task.ID {
		t.Fatalf("checkpoint was lost on restart: %+v", reply)
	}
}

func TestTaskOldestDeadlineRunsFirst(t *testing.T) {
	_, s := testTasks(t)
	first := addTestTask(t, s, "1m", "1h")
	reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: "/task add 1m 1h Check the second service"})
	if reply.err != nil || len(s.file.Tasks) != 2 {
		t.Fatalf("second task: %+v", reply)
	}
	second := s.file.Tasks[1]
	first.State.NextRun = s.now()
	second.State.NextRun = s.now().Add(-time.Minute)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	jobs := make(chan scheduledTask, 1)
	outgoing := make(chan taskDelivery, 1)
	checking, sending := false, false
	if err := s.dispatch(jobs, outgoing, &checking, &sending); err != nil {
		t.Fatal(err)
	}
	if job := <-jobs; job.ID != second.ID {
		t.Fatal("list order starved an older deadline")
	}
}

func TestTaskModelDeadlineIsUnknown(t *testing.T) {
	b, _ := testTasks(t)
	b.newSession = func() *session.Session {
		return &session.Session{Turn: func(context.Context, agent.Request) (*agent.Response, error) {
			return nil, context.DeadlineExceeded
		}}
	}
	result := b.checkTask(context.Background(), scheduledTask{OwnerID: 1, Prompt: "check"})
	if result.Status != "unknown" {
		t.Fatalf("timeout was classified as an observed incident: %+v", result)
	}
}

func TestTaskLimitIncludesPausedTasksAndAllOwners(t *testing.T) {
	_, s := testTasks(t)
	for i := range maxScheduledTasks {
		reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, updateID: int64(i + 1), input: "/task add 1m 1h Check nginx"})
		if reply.err != nil || !strings.Contains(reply.text, "created") {
			t.Fatalf("task below limit rejected: %+v", reply)
		}
	}
	checkpoint := s.file.LastCommand
	duplicate := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, updateID: checkpoint.UpdateID, input: checkpoint.Input})
	if duplicate.err != nil || duplicate.text != checkpoint.Reply || len(s.file.Tasks) != maxScheduledTasks {
		t.Fatalf("duplicate command was rejected at capacity: %+v", duplicate)
	}
	id := s.file.Tasks[0].ID
	reply := s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: "/task pause " + id})
	if reply.err != nil || s.file.Tasks[0].Enabled {
		t.Fatalf("pause failed: %+v", reply)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTaskFile(before); err != nil {
		t.Fatalf("file at the limit was rejected: %v", err)
	}
	for _, owner := range []int64{1, 2} {
		reply := s.handleCommand(taskCommand{ownerID: owner, chatID: owner, input: "/task add 1m 1h Another task"})
		if reply.err != nil || !strings.Contains(reply.text, "MVP limit reached: 5") ||
			!strings.Contains(reply.text, "/task delete <id>") || len(s.file.Tasks) != maxScheduledTasks {
			t.Fatalf("owner %d bypassed limit: %+v", owner, reply)
		}
	}
	after, err := os.ReadFile(s.path)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected additions changed persisted tasks")
	}
	reply = s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: "/task list"})
	if reply.err != nil || !strings.Contains(reply.text, id+" · paused") {
		t.Fatalf("list omitted the task ID or paused state: %+v", reply)
	}
	reply = s.handleCommand(taskCommand{ownerID: 1, chatID: 1, input: "/task delete " + id})
	if reply.err != nil || s.find(id) != nil {
		t.Fatalf("delete did not free a slot: %+v", reply)
	}
	reply = s.handleCommand(taskCommand{ownerID: 2, chatID: 2, input: "/task add 1m 1h New task"})
	if reply.err != nil || !strings.Contains(reply.text, "created") || len(s.file.Tasks) != maxScheduledTasks {
		t.Fatalf("freed slot was unavailable: %+v", reply)
	}
}

func TestTaskLimitRejectsOversizedFilesWithoutOverwriting(t *testing.T) {
	b, s := testTasks(t)
	task := addTestTask(t, s, "1m", "1h")
	editTaskFile(t, s, func(file *taskFile) {
		for range maxScheduledTasks {
			copy := *task
			copy.ID = newTaskID()
			file.Tasks = append(file.Tasks, &copy)
		}
	})
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err == nil || !strings.Contains(err.Error(), "at most 5 tasks") ||
		!s.blocked || len(s.file.Tasks) != 1 {
		t.Fatalf("oversized reload replaced valid tasks: %v", err)
	}
	if err := b.UseTasks(s.path); err == nil || !strings.Contains(err.Error(), "at most 5 tasks") {
		t.Fatalf("oversized startup accepted: %v", err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil || string(before) != string(after) {
		t.Fatal("oversized file was overwritten")
	}
	repaired := taskFile{Version: 1, Tasks: []*scheduledTask{task}}
	data, err := json.Marshal(repaired)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeState(s.path, data); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil || s.blocked {
		t.Fatalf("repaired task file stayed blocked: %v", err)
	}
}
