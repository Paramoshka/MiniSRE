package telegram

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

const taskHelp = "/task add <check interval> <report interval> <prompt> — create a task, for example:\n" +
	"/task add 5m 1h Check nginx on web-1; report a problem if the service is not active.\n" +
	"/task list, /task show <id>, /task pause <id>, /task resume <id>, /task delete <id>.\n" +
	"MVP limit: 5 tasks across the bot, including paused tasks.\n" +
	"Without explicit criteria, the model decides what counts as a problem."

type taskCommand struct {
	ownerID  int64
	chatID   int64
	updateID int64
	input    string
	reply    chan taskCommandReply
}

type taskCommandReply struct {
	text string
	err  error
}

func (s *taskScheduler) command(ctx context.Context, m *message, updateID int64, input string) (string, error) {
	cmd := taskCommand{
		ownerID: m.From.ID, chatID: m.Chat.ID, updateID: updateID,
		input: input, reply: make(chan taskCommandReply, 1),
	}
	select {
	case s.commands <- cmd:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case reply := <-cmd.reply:
		return reply.text, reply.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *taskScheduler) handleCommand(cmd taskCommand) taskCommandReply {
	if !slices.Contains(s.bot.users(), cmd.ownerID) {
		return taskCommandReply{}
	}
	if err := s.reload(); err != nil {
		s.bot.log(err)
		return taskCommandReply{text: s.bot.text("Файл задач недоступен или некорректен. Исправьте файл; он не был перезаписан.", "The tasks file is unavailable or invalid. Fix the file; it has not been overwritten.")}
	}
	last := s.file.LastCommand
	if cmd.updateID != 0 && last.UpdateID == cmd.updateID && last.OwnerID == cmd.ownerID &&
		last.Input == cmd.input && s.now().Sub(last.At) < 24*time.Hour {
		return taskCommandReply{text: last.Reply}
	}
	text, changed := s.editCommand(cmd)
	if !changed {
		return taskCommandReply{text: text}
	}
	s.file.LastCommand = taskCommandCheckpoint{
		UpdateID: cmd.updateID, OwnerID: cmd.ownerID,
		Input: cmd.input, Reply: text, At: s.now().UTC(),
	}
	return taskCommandReply{text: text, err: s.save()}
}

func (s *taskScheduler) editCommand(cmd taskCommand) (string, bool) {
	parts := strings.Fields(cmd.input)
	if len(parts) < 2 {
		return taskHelp, false
	}
	switch parts[1] {
	case "add":
		return s.addTask(cmd, parts)
	case "list":
		if len(parts) != 2 {
			return taskHelp, false
		}
		return s.listTasks(cmd.ownerID), false
	case "show", "pause", "resume", "delete":
		if len(parts) != 3 {
			return taskHelp, false
		}
		task := s.find(parts[2])
		if task == nil || task.OwnerID != cmd.ownerID {
			return s.bot.text("Задача не найдена.", "Task not found."), false
		}
		if parts[1] == "show" {
			return fmt.Sprintf("Task %s · enabled: %t\n%s\nCheck: %s; report: %s\nNext check: %s\nLast result: %s\n%s", task.ID, task.Enabled, task.Prompt, task.Every, task.ReportEvery, task.State.NextRun.Format(time.RFC3339), task.State.LastResult.Status, task.State.LastResult.Summary), false
		}
		s.revision++
		task.revision = s.revision
		task.State.Running = false
		task.State.PendingNotice = ""
		if parts[1] == "delete" {
			s.file.Tasks = slices.DeleteFunc(s.file.Tasks, func(t *scheduledTask) bool { return t.ID == task.ID })
			return "Task deleted.", true
		}
		task.Enabled = parts[1] == "resume"
		if task.Enabled {
			check, report, _ := taskIntervals(task.Every, task.ReportEvery)
			task.State.NextRun = s.now().UTC().Add(check)
			task.State.NextReport = s.now().UTC().Add(report)
			return "Task resumed.", true
		}
		return "Task paused.", true
	default:
		return taskHelp, false
	}
}

func (s *taskScheduler) addTask(cmd taskCommand, parts []string) (string, bool) {
	if len(parts) < 5 {
		return taskHelp, false
	}
	checkInterval, reportInterval, err := taskIntervals(parts[2], parts[3])
	if err != nil {
		if errors.Is(err, errCheckInterval) {
			return s.bot.text("Интервал проверки должен быть не меньше минуты, например 5m или 1h.", err.Error()), false
		}
		if errors.Is(err, errReportInterval) {
			return s.bot.text("Интервал отчёта должен быть не меньше интервала проверки.", err.Error()), false
		}
		return err.Error(), false
	}
	if len(s.file.Tasks) >= maxScheduledTasks {
		return fmt.Sprintf(s.bot.text("Достигнут лимит MVP: %d задач на бота, включая приостановленные. /task list — список задач; /task delete <id> — удалить задачу и освободить место.", "MVP limit reached: %d tasks across the bot, including paused tasks. /task list — view your tasks; /task delete <id> — delete a task and free a slot."), maxScheduledTasks), false
	}
	// Cut the four command words without changing whitespace inside the prompt.
	prompt := cmd.input
	for range 4 {
		prompt = strings.TrimSpace(prompt)
		index := strings.IndexFunc(prompt, unicode.IsSpace)
		if index < 0 {
			return taskHelp, false
		}
		prompt = prompt[index:]
	}
	prompt = strings.TrimSpace(prompt)
	now := s.now().UTC()
	s.revision++
	task := &scheduledTask{
		ID: newTaskID(), OwnerID: cmd.ownerID, ChatID: cmd.chatID, Prompt: prompt,
		Every: parts[2], ReportEvery: parts[3], Enabled: true, revision: s.revision,
		State: taskState{NextRun: now.Add(checkInterval), NextReport: now.Add(reportInterval), PeriodStart: now},
	}
	s.file.Tasks = append(s.file.Tasks, task)
	return fmt.Sprintf("Task %s created. Check every %s, report every %s.\n%s\nWithout explicit criteria, the model decides what counts as a problem.", task.ID, task.Every, task.ReportEvery, task.Prompt), true
}

func (s *taskScheduler) listTasks(ownerID int64) string {
	var lines []string
	for _, task := range s.file.Tasks {
		if task.OwnerID != ownerID {
			continue
		}
		status := "paused"
		if task.Enabled {
			status = "enabled"
		}
		lines = append(lines, fmt.Sprintf("%s · %s · %s / %s\n%s", task.ID, status, task.Every, task.ReportEvery, task.Prompt))
	}
	if len(lines) == 0 {
		return "No tasks yet.\n" + taskHelp
	}
	return strings.Join(lines, "\n\n")
}
