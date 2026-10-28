package agent

import (
	"context"
	"fmt"
	"iter"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

type Stream struct {
	stream *ssestream.Stream[openai.ChatCompletionChunk]
	resp   Response
}

func (c *AgentClient) ChatStream(ctx context.Context, req Request) (*Stream, error) {
	params, err := c.buildParams(req)
	if err != nil {
		return nil, err
	}
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: openai.Bool(true),
	}

	inner := c.client.Chat.Completions.NewStreaming(ctx, params, c.thinkingOption())
	if err := inner.Err(); err != nil {
		return nil, fmt.Errorf("agent: chat stream: %w", classifyError(err))
	}
	return &Stream{stream: inner}, nil
}

func (s *Stream) Chunks() iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		defer s.stream.Close()

		for s.stream.Next() {
			raw := s.stream.Current()
			var chunk Chunk
			if len(raw.Choices) > 0 {
				choice := raw.Choices[0]
				chunk = Chunk{
					Content:          choice.Delta.Content,
					ReasoningContent: extraString(choice.Delta.JSON.ExtraFields, "reasoning_content"),
					FinishReason:     choice.FinishReason,
				}
				s.mergeToolCalls(choice.Delta.ToolCalls)
			}
			s.accumulate(raw, chunk)
			if !yield(chunk, nil) {
				return
			}
		}
		if err := s.stream.Err(); err != nil {
			yield(Chunk{}, fmt.Errorf("agent: chat stream: %w", classifyError(err)))
		}
	}
}

func (s *Stream) Response() Response {
	return s.resp
}

func (s *Stream) accumulate(raw openai.ChatCompletionChunk, chunk Chunk) {
	if s.resp.ID == "" {
		s.resp.ID = raw.ID
	}
	if s.resp.Model == "" {
		s.resp.Model = raw.Model
	}
	s.resp.Content += chunk.Content
	s.resp.ReasoningContent += chunk.ReasoningContent
	if chunk.FinishReason != "" {
		s.resp.FinishReason = chunk.FinishReason
	}
	if raw.JSON.Usage.Valid() {
		s.resp.Usage = usageFrom(raw.Usage)
	}
}

func (s *Stream) mergeToolCalls(deltas []openai.ChatCompletionChunkChoiceDeltaToolCall) {
	for _, delta := range deltas {
		index := int(delta.Index)
		for len(s.resp.ToolCalls) <= index {
			s.resp.ToolCalls = append(s.resp.ToolCalls, ToolCall{})
		}

		call := &s.resp.ToolCalls[index]
		if delta.ID != "" {
			call.ID = delta.ID
		}
		call.Name += delta.Function.Name
		call.Arguments += delta.Function.Arguments
	}
}
