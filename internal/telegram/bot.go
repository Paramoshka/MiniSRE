package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/remote"
	"mini-sre/internal/session"
)

const maxHistoryTurns = 20

type Bot struct {
	token         string
	users         func() []int64
	newSession    func() *session.Session
	language      func() string
	sessions      map[int64]*session.Session
	errOut        io.Writer
	client        *http.Client
	baseURL       string
	offset        int64
	statePath     string
	offsetSavedAt time.Time
	tasks         *taskScheduler
	logMu         sync.Mutex
}

type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}

type message struct {
	Text string `json:"text"`
	From *struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}

func New(token string, users func() []int64, newSession func() *session.Session, errOut io.Writer) (*Bot, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("telegram: TELEGRAM_BOT_TOKEN is required")
	}
	if users == nil {
		return nil, errors.New("telegram: allowed_user_ids must not be empty")
	}
	initialUsers := users()
	if len(initialUsers) == 0 {
		return nil, errors.New("telegram: allowed_user_ids must not be empty")
	}
	for _, id := range initialUsers {
		if id <= 0 {
			return nil, errors.New("telegram: allowed_user_ids must be positive")
		}
	}
	client := &http.Client{Timeout: 45 * time.Second}
	if proxy := strings.TrimSpace(os.Getenv("TELEGRAM_HTTP_PROXY")); proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") || proxyURL.Hostname() == "" {
			return nil, errors.New("telegram: TELEGRAM_HTTP_PROXY must be an http:// or https:// proxy URL")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyURL(proxyURL)
		client.Transport = transport
	}
	return &Bot{
		token: token, users: users, newSession: newSession,
		sessions: make(map[int64]*session.Session), errOut: errOut,
		client: client, baseURL: "https://api.telegram.org",
	}, nil
}

// SetLanguage supplies the current response language; call before Run.
func (b *Bot) SetLanguage(language func() string) {
	b.language = language
}

func (b *Bot) currentLanguage() string {
	if b.language != nil {
		return b.language()
	}
	return "ru"
}

func (b *Bot) Run(ctx context.Context) (runErr error) {
	var webhook struct {
		URL string `json:"url"`
	}
	if err := b.callWithRetry(ctx, "getWebhookInfo", struct{}{}, &webhook); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if webhook.URL != "" {
		return errors.New("telegram: webhook is configured; remove it manually before using polling")
	}
	if b.tasks != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- b.tasks.run(ctx)
			cancel()
		}()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				runErr = err
			}
		}()
	}
	delay := time.Second
	for ctx.Err() == nil {
		// Telegram expires queued updates after 24h and can later reuse lower IDs.
		if b.offset != 0 && time.Since(b.offsetSavedAt) >= 24*time.Hour {
			b.offset = 0
		}
		var updates []update
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset": b.offset, "timeout": 30, "allowed_updates": []string{"message"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !retryable(err) {
				return err
			}
			b.log(err)
			if err := wait(ctx, retryDelay(err, delay)); err != nil {
				return nil
			}
			delay = min(delay*2, 30*time.Second)
			continue
		}
		delay = time.Second
		sort.Slice(updates, func(i, j int) bool { return updates[i].ID < updates[j].ID })
		for _, u := range updates {
			if ctx.Err() != nil {
				return nil
			}
			if u.ID < b.offset {
				continue
			}
			if err := b.process(ctx, u); err != nil {
				var stateErr *stateError
				if errors.As(err, &stateErr) {
					return err
				}
				if ctx.Err() != nil {
					return nil
				}
				var apiErr *apiError
				if errors.As(err, &apiErr) && apiErr.Code == http.StatusUnauthorized {
					return err
				}
				b.log(err)
			}
		}
	}
	return nil
}

