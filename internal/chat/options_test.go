// Coverage for the chat package's option setters across the
// claude-subscription and cli-client provider variants. These are
// trivial closures over single fields; a sweep test that
// constructs each provider with every option lands ~20+ functions
// of coverage at once.

package chat

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestClaudeSubscriptionClient_AllOptionsApply runs every
// WithClaudeSubscription* option through NewClaudeSubscriptionClient
// and verifies the resulting field. Mirrors the api/telegram
// option-sweep tests.
func TestClaudeSubscriptionClient_AllOptionsApply(t *testing.T) {
	catalog := []ModelInfo{{ID: "claude-opus-4-7"}}
	c := NewClaudeSubscriptionClient("claude-opus-4-7",
		WithClaudeSubscriptionAuthPath("/tmp/credentials.json"),
		WithClaudeSubscriptionLogger(zerolog.Nop()),
		WithClaudeSubscriptionTimeout(45*time.Second),
		WithClaudeSubscriptionMaxTokens(16384),
		WithClaudeSubscriptionThinkingBudget(2048),
		WithClaudeSubscriptionUserAgent("custom-agent/1.0"),
		WithClaudeSubscriptionModelCatalog(catalog),
	)
	if c.timeout != 45*time.Second {
		t.Errorf("timeout: got %v, want 45s", c.timeout)
	}
	if c.maxTokens != 16384 {
		t.Errorf("maxTokens: got %d, want 16384", c.maxTokens)
	}
	if c.thinkingBudget != 2048 {
		t.Errorf("thinkingBudget: got %d, want 2048", c.thinkingBudget)
	}
	if c.userAgent != "custom-agent/1.0" {
		t.Errorf("userAgent: got %q, want %q", c.userAgent, "custom-agent/1.0")
	}
	if c.Model() != "claude-opus-4-7" {
		t.Errorf("Model() = %q, want claude-opus-4-7", c.Model())
	}
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "claude-opus-4-7" {
		t.Errorf("ListModels: got %+v, want catalog of one", got)
	}
}

// TestClaudeSubscriptionClient_SetMetricsLandsMetrics — post-
// construction setter for the metrics injected by the router after
// the prometheus registry is wired.
func TestClaudeSubscriptionClient_SetMetricsLandsMetrics(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-opus-4-7")
	m := &Metrics{}
	c.SetMetrics(m)
	if c.metrics != m {
		t.Error("SetMetrics: not applied")
	}
}

// TestClaudeSubscriptionClient_ListModels_EmptyReturnsNil pins the
// no-catalog path: the Anthropic API has no list-models endpoint,
// so when nothing was wired we return nil + nil rather than
// fabricating an empty slice that the caller would render as an
// empty list in the UI.
func TestClaudeSubscriptionClient_ListModels_EmptyReturnsNil(t *testing.T) {
	c := NewClaudeSubscriptionClient("any")
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels with no catalog: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil with empty catalog, got %+v", got)
	}
}

// TestClaudeSubscriptionClient_WithModel_ClonesAndOverrides verifies
// the ModelOverridable contract: a cloned provider with the new
// model shares auth/http/etc with the parent so refresh tokens
// rotated on one clone propagate everywhere.
func TestClaudeSubscriptionClient_WithModel_ClonesAndOverrides(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-opus-4-7")
	clone := c.WithModel("claude-sonnet-4-6")
	if clone == nil {
		t.Fatal("WithModel returned nil")
	}
	if clone.Model() != "claude-sonnet-4-6" {
		t.Errorf("clone Model() = %q, want claude-sonnet-4-6", clone.Model())
	}
	if c.Model() != "claude-opus-4-7" {
		t.Errorf("original Model() must not be mutated: got %q", c.Model())
	}
}

// TestClaudeSubscriptionClient_WithModelOnNilSafe — defensive: the
// ModelOverridable interface is checked at call sites without a nil
// guard, so the helper itself must handle nil receivers.
func TestClaudeSubscriptionClient_WithModelOnNilSafe(t *testing.T) {
	var c *ClaudeSubscriptionClient
	clone := c.WithModel("x")
	// Either clone is nil (one valid behaviour) or it's a usable
	// provider (the other) — but never a panic.
	_ = clone
}

// TestCLIClient_AllOptionsApply mirrors the subscription sweep for
// the cli-client variant. Same pattern: every WithCLI* option
// lands on the right field.
func TestCLIClient_AllOptionsApply(t *testing.T) {
	catalog := []ModelInfo{{ID: "claude-opus-4-7"}}
	c := NewCLIClient("claude-opus-4-7",
		WithCLITimeout(120*time.Second),
		WithCLILogger(zerolog.Nop()),
		WithCLIEffortLevel("high"),
		WithCLIModelCatalog(catalog),
	)
	if c.timeout != 120*time.Second {
		t.Errorf("timeout: got %v, want 120s", c.timeout)
	}
	if c.effortLevel != "high" {
		t.Errorf("effortLevel: got %q, want high", c.effortLevel)
	}
	if c.Model() != "claude-opus-4-7" {
		t.Errorf("Model() = %q, want claude-opus-4-7", c.Model())
	}
	got, _ := c.ListModels(context.Background())
	if len(got) != 1 {
		t.Errorf("ListModels: got %d entries, want 1", len(got))
	}
}

// TestCLIClient_DefaultEffortLevelLow pins the "low" default —
// changing it without an operator-facing change to vornik
// shouldn't slip through silently. The rationale is in the
// WithCLIEffortLevel docstring (latency vs reasoning trade-off).
func TestCLIClient_DefaultEffortLevelLow(t *testing.T) {
	c := NewCLIClient("any")
	if c.effortLevel != "low" {
		t.Errorf("default effortLevel: got %q, want %q", c.effortLevel, "low")
	}
}

// TestCLIClient_SetMetrics covers the post-construction hook
// with the nil-safe path. The label pre-registration logic for
// non-nil metrics requires real Prometheus vectors (covered by
// the dedicated metrics tests); we just exercise the nil branch
// here.
func TestCLIClient_SetMetrics(t *testing.T) {
	c := NewCLIClient("claude-opus-4-7")
	c.SetMetrics(nil) // nil-safe path — must not panic
}

// TestCLIClient_ListModels_EmptyReturnsNil mirrors the
// subscription variant's empty-catalog path.
func TestCLIClient_ListModels_EmptyReturnsNil(t *testing.T) {
	c := NewCLIClient("x")
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != nil {
		t.Errorf("empty catalog: got %+v, want nil", got)
	}
}
