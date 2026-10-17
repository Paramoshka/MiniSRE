package agent

import (
	"encoding/json"
	"os"
	"time"
)

const (
	DefaultBaseURL    = "https://api.deepseek.com"
	DefaultModel      = "deepseek-flash"
	DefaultTimeout    = 180 * time.Second
	DefaultMaxRetries = 2
)

type ThinkingMode string

const (
	ThinkingEnabled  ThinkingMode = "enabled"
	ThinkingDisabled ThinkingMode = "disabled"
)

type Config struct {
	APIKey          string
	BaseURL         string
	Model           string
	Temperature     *float64
	MaxTokens       int
	MaxRetries      *int
	Thinking        ThinkingMode
	ReasoningEffort string
	Timeout         time.Duration
}

func (c Config) withDefaults() Config {
	if c.APIKey == "" {
		c.APIKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.Thinking == "" {
		c.Thinking = ThinkingDisabled
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxRetries == nil {
		n := DefaultMaxRetries
		c.MaxRetries = &n
	}
	return c
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role             Role
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	ToolCallID       string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	// Mutating is executor policy metadata; it is not sent to the model.
	Mutating bool
}

type Request struct {
	Messages         []Message
	Temperature      *float64
	MaxTokens        *int
	Tools            []Tool
	DisableToolCalls bool
}

type Chunk struct {
	Content          string
	ReasoningContent string
	FinishReason     string
}

type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	ReasoningTokens  int64
}

type Response struct {
	ID               string
	Model            string
	Content          string
	ReasoningContent string
	FinishReason     string
	ToolCalls        []ToolCall
	Usage            Usage
}
