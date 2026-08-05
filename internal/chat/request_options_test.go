package chat

import (
	"context"
	"testing"
)

// TestWithRequestMaxTokens — the round-trip contract: a value set
// via WithRequestMaxTokens is recoverable via MaxTokensFromContext;
// 0 / negative inputs are coerced to "absent" so a malformed agent
// payload doesn't accidentally raise the cap.
func TestWithRequestMaxTokens(t *testing.T) {
	ctx := context.Background()
	if got := MaxTokensFromContext(ctx); got != 0 {
		t.Errorf("empty ctx: got %d, want 0", got)
	}

	ctx = WithRequestMaxTokens(ctx, 4096)
	if got := MaxTokensFromContext(ctx); got != 4096 {
		t.Errorf("set 4096: got %d", got)
	}

	// Setting 0 is a no-op (returns the parent ctx unchanged).
	ctx2 := WithRequestMaxTokens(ctx, 0)
	if got := MaxTokensFromContext(ctx2); got != 4096 {
		t.Errorf("set 0 should preserve parent value: got %d", got)
	}

	// Negative values coerced to absent.
	ctx3 := WithRequestMaxTokens(context.Background(), -1)
	if got := MaxTokensFromContext(ctx3); got != 0 {
		t.Errorf("negative input: got %d, want 0", got)
	}
}

// TestWithRequestPromptCacheKey — same set/no-op contract for the
// OpenAI prompt_cache_key steering hint (BACKLOG "OpenAI-compat
// prompt_cache_key passthrough").
func TestWithRequestPromptCacheKey(t *testing.T) {
	ctx := context.Background()
	if got := PromptCacheKeyFromContext(ctx); got != "" {
		t.Errorf("empty ctx: got %q, want \"\"", got)
	}

	ctx = WithRequestPromptCacheKey(ctx, "assistant:researcher")
	if got := PromptCacheKeyFromContext(ctx); got != "assistant:researcher" {
		t.Errorf("set key: got %q", got)
	}

	// Empty key is a no-op (returns the parent ctx unchanged).
	ctx2 := WithRequestPromptCacheKey(ctx, "")
	if got := PromptCacheKeyFromContext(ctx2); got != "assistant:researcher" {
		t.Errorf("set empty should preserve parent: got %q", got)
	}
}

// TestWithRequestResponseFormat — same contract for the
// response_format directive.
func TestWithRequestResponseFormat(t *testing.T) {
	ctx := context.Background()
	if got := ResponseFormatFromContext(ctx); got != "" {
		t.Errorf("empty ctx: got %q", got)
	}

	ctx = WithRequestResponseFormat(ctx, "json_object")
	if got := ResponseFormatFromContext(ctx); got != "json_object" {
		t.Errorf("set json_object: got %q", got)
	}

	// Empty string is a no-op (returns parent unchanged).
	ctx2 := WithRequestResponseFormat(ctx, "")
	if got := ResponseFormatFromContext(ctx2); got != "json_object" {
		t.Errorf("set empty should preserve parent: got %q", got)
	}
}

// TestNilContext_SafeAccessors — both helpers return zero-values on
// a nil context. Defensive: callers that forget to thread context
// shouldn't panic.
//
// We deliberately pass an explicitly-nil context.Context here (the
// staticcheck SA1012 lint is suppressed via the typed nil
// assignment). The contract under test is exactly "what happens
// when the caller mistakenly passes nil" — point of the
// defensive-zero-value guard.
func TestNilContext_SafeAccessors(t *testing.T) {
	var nilCtx context.Context
	if got := MaxTokensFromContext(nilCtx); got != 0 {
		t.Errorf("nil ctx max_tokens: got %d", got)
	}
	if got := ResponseFormatFromContext(nilCtx); got != "" {
		t.Errorf("nil ctx response_format: got %q", got)
	}
	if got := PromptCacheKeyFromContext(nilCtx); got != "" {
		t.Errorf("nil ctx prompt_cache_key: got %q", got)
	}
}

func TestWithReasoningEffort(t *testing.T) {
	ctx := context.Background()
	if got := ReasoningEffortFromContext(ctx); got != "" {
		t.Errorf("unset must be empty, got %q", got)
	}
	if got := ReasoningEffortFromContext(nil); got != "" { //nolint:staticcheck // nil ctx is a real caller mistake worth covering
		t.Errorf("nil ctx must be empty, got %q", got)
	}
	for _, lvl := range []string{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh} {
		if got := ReasoningEffortFromContext(WithReasoningEffort(ctx, lvl)); got != lvl {
			t.Errorf("effort %q round-trip = %q", lvl, got)
		}
	}
	// A typo must not silently reach the provider as a made-up level: an
	// unrecognised value leaves the model's own default in place rather than
	// changing behaviour in an unpredictable direction.
	for _, bad := range []string{"", "LOW", "lowest", "none", "off", "0"} {
		if got := ReasoningEffortFromContext(WithReasoningEffort(ctx, bad)); got != "" {
			t.Errorf("unrecognised effort %q must be ignored, got %q", bad, got)
		}
	}
}
