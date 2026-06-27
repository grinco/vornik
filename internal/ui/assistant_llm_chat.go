package ui

// Production adapter wiring internal/chat.Client behind the
// AssistantLLM interface used by the prompt-writing assistant
// handler. Daemon bootstrap code instantiates this and passes
// it to the UI Server via WithAssistantLLM.
//
// Kept in its own file (not in assistant.go) so the test mock
// in assistant_test.go stays the canonical reference for the
// interface — operators reading the assistant handler don't
// have to wade through chat-client setup boilerplate.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/chat"
)

// ChatClientAssistant adapts the daemon's chat.Client to the
// AssistantLLM contract. Stateless beyond the endpoint / API
// key / timeout pinned at construction; a fresh chat.Client is
// built per Complete call so per-call model selection works
// without mutating shared state.
type ChatClientAssistant struct {
	endpoint string
	apiKey   string
	timeout  time.Duration
}

// NewChatClientAssistant returns an adapter pointed at an
// OpenAI-compatible chat-completions endpoint. timeout bounds
// each Complete call; pass 0 to inherit chat.Client's default
// (30s).
func NewChatClientAssistant(endpoint, apiKey string, timeout time.Duration) *ChatClientAssistant {
	return &ChatClientAssistant{
		endpoint: endpoint,
		apiKey:   apiKey,
		timeout:  timeout,
	}
}

// Complete sends a system+user pair and returns the assistant's
// reply + token usage. Errors propagate verbatim so the
// handler's JSON response surfaces the underlying gateway
// issue to operators (e.g. "context length exceeded" vs "rate
// limited" vs network).
func (a *ChatClientAssistant) Complete(ctx context.Context, model, system, user string) (*AssistantResult, error) {
	if a.endpoint == "" {
		return nil, errors.New("ChatClientAssistant: endpoint is empty")
	}
	if model == "" {
		return nil, errors.New("ChatClientAssistant: model is empty")
	}

	opts := []chat.ClientOption{}
	if a.timeout > 0 {
		opts = append(opts, chat.WithTimeout(a.timeout))
	}
	client := chat.NewClient(a.endpoint, a.apiKey, model, opts...)
	resp, err := client.Complete(ctx, []chat.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("ChatClientAssistant: LLM returned no choices")
	}
	return &AssistantResult{
		Text:                resp.Choices[0].Message.Content,
		Model:               resp.Model,
		PromptTokens:        resp.Usage.PromptTokens,
		CompletionTokens:    resp.Usage.CompletionTokens,
		CacheCreationTokens: resp.Usage.CacheCreationTokens,
		CacheReadTokens:     resp.Usage.CacheReadTokens,
	}, nil
}
