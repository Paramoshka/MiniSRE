package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mini-sre/internal/agent"
	"mini-sre/internal/cli"
	"mini-sre/internal/config"
	"mini-sre/internal/dotenv"
	"mini-sre/internal/remote"
	"mini-sre/internal/session"
	"mini-sre/internal/telegram"
	"mini-sre/internal/tools"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mini-sre:", err)
		os.Exit(1)
	}
}

func run() (err error) {
	flag.String("model", "", "model ID (default deepseek-flash)")
	flag.String("base-url", "", "API base URL (default https://api.deepseek.com)")
	flag.Duration("model-timeout", agent.DefaultTimeout, "model HTTP attempt timeout; overrides DEEPSEEK_TIMEOUT")
	flag.Float64("temperature", -1, "sampling temperature 0..2 (-1 = server default); overrides DEEPSEEK_TEMPERATURE")
	flag.Bool("thinking", false, "enable thinking mode; overrides DEEPSEEK_THINKING")
	flag.String("reasoning-effort", "", "thinking effort: low, high or max; overrides DEEPSEEK_REASONING_EFFORT")
	var (
		showVersion   = flag.Bool("version", false, "print version and commit, then exit")
		reasoning     = flag.Bool("reasoning", false, "print reasoning content to stderr")
		stream        = flag.Bool("stream", true, "stream tokens as they arrive")
		configPath    = flag.String("config", "", "host and Telegram configuration YAML (default local only)")
		telegramMode  = flag.Bool("telegram", false, "run the Telegram bot instead of the console")
		telegramState = flag.String("telegram-state", "", "Telegram state file (default $XDG_STATE_HOME/mini-sre/telegram.json or ~/.local/state/mini-sre/telegram.json)")
		telegramTasks = flag.String("telegram-tasks", "", "Scheduled tasks JSON file (default tasks.json beside Telegram state)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Printf("MiniSRE %s (%s)\n", version, commit)
		return nil
	}

	if err := dotenv.Load(".env"); err != nil {
		return err
	}
	cfg, err := modelSettings(flag.CommandLine)
	if err != nil {
		return err
	}
	if *telegramMode && (*configPath == "" || len(flag.Args()) != 0) {
		return fmt.Errorf("telegram mode requires -config and does not accept a positional prompt")
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(signalCtx)
	defer cancel(nil)
	defer func() {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
	}()
	hostConfig, err := config.Watch(ctx, *configPath, *telegramMode, os.Stderr, cancel)
	if err != nil {
		return err
	}

	client, err := agent.New(cfg)
	if err != nil {
		return err
	}

	if *telegramMode {
		bot, err := telegram.New(os.Getenv("TELEGRAM_BOT_TOKEN"), func() []int64 {
			return hostConfig.Current().Telegram.AllowedUserIDs
		},
			func() *session.Session {
				return &session.Session{Turn: client.Chat, Tools: toolSpecs(), RunTool: toolRunner(hostConfig.Current)}
			}, os.Stderr)
		if err != nil {
			return err
		}
		statePath := *telegramState
		bot.SetLanguage(func() string { return hostConfig.Current().Language })
		if statePath == "" {
			statePath, err = telegram.DefaultStatePath()
			if err != nil {
				return err
			}
		}
		if err := bot.UseState(statePath); err != nil {
			return err
		}
		tasksPath := *telegramTasks
		if tasksPath == "" {
			tasksPath = filepath.Join(filepath.Dir(statePath), "tasks.json")
		}
		if err := bot.UseTasks(tasksPath); err != nil {
			return err
		}
		return bot.Run(ctx)
	}

	app := &cli.App{
		Client:       client,
		In:           os.Stdin,
		Out:          os.Stdout,
		Err:          os.Stderr,
		SystemPrompt: cli.DefaultSystemPrompt,
		Language:     func() string { return hostConfig.Current().Language },
		Stream:       *stream,
		Reasoning:    *reasoning,
		Tools:        toolSpecs(),
		RunTool:      toolRunner(hostConfig.Current),
	}

	if args := flag.Args(); len(args) > 0 {
		return app.Ask(ctx, strings.Join(args, " "))
	}
	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func modelSettings(flags *flag.FlagSet) (agent.Config, error) {
	set := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	value := func(name, env string) string {
		if !set[name] {
			if text := strings.TrimSpace(os.Getenv(env)); text != "" {
				return text
			}
		}
		return flags.Lookup(name).Value.String()
	}
	cfg := agent.Config{
		Model:           flags.Lookup("model").Value.String(),
		BaseURL:         flags.Lookup("base-url").Value.String(),
		ReasoningEffort: value("reasoning-effort", "DEEPSEEK_REASONING_EFFORT"),
		Thinking:        agent.ThinkingDisabled,
	}
	timeout, err := time.ParseDuration(value("model-timeout", "DEEPSEEK_TIMEOUT"))
	if err != nil || timeout <= 0 {
		return cfg, errors.New("model timeout must be a positive duration, such as 180s or 3m")
	}
	cfg.Timeout = timeout
	thinking, err := strconv.ParseBool(value("thinking", "DEEPSEEK_THINKING"))
	if err != nil {
		return cfg, errors.New("DEEPSEEK_THINKING must be true or false")
	}
	if thinking {
		cfg.Thinking = agent.ThinkingEnabled
	}
	temperature, err := strconv.ParseFloat(value("temperature", "DEEPSEEK_TEMPERATURE"), 64)
	if err != nil {
		return cfg, errors.New("temperature must be a number between 0 and 2")
	}
	if temperature == -1 && (set["temperature"] || strings.TrimSpace(os.Getenv("DEEPSEEK_TEMPERATURE")) == "") {
		return cfg, nil
	}
	cfg.Temperature = &temperature
	return cfg, nil
}

func toolSpecs() []agent.Tool {
	specs := tools.Specs()
	out := make([]agent.Tool, 0, len(specs))
	for _, spec := range specs {
		out = append(out, agent.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  spec.Parameters,
		})
	}
	return out
}

func toolRunner(current func() config.Config) func(ctx context.Context, call agent.ToolCall) (string, error) {
	return func(ctx context.Context, call agent.ToolCall) (string, error) {
		// The connection and redaction must use the same configuration snapshot.
		registry := tools.Registry(&remote.Runner{Config: current()})
		run, ok := registry[call.Name]
		if !ok {
			return "", fmt.Errorf("unknown tool %q", call.Name)
		}
		var args json.RawMessage
		if call.Arguments != "" {
			args = json.RawMessage(call.Arguments)
		}
		return run(ctx, args)
	}
}
