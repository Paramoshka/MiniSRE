package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"mini-sre/internal/agent"
	"mini-sre/internal/session"
)

const (
	DefaultSystemPrompt = session.DefaultSystemPrompt
	prompt              = "mini-sre> "
)

type Chatter interface {
	ChatStream(ctx context.Context, req agent.Request) (*agent.Stream, error)
	Chat(ctx context.Context, req agent.Request) (*agent.Response, error)
}

type App struct {
	Client       Chatter
	In           io.Reader
	Out          io.Writer
	Err          io.Writer
	SystemPrompt string
	Language     func() string
	Stream       bool
	Reasoning    bool
	Tools        []agent.Tool
	RunTool      func(ctx context.Context, call agent.ToolCall) (string, error)

	session *session.Session
	usage   agent.Usage
}

func (a *App) Run(ctx context.Context) error {
	a.ensureSession()
	if closer, ok := a.In.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() {
			// Best effort: closing input releases a blocked read; Run returns the context error.
			_ = closer.Close()
		})
		defer func() {
			if ctx.Err() == nil {
				stop()
			}
		}()
	}

	scanner := bufio.NewScanner(a.In)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprint(a.Out, prompt)
		text, err := scanLine(ctx, scanner)
		if err == io.EOF {
			fmt.Fprintln(a.Out)
			return nil
		}
		if err != nil {
			return err
		}

		line := strings.TrimSpace(text)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if a.handleCommand(line) {
				return nil
			}
			continue
		}
		if err := a.Ask(ctx, line); err != nil {
			fmt.Fprintf(a.Err, "error: %v\n", err)
		}
	}
}

func scanLine(ctx context.Context, scanner *bufio.Scanner) (string, error) {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if scanner.Scan() {
			done <- result{text: scanner.Text()}
			return
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		done <- result{err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return r.text, r.err
	}
}

func (a *App) Ask(ctx context.Context, input string) error {
	a.ensureSession()
	if a.Language != nil {
		a.session.Language = a.Language()
	}
	_, err := a.session.Ask(ctx, input)
	return err
}

func (a *App) turn(ctx context.Context, req agent.Request) (*agent.Response, error) {
	if !a.Stream {
		resp, err := a.Client.Chat(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.Content != "" {
			fmt.Fprintln(a.Out, resp.Content)
		}
		a.usage = resp.Usage
		a.printUsage()
		return resp, nil
	}

	stream, err := a.Client.ChatStream(ctx, req)
	if err != nil {
		return nil, err
	}

	reasoningOpen := false
	wroteContent := false
	for chunk, err := range stream.Chunks() {
		if err != nil {
			return nil, err
		}
		if a.Reasoning && chunk.ReasoningContent != "" {
			fmt.Fprint(a.Err, chunk.ReasoningContent)
			reasoningOpen = true
		}
		if chunk.Content != "" {
			if reasoningOpen {
				fmt.Fprintln(a.Err)
				reasoningOpen = false
			}
			fmt.Fprint(a.Out, chunk.Content)
			wroteContent = true
		}
	}
	if reasoningOpen {
		fmt.Fprintln(a.Err)
	}
	if wroteContent {
		fmt.Fprintln(a.Out)
	}

	resp := stream.Response()
	a.usage = resp.Usage
	a.printUsage()
	return &resp, nil
}

func (a *App) handleCommand(line string) (quit bool) {
	switch line {
	case "/exit", "/quit":
		return true
	case "/clear":
		a.session.Reset()
	case "/usage":
		fmt.Fprintf(a.Out, "tokens: prompt=%d completion=%d total=%d, cache: hit=%d miss=%d\n",
			a.usage.PromptTokens, a.usage.CompletionTokens, a.usage.TotalTokens,
			a.usage.CacheHitTokens, a.usage.CacheMissTokens)
	default:
		fmt.Fprintf(a.Err, "unknown command: %s\n", line)
	}
	return false
}

func (a *App) printUsage() {
	if a.usage.TotalTokens == 0 {
		return
	}
	fmt.Fprintf(a.Err, "[tokens prompt=%d completion=%d total=%d, cache hit=%d miss=%d]\n",
		a.usage.PromptTokens, a.usage.CompletionTokens, a.usage.TotalTokens,
		a.usage.CacheHitTokens, a.usage.CacheMissTokens)
}

func (a *App) ensureSession() {
	if a.session != nil {
		return
	}
	a.session = &session.Session{
		Turn: a.turn, Tools: a.Tools, RunTool: a.RunTool, SystemPrompt: a.SystemPrompt,
		OnToolCall: func(call agent.ToolCall) {
			fmt.Fprintf(a.Err, "→ %s(%s)\n", call.Name, call.Arguments)
		},
	}
	a.session.Reset()
}
