package ui

// Production adapter wrapping chat.Provider (the daemon's
// configured chat backend — HTTP / claude-cli / codex-cli /
// claude-subscription / codex-subscription / router) behind
// the AssistantLLM interface used by the prompt-writing
// assistant handler.
//
// Why this exists alongside ChatClientAssistant:
//   - ChatClientAssistant builds a *chat.Client per call from a
//     raw (endpoint, apiKey, model) triple. That only works for
//     the OpenAI-compatible HTTP path — operators on Claude-CLI
//     or Codex-CLI have no endpoint to point it at.
//   - ProviderAssistant wraps whatever chat.Provider the daemon
//     already wired (c.ChatClient in container_chat.go). Every
//     backend that satisfies the Provider interface works,
//     including the subscription paths that don't expose an
//     OpenAI-style /v1/chat/completions surface at all.
//   - Per-call model selection happens via the ModelOverridable
//     interface when the underlying provider supports it (HTTP,
//     router); providers that don't (CLIs pinned at boot)
//     ignore the requested model and use their construction-
//     time pick.

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/chat"
)

// ProviderAssistant adapts a chat.Provider to AssistantLLM.
// Stateless — every Complete call walks through the provider's
// own dispatch (HTTP, queue, CLI subprocess, etc.) without
// holding extra state in this layer.
type ProviderAssistant struct {
	provider chat.Provider
}

// NewProviderAssistant returns an adapter pointed at the
// daemon's configured chat.Provider. A nil provider is rejected
// at construction so accidental wiring failures don't surface
// as misleading "no choices" errors later.
func NewProviderAssistant(provider chat.Provider) (*ProviderAssistant, error) {
	if provider == nil {
		return nil, errors.New("NewProviderAssistant: provider is nil")
	}
	return &ProviderAssistant{provider: provider}, nil
}

// Complete dispatches a system+user pair to the underlying
// provider. When the provider implements ModelOverridable and
// the requested model differs from the provider's own, the
// adapter pins a model-overridden shallow copy for this call;
// otherwise the request rides the provider's default model.
func (a *ProviderAssistant) Complete(ctx context.Context, model, system, user string) (*AssistantResult, error) {
	target := a.provider
	if model != "" && model != a.provider.Model() {
		if overridable, ok := a.provider.(chat.ModelOverridable); ok {
			target = overridable.WithModel(model)
		}
		// If the provider can't honor per-call model selection
		// (CLI providers fall here), we silently use the
		// provider's default. Operators see the actual model
		// the call ran with via the response's Model field, so
		// the discrepancy is visible without crashing the call.
	}

	resp, err := target.Complete(ctx, []chat.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("ProviderAssistant: LLM returned no choices")
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
