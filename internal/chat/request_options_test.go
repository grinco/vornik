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
}
