// Package agent openai compatable
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"
)

type AgentClient struct {
	cfg    Config
	client openai.Client
}

func New(cfg Config) (*AgentClient, error) {
	cfg = cfg.withDefaults()

	if cfg.APIKey == "" {
		return nil, errors.New("agent: api key is required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("agent: timeout must be positive")
	}
	if *cfg.MaxRetries < 0 {
		return nil, errors.New("agent: max retries must be >= 0")
	}
	if cfg.Thinking != ThinkingEnabled && cfg.Thinking != ThinkingDisabled {
		return nil, errors.New("agent: thinking must be enabled or disabled")
	}
	if cfg.ReasoningEffort != "" && cfg.ReasoningEffort != "low" && cfg.ReasoningEffort != "high" && cfg.ReasoningEffort != "max" {
		return nil, errors.New("agent: reasoning effort must be low, high or max")
	}
	if cfg.Temperature != nil && (math.IsNaN(*cfg.Temperature) || *cfg.Temperature < 0 || *cfg.Temperature > 2) {
		return nil, errors.New("agent: temperature must be between 0 and 2")
	}

	client := openai.NewClient(
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(cfg.BaseURL),
		option.WithRequestTimeout(cfg.Timeout),
		option.WithMaxRetries(*cfg.MaxRetries),
	)

	return &AgentClient{cfg: cfg, client: client}, nil
}

func (c *AgentClient) Chat(ctx context.Context, req Request) (*Response, error) {
	params, err := c.buildParams(req)
	if err != nil {
		return nil, err
	}

	completion, err := c.client.Chat.Completions.New(ctx, params, c.thinkingOption())
	if err != nil {
		return nil, fmt.Errorf("agent: chat: %w", classifyError(err))
	}
	if len(completion.Choices) == 0 {
		return nil, errors.New("agent: chat: response has no choices")
	}

	choice := completion.Choices[0]
	return &Response{
		ID:               completion.ID,
		Model:            completion.Model,
		Content:          choice.Message.Content,
		ReasoningContent: extraString(choice.Message.JSON.ExtraFields, "reasoning_content"),
		FinishReason:     choice.FinishReason,
		ToolCalls:        toolCallsFrom(choice.Message.ToolCalls),
		Usage:            usageFrom(completion.Usage),
	}, nil
}

func (c *AgentClient) buildParams(req Request) (openai.ChatCompletionNewParams, error) {
	messages, err := messageParams(req.Messages)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    c.cfg.Model,
		Messages: messages,
	}
	if c.cfg.Thinking == ThinkingEnabled && c.cfg.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(c.cfg.ReasoningEffort)
	}

	temperature := c.cfg.Temperature
	if req.Temperature != nil {
		temperature = req.Temperature
	}
	if temperature != nil {
		if math.IsNaN(*temperature) || *temperature < 0 || *temperature > 2 {
			return openai.ChatCompletionNewParams{}, errors.New("agent: temperature must be between 0 and 2")
		}
		params.Temperature = openai.Float(*temperature)
	}

	maxTokens := c.cfg.MaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	if maxTokens > 0 {
		params.MaxTokens = openai.Int(int64(maxTokens))
	}

	tools, err := toolParams(req.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	params.Tools = tools
	if req.DisableToolCalls {
		params.ToolChoice.OfAuto = openai.String("none")
	}

	return params, nil
}

func (c *AgentClient) thinkingOption() option.RequestOption {
	return option.WithJSONSet("thinking", map[string]string{"type": string(c.cfg.Thinking)})
}

func usageFrom(raw openai.CompletionUsage) Usage {
	return Usage{
		PromptTokens:     raw.PromptTokens,
		CompletionTokens: raw.CompletionTokens,
		TotalTokens:      raw.TotalTokens,
		CacheHitTokens:   extraInt64(raw.JSON.ExtraFields, "prompt_cache_hit_tokens"),
		CacheMissTokens:  extraInt64(raw.JSON.ExtraFields, "prompt_cache_miss_tokens"),
		ReasoningTokens:  raw.CompletionTokensDetails.ReasoningTokens,
	}
}

func messageParams(messages []Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	if len(messages) == 0 {
		return nil, errors.New("agent: chat: messages are required")
	}

	params := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for i, m := range messages {
		switch m.Role {
		case RoleSystem:
			params = append(params, openai.SystemMessage(m.Content))
		case RoleUser:
			params = append(params, openai.UserMessage(m.Content))
		case RoleAssistant:
			params = append(params, assistantMessage(m))
		case RoleTool:
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("agent: chat: message %d: tool message requires tool_call_id", i)
			}
			params = append(params, openai.ToolMessage(m.Content, m.ToolCallID))
		default:
			return nil, fmt.Errorf("agent: chat: message %d has unknown role %q", i, m.Role)
		}
	}
	return params, nil
}

func assistantMessage(m Message) openai.ChatCompletionMessageParamUnion {
	var assistant openai.ChatCompletionAssistantMessageParam

	if m.Content != "" || len(m.ToolCalls) == 0 {
		assistant.Content.OfString = openai.String(m.Content)
	}
	if len(m.ToolCalls) > 0 {
		assistant.ToolCalls = make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolCalls))
		for _, call := range m.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: call.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      call.Name,
						Arguments: call.Arguments,
					},
				},
			})
		}
	}
	if m.ReasoningContent != "" {
		assistant.SetExtraFields(map[string]any{"reasoning_content": m.ReasoningContent})
	}

	return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}
}

func toolParams(tools []Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	params := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for i, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("agent: tool %d: name is required", i)
		}

		definition := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			definition.Description = openai.String(t.Description)
		}
		if len(t.Parameters) > 0 {
			var parameters shared.FunctionParameters
			if err := json.Unmarshal(t.Parameters, &parameters); err != nil {
				return nil, fmt.Errorf("agent: tool %q: invalid parameters schema: %w", t.Name, err)
			}
			definition.Parameters = parameters
		}

		params = append(params, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{Function: definition},
		})
	}
	return params, nil
}

func toolCallsFrom(calls []openai.ChatCompletionMessageToolCallUnion) []ToolCall {
	if len(calls) == 0 {
		return nil
	}

	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	return out
}

func extraInt64(fields map[string]respjson.Field, key string) int64 {
	field, ok := fields[key]
	if !ok || field.Raw() == "" || field.Raw() == respjson.Null {
		return 0
	}
	var value int64
	if err := json.Unmarshal([]byte(field.Raw()), &value); err != nil {
		return 0
	}
	return value
}

func extraString(fields map[string]respjson.Field, key string) string {
	field, ok := fields[key]
	if !ok || field.Raw() == "" || field.Raw() == respjson.Null {
		return ""
	}
	var value string
	if err := json.Unmarshal([]byte(field.Raw()), &value); err != nil {
		return ""
	}
	return value
}