func (b *Bot) process(ctx context.Context, u update) error {
	var progressID int64
	text, err := b.handle(ctx, u.Message, u.ID, func() {
		progressCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var sent struct {
			ID int64 `json:"message_id"`
		}
		err := b.call(progressCtx, "sendMessage", map[string]any{
			"chat_id": u.Message.Chat.ID, "text": "⏳ Принял, разбираюсь…",
		}, &sent)
		if err != nil {
			b.log(err)
			return
		}
		progressID = sent.ID
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	nextOffset, savedAt := u.ID+1, time.Now().UTC()
	started := time.Now()
	err = b.saveState(nextOffset, savedAt)
	if text != "" {
		b.log(fmt.Errorf("telegram: update=%d stage=state duration=%s failed=%t", u.ID, time.Since(started), err != nil))
	}
	if err != nil {
		return err
	}
	b.offset, b.offsetSavedAt = nextOffset, savedAt
	if text == "" {
		return nil
	}
	started = time.Now()
	b.log(fmt.Errorf("telegram: update=%d stage=send started", u.ID))
	err = b.sendReply(ctx, u.Message.Chat.ID, progressID, text)
	b.log(fmt.Errorf("telegram: update=%d stage=send duration=%s failed=%t", u.ID, time.Since(started), err != nil))
	return err
}

func (b *Bot) handle(ctx context.Context, m *message, updateID int64, onStart func()) (string, error) {
	if m == nil || m.From == nil || m.Chat.Type != "private" || !slices.Contains(b.users(), m.From.ID) || strings.TrimSpace(m.Text) == "" {
		return "", nil
	}
	input := strings.TrimSpace(m.Text)
	language := b.currentLanguage()
	if input == "/task" || strings.HasPrefix(input, "/task ") || strings.HasPrefix(input, "/task\n") || strings.HasPrefix(input, "/task\t") {
		if b.tasks == nil {
			return localized(language, "Плановые задачи не включены для этого процесса.", "Scheduled tasks are not enabled for this process."), nil
		}
		return b.tasks.command(ctx, m, updateID, input)
	}
	switch input {
	case "/start":
		return "Send a request or forward a problem report. Specify the host and service; local is the agent machine. /clear resets history. /task manages scheduled tasks.", nil
	case "/clear":
		if s := b.sessions[m.Chat.ID]; s != nil {
			s.Reset()
		}
		return "History cleared.", nil
	}
	if strings.HasPrefix(input, "/") {
		return localized(language, "Неизвестная команда. /start — справка, /clear — очистить историю.", "Unknown command. /start — help, /clear — reset history."), nil
	}
	onStart()
	s := b.sessions[m.Chat.ID]
	if s == nil {
		s = b.newSession()
		s.MaxHistoryTurns = maxHistoryTurns
		b.sessions[m.Chat.ID] = s
	}
	s.Language = language
	turn, runTool := s.Turn, s.RunTool
	defer func() {
		s.Turn, s.RunTool = turn, runTool
	}()
	if turn != nil {
		round := 0
		s.Turn = func(ctx context.Context, req agent.Request) (*agent.Response, error) {
			started := time.Now()
			round++
			b.log(fmt.Errorf("telegram: update=%d stage=model started round=%d disable_tool_calls=%t", updateID, round, req.DisableToolCalls))
			resp, err := turn(ctx, req)
			finishReason, calls := "", 0
			if resp != nil {
				finishReason, calls = resp.FinishReason, len(resp.ToolCalls)
			}
			b.log(fmt.Errorf("telegram: update=%d stage=model duration=%s failed=%t round=%d disable_tool_calls=%t finish_reason=%q tool_calls=%d", updateID, time.Since(started), err != nil, round, req.DisableToolCalls, finishReason, calls))
			return resp, err
		}
	}
	if runTool != nil {
		s.RunTool = func(ctx context.Context, call agent.ToolCall) (string, error) {
			started := time.Now()
			b.log(fmt.Errorf("telegram: update=%d stage=tool name=%q started", updateID, call.Name))
			out, err := runTool(ctx, call)
			entry := fmt.Sprintf("telegram: update=%d stage=tool name=%q duration=%s failed=%t", updateID, call.Name, time.Since(started), err != nil)
			if err != nil {
				entry += fmt.Sprintf(" error=%q", remote.RedactRuntimeSecrets(err.Error(), b.token))
			}
			b.log(errors.New(entry))
			return out, err
		}
	}
	resp, err := s.Ask(ctx, input)
	if err != nil {
		b.log(err)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return localized(language,
				"Модель не ответила за отведённое время. Действия, выполненные до ошибки, могли сохраниться. Подробности — в журнале агента.",
				"The model did not respond within the allotted time. Actions completed before the error may have persisted. See the agent log for details."), nil
		}
		if errors.Is(err, session.ErrResponseFormat) {
			return localized(language,
				"Не удалось сформировать итог диагностики: модель повторно вернула некорректный ответ. Предыдущие проверки или действия могли выполниться; я не запускал их повторно для исправления ответа. Подробности — в журнале агента.",
				"Could not produce the diagnostic summary: the model returned an invalid response again. Earlier checks or actions may have completed; they were not rerun to repair the response. See the agent log for details."), nil
		}
		return localized(language, "Не удалось выполнить запрос. Подробности — в журнале агента.", "Could not complete the request. See the agent log for details."), nil
	}
	text := resp.Content
	if strings.TrimSpace(text) == "" {
		text = localized(language, "Модель вернула пустой ответ. Попробуйте уточнить запрос.", "The model returned an empty response. Try a more specific request.")
	}
	return text, nil
}

func (b *Bot) send(ctx context.Context, chatID int64, text string) error {
	return b.sendReply(ctx, chatID, 0, text)
}

func (b *Bot) sendReply(ctx context.Context, chatID, progressID int64, text string) error {
	for _, part := range formatReply(text) {
		params := map[string]any{"chat_id": chatID, "text": part.Text}
		if len(part.Entities) > 0 {
			params["entities"] = part.Entities
		}
		if progressID != 0 {
			params["message_id"] = progressID
			err := b.callWithRetry(ctx, "editMessageText", params, nil)
			progressID = 0
			var apiErr *apiError
			if err == nil || (errors.As(err, &apiErr) && apiErr.Code == 400 && strings.Contains(apiErr.Description, "message is not modified")) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.log(err)
			delete(params, "message_id")
		}
		if err := b.callWithRetry(ctx, "sendMessage", params, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) log(err error) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	if b.errOut != nil {
		fmt.Fprintln(b.errOut, remote.RedactRuntimeSecrets(err.Error(), b.token))
	}
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
