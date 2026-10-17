package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

const scheduledPrompt = " This is an independent scheduled diagnostic check, not a conversation. " +
	"Use diagnostic tools on every run. Apply the problem criteria in the task text. " +
	"If no criteria are supplied, use your judgement and explicitly mention that in summary. " +
	"If the tools cannot establish the requested facts, report unknown, never ok. " +
	"Return exactly one JSON object without Markdown: {\"status\":\"ok\"|\"problem\"|\"unknown\",\"summary\":\"brief explanation in the configured response language\"}. " +
	"Problem means an observed unhealthy condition, not an API error. Do not create schedules or execute arbitrary commands."

type taskCompletion struct {
	task   scheduledTask
	result taskOutcome
}

type taskDelivery struct {
	task   scheduledTask
	report bool
	text   string
	end    time.Time
	err    error
}

func (b *Bot) checkTask(ctx context.Context, task scheduledTask) taskOutcome {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	language := b.currentLanguage()
	if !slices.Contains(b.users(), task.OwnerID) {
		return taskOutcome{"unknown", localized(language, "Доступ владельца задачи отозван.", "Task owner access has been revoked.")}
	}
	s := b.newSession()
	s.SystemPrompt = session.DefaultSystemPrompt + scheduledPrompt
	s.Language = language
	mutating := make(map[string]bool)
	allowedTools := make([]agent.Tool, 0, len(s.Tools))
	for _, tool := range s.Tools {
		if tool.Mutating {
			mutating[tool.Name] = true
			continue
		}
		allowedTools = append(allowedTools, tool)
	}
	s.Tools = allowedTools
	run := s.RunTool
	probeSucceeded, probeFailed := false, false
	if run != nil {
		s.RunTool = func(ctx context.Context, call agent.ToolCall) (string, error) {
			if mutating[call.Name] {
				probeFailed = true
				return "", errors.New("tasks: mutating tools are forbidden in scheduled checks")
			}
			if !slices.Contains(b.users(), task.OwnerID) {
				probeFailed = true
				return "", errors.New("tasks: owner access revoked")
			}
			out, err := run(ctx, call)
			if call.Name != "list_hosts" {
				if err != nil {
					probeFailed = true
				} else {
					probeSucceeded = true
				}
			}
			return out, err
		}
	}
	response, err := s.Ask(ctx, task.Prompt)
	if err != nil {
		b.log(err)
		return taskOutcome{"unknown", localized(language, "Не удалось выполнить проверку. Подробности — в журнале агента.", "Could not run the check. See the agent log for details.")}
	}
	var result taskOutcome
	dec := json.NewDecoder(strings.NewReader(response.Content))
	dec.DisallowUnknownFields()
	var extra any
	if err := dec.Decode(&result); err != nil || dec.Decode(&extra) != io.EOF ||
		!validTaskStatus(result.Status, false) || strings.TrimSpace(result.Summary) == "" {
		return taskOutcome{"unknown", localized(language, "Модель вернула некорректный результат проверки.", "The model returned an invalid check result.")}
	}
	if !probeSucceeded || (probeFailed && result.Status == "ok") {
		return taskOutcome{"unknown", localized(language, "Инструменты не подтвердили результат проверки. ", "The tools did not confirm the check result. ") + result.Summary}
	}
	return result
}

func (s *taskScheduler) finish(completion taskCompletion) error {
	task := s.find(completion.task.ID)
	if task == nil || task.revision != completion.task.revision {
		return nil
	}
	now := s.now().UTC()
	task.State.Running = false
	task.State.LastFinished = now
	task.State.LastResult = completion.result
	switch completion.result.Status {
	case "ok":
		task.State.Counts.OK++
	case "problem":
		task.State.Counts.Problem++
	default:
		task.State.Counts.Unknown++
	}
	check, _, _ := taskIntervals(task.Every, task.ReportEvery)
	if !task.State.NextRun.After(now) {
		task.State.NextRun, task.State.Counts.Skipped = skipTaskIntervals(task.State.NextRun, now, check, task.State.Counts.Skipped)
	}
	status := completion.result.Status
	if status == "problem" && task.State.Confirmed != "problem" {
		task.State.PendingNotice = fmt.Sprintf("Task %s: problem\n%s\n%s", task.ID, task.Prompt, completion.result.Summary)
	} else if status == "ok" && task.State.Confirmed == "problem" {
		task.State.PendingNotice = fmt.Sprintf("Task %s: recovery\n%s\n%s", task.ID, task.Prompt, completion.result.Summary)
	}
	if status == "ok" || status == "problem" {
		task.State.Confirmed = status
	}
	return s.save()
}

func skipTaskIntervals(next, now time.Time, interval time.Duration, skipped int) (time.Time, int) {
	count := int(now.Sub(next)/interval) + 1
	return now.Add(interval - now.Sub(next)%interval), skipped + count
}

func taskReport(task scheduledTask, end time.Time) string {
	c := task.State.Counts
	return fmt.Sprintf("Task %s summary\n%s\n"+
		"Period: %s — %s\n"+
		"Completed: %d; healthy: %d; problems: %d; unknown: %d.\n"+
		"Interrupted: %d; skipped intervals: %d.\n"+
		"Last result: %s\n%s\n"+
		"Without explicit criteria in the task prompt, the model decides what counts as a problem.",
		task.ID, task.Prompt, task.State.PeriodStart.Format(time.RFC3339), end.Format(time.RFC3339),
		c.OK+c.Problem+c.Unknown, c.OK, c.Problem, c.Unknown, c.Interrupted, c.Skipped,
		task.State.LastResult.Status, task.State.LastResult.Summary)
}

func (s *taskScheduler) delivered(delivery taskDelivery) error {
	task := s.find(delivery.task.ID)
	if task == nil || task.revision != delivery.task.revision {
		return nil
	}
	if delivery.err != nil {
		s.bot.log(delivery.err)
		// Failed notices are covered by the periodic summary; do not retry every tick.
		if !delivery.report && task.State.PendingNotice == delivery.text {
			task.State.PendingNotice = ""
		}
	} else if delivery.report {
		before := delivery.task.State.Counts
		counts := &task.State.Counts
		counts.OK -= before.OK
		counts.Problem -= before.Problem
		counts.Unknown -= before.Unknown
		counts.Interrupted -= before.Interrupted
		counts.Skipped -= before.Skipped
		task.State.PeriodStart = delivery.end
	} else if task.State.PendingNotice == delivery.text {
		task.State.PendingNotice = ""
	}
	return s.save()
}
