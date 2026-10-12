package main

import (
	"flag"
	"testing"
	"time"

	"mini-sre/internal/agent"
)

func TestModelSettings(t *testing.T) {
	for _, test := range []struct {
		name         string
		thinking     string
		effort       string
		temperature  string
		args         []string
		wantThinking agent.ThinkingMode
		wantEffort   string
		wantTemp     *float64
		wantError    bool
	}{
		{name: "defaults", wantThinking: agent.ThinkingDisabled},
		{name: "environment", thinking: "true", effort: "max", temperature: "0.2", wantThinking: agent.ThinkingEnabled, wantEffort: "max", wantTemp: float64Ptr(0.2)},
		{name: "zero temperature", temperature: "0", wantThinking: agent.ThinkingDisabled, wantTemp: float64Ptr(0)},
		{name: "flags override invalid environment", thinking: "bad", effort: "bad", temperature: "bad", args: []string{"-thinking=false", "-reasoning-effort=low", "-temperature=2"}, wantThinking: agent.ThinkingDisabled, wantEffort: "low", wantTemp: float64Ptr(2)},
		{name: "server temperature override", temperature: "0.2", args: []string{"-temperature=-1"}, wantThinking: agent.ThinkingDisabled},
		{name: "invalid thinking", thinking: "bad", wantError: true},
		{name: "invalid temperature", temperature: "bad", wantError: true},
		{name: "negative temperature", temperature: "-1", wantError: true},
		{name: "high temperature", temperature: "2.1", wantError: true},
		{name: "nan temperature", temperature: "NaN", wantError: true},
		{name: "infinite temperature", temperature: "+Inf", wantError: true},
		{name: "invalid effort", effort: "medium", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DEEPSEEK_TIMEOUT", "")
			t.Setenv("DEEPSEEK_THINKING", test.thinking)
			t.Setenv("DEEPSEEK_REASONING_EFFORT", test.effort)
			t.Setenv("DEEPSEEK_TEMPERATURE", test.temperature)
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.String("model", "", "")
			flags.String("base-url", "", "")
			flags.Duration("model-timeout", agent.DefaultTimeout, "")
			flags.Bool("thinking", false, "")
			flags.String("reasoning-effort", "", "")
			flags.Float64("temperature", -1, "")
			if err := flags.Parse(test.args); err != nil {
				t.Fatal(err)
			}
			cfg, err := modelSettings(flags)
			if err == nil {
				cfg.APIKey = "test-key"
				_, err = agent.New(cfg)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %t", err, test.wantError)
			}
			if test.wantError {
				return
			}
			if cfg.Thinking != test.wantThinking || cfg.ReasoningEffort != test.wantEffort {
				t.Fatalf("wrong thinking settings: %+v", cfg)
			}
			if (cfg.Temperature == nil) != (test.wantTemp == nil) ||
				(cfg.Temperature != nil && *cfg.Temperature != *test.wantTemp) {
				t.Fatalf("temperature = %v, want %v", cfg.Temperature, test.wantTemp)
			}
		})
	}
}

func float64Ptr(value float64) *float64 { return &value }

func TestModelTimeoutSettings(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       string
		args      []string
		want      time.Duration
		wantError bool
	}{
		{name: "default", want: 180 * time.Second},
		{name: "environment", env: " 4m ", want: 4 * time.Minute},
		{name: "empty environment", env: " ", want: 180 * time.Second},
		{name: "flag", args: []string{"-model-timeout=90s"}, want: 90 * time.Second},
		{name: "flag overrides environment", env: "4m", args: []string{"-model-timeout=90s"}, want: 90 * time.Second},
		{name: "flag overrides invalid environment", env: "bad", args: []string{"-model-timeout=3m"}, want: 3 * time.Minute},
		{name: "invalid environment", env: "bad", wantError: true},
		{name: "missing unit", env: "180", wantError: true},
		{name: "zero environment", env: "0s", wantError: true},
		{name: "negative environment", env: "-1s", wantError: true},
		{name: "zero flag", args: []string{"-model-timeout=0s"}, wantError: true},
		{name: "negative flag", args: []string{"-model-timeout=-1s"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DEEPSEEK_TIMEOUT", test.env)
			t.Setenv("DEEPSEEK_THINKING", "")
			t.Setenv("DEEPSEEK_REASONING_EFFORT", "")
			t.Setenv("DEEPSEEK_TEMPERATURE", "")
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.String("model", "", "")
			flags.String("base-url", "", "")
			flags.Bool("thinking", false, "")
			flags.String("reasoning-effort", "", "")
			flags.Float64("temperature", -1, "")
			flags.Duration("model-timeout", agent.DefaultTimeout, "")
			if err := flags.Parse(test.args); err != nil {
				t.Fatal(err)
			}
			cfg, err := modelSettings(flags)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %t", err, test.wantError)
			}
			if !test.wantError && cfg.Timeout != test.want {
				t.Fatalf("timeout = %s, want %s", cfg.Timeout, test.want)
			}
		})
	}
}
