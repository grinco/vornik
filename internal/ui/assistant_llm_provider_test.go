package ui

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
)

// fakeProvider is a chat.Provider stub for the adapter tests.
// Implements Provider + optionally ModelOverridable depending
// on whether ModelOverride is set on the case.
type fakeProvider struct {
	model            string
	completeResp     *chat.ChatResponse
	completeErr      error
	gotMessages      []chat.Message
	overridableCalls []string // every WithModel(x) call records x
}

func (f *fakeProvider) Complete(_ context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	f.gotMessages = msgs
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	return f.completeResp, nil
}
func (f *fakeProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *fakeProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *fakeProvider) Model() string              { return f.model }
func (f *fakeProvider) SetMetrics(_ *chat.Metrics) {}

// overridableFakeProvider also implements ModelOverridable so
// WithModel("x") returns a fake pinned to x; the original
// records the call for assertion.
type overridableFakeProvider struct {
	*fakeProvider
}

func (o *overridableFakeProvider) WithModel(model string) chat.Provider {
	o.overridableCalls = append(o.overridableCalls, model)
	pinned := &fakeProvider{
		model:        model,
		completeResp: o.completeResp,
		completeErr:  o.completeErr,
	}
	return pinned
}

// TestProviderAssistant_HappyPath — the simplest case: provider
// returns a single choice; adapter unwraps it into an
// AssistantResult with token counts.
func TestProviderAssistant_HappyPath(t *testing.T) {
	resp := &chat.ChatResponse{
		Model: "default-model",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{
				Index:        0,
				Message:      chat.Message{Role: "assistant", Content: "hi from provider"},
				FinishReason: "stop",
			},
		},
	}
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 25
	p := &fakeProvider{model: "default-model", completeResp: resp}
	adapter, err := NewProviderAssistant(p)
	require.NoError(t, err)
	got, err := adapter.Complete(context.Background(), "default-model", "be helpful", "draft something")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "hi from provider", got.Text)
	assert.Equal(t, "default-model", got.Model)
	assert.Equal(t, 100, got.PromptTokens)
	assert.Equal(t, 25, got.CompletionTokens)

	// Messages reached the provider in system+user order.
	require.Len(t, p.gotMessages, 2)
	assert.Equal(t, "system", p.gotMessages[0].Role)
	assert.Equal(t, "be helpful", p.gotMessages[0].Content)
	assert.Equal(t, "user", p.gotMessages[1].Role)
	assert.Equal(t, "draft something", p.gotMessages[1].Content)
}

// TestProviderAssistant_NilProviderRejected — defensive: catch
// wiring mistakes at construction.
func TestProviderAssistant_NilProviderRejected(t *testing.T) {
	_, err := NewProviderAssistant(nil)
	require.Error(t, err)
}

// TestProviderAssistant_ModelOverrideHonored — when the
// underlying provider is ModelOverridable and the request asks
// for a different model, the adapter pins a fresh copy via
// WithModel before calling Complete.
func TestProviderAssistant_ModelOverrideHonored(t *testing.T) {
	resp := &chat.ChatResponse{
		Model: "claude-via-cli",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Content: "ok"}},
		},
	}
	inner := &fakeProvider{model: "default-model", completeResp: resp}
	overridable := &overridableFakeProvider{fakeProvider: inner}
	adapter, err := NewProviderAssistant(overridable)
	require.NoError(t, err)

	_, err = adapter.Complete(context.Background(), "stronger-model", "s", "u")
	require.NoError(t, err)
	// The override was requested.
	assert.Equal(t, []string{"stronger-model"}, overridable.overridableCalls)
	// And the inner provider was NOT called directly — the
	// model-pinned copy fielded it (so inner.gotMessages stays
	// empty).
	assert.Empty(t, inner.gotMessages)
}

// TestProviderAssistant_SameModelSkipsOverride — when the
// requested model matches the provider's default, no WithModel
// call fires (avoids an unnecessary copy on every assist call).
func TestProviderAssistant_SameModelSkipsOverride(t *testing.T) {
	resp := &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Content: "ok"}},
		},
	}
	inner := &fakeProvider{model: "default-model", completeResp: resp}
	overridable := &overridableFakeProvider{fakeProvider: inner}
	adapter, _ := NewProviderAssistant(overridable)
	_, err := adapter.Complete(context.Background(), "default-model", "s", "u")
	require.NoError(t, err)
	assert.Empty(t, overridable.overridableCalls, "same-model request should not fire WithModel")
}

// TestProviderAssistant_NonOverridableProviderUsesDefault —
// CLI providers don't implement ModelOverridable. The adapter
// silently uses the provider's default model rather than
// crashing. Operators see the actual model via the response's
// Model field.
func TestProviderAssistant_NonOverridableProviderUsesDefault(t *testing.T) {
	resp := &chat.ChatResponse{
		Model: "claude-via-cli",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Content: "cli reply"}},
		},
	}
	p := &fakeProvider{model: "claude-via-cli", completeResp: resp}
	adapter, err := NewProviderAssistant(p)
	require.NoError(t, err)
	got, err := adapter.Complete(context.Background(), "ignored-model", "s", "u")
	require.NoError(t, err)
	assert.Equal(t, "cli reply", got.Text)
	assert.Equal(t, "claude-via-cli", got.Model)
	// Inner provider WAS called (no copy was made since it's
	// not ModelOverridable).
	require.Len(t, p.gotMessages, 2)
}

// TestProviderAssistant_NoChoicesError — upstream returned
// empty choices; adapter surfaces a clear error rather than
// returning an empty suggestion.
func TestProviderAssistant_NoChoicesError(t *testing.T) {
	p := &fakeProvider{model: "x", completeResp: &chat.ChatResponse{}}
	adapter, _ := NewProviderAssistant(p)
	_, err := adapter.Complete(context.Background(), "x", "s", "u")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no choices")
}

// TestProviderAssistant_PropagatesProviderError — gateway
// errors surface verbatim so the handler can return 502 with
// the detail (rate-limit, auth, timeout).
func TestProviderAssistant_PropagatesProviderError(t *testing.T) {
	p := &fakeProvider{model: "x", completeErr: errors.New("rate limit")}
	adapter, _ := NewProviderAssistant(p)
	_, err := adapter.Complete(context.Background(), "x", "s", "u")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limit")
}

// TestProviderAssistant_PropagatesCacheTokens — Bedrock /
// Anthropic-Converse providers emit cache_creation_input_tokens
// and cache_read_input_tokens on prompt-prefix cache hits. The
// adapter must thread these through so the `_authoring` source
// on /ui/spend shows the same hit-ratio + savings as the
// dispatcher / executor / chat_proxy sources.
func TestProviderAssistant_PropagatesCacheTokens(t *testing.T) {
	resp := &chat.ChatResponse{
		Model: "claude-haiku-4-5",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Content: "cached reply"}},
		},
	}
	resp.Usage.PromptTokens = 500
	resp.Usage.CompletionTokens = 60
	resp.Usage.CacheCreationTokens = 1200
	resp.Usage.CacheReadTokens = 4800
	p := &fakeProvider{model: "claude-haiku-4-5", completeResp: resp}
	adapter, err := NewProviderAssistant(p)
	require.NoError(t, err)
	got, err := adapter.Complete(context.Background(), "claude-haiku-4-5", "s", "u")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1200, got.CacheCreationTokens)
	assert.Equal(t, 4800, got.CacheReadTokens)
}
