// Coverage sweep for the option-setter and post-construction
// helper surface across the chat package. These are all single-
// field closures the dispatcher / container wires unconditionally;
// each landing one line of "field set" assertion lifts a stack of
// 0%-covered funcs into the green without touching production code.
//
// Specifically covers:
//   - Client (HTTP/OpenAI-compatible): WithContextSize /
//     WithLogger / WithMetrics / SetMetrics
//   - CodexCLIClient: WithCodexTimeout /
//     WithCodexLogger / WithCodexModelCatalog / SetMetrics / ListModels
//   - CodexSubscriptionClient: every WithCodexSubscription* option +
//     Model() / ListModels() / WithModel()
//   - Conversation: CreatedAt / LastUsed / ReplaceMessages / DropLast

package chat

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// -- Client option sweep --------------------------------------------------

func TestClient_OptionSweep_LandsOnFields(t *testing.T) {
	hc := &http.Client{Timeout: 7 * time.Second}
	logger := zerolog.Nop()
	m := &Metrics{}
	models := []ModelInfo{{ID: "stub-1"}}

	c := NewClient("https://api.example.com", "key", "stub-1",
		WithTimeout(11*time.Second),
		WithContextSize(8192),
		WithMaxTokens(2048),
		WithStaticModelList(models),
		WithAuthHeader("X-Custom", "Tok "),
		WithHTTPClient(hc),
		WithLogger(logger),
		WithMetrics(m),
	)

	if c.timeout != 11*time.Second {
		t.Errorf("timeout: got %v, want 11s", c.timeout)
	}
	if c.contextSize != 8192 {
		t.Errorf("contextSize: got %d, want 8192", c.contextSize)
	}
	if c.maxTokens != 2048 {
		t.Errorf("maxTokens: got %d, want 2048", c.maxTokens)
	}
	if c.httpClient != hc {
		t.Errorf("WithHTTPClient: not applied")
	}
	if c.authHeader != "X-Custom" || c.authPrefix != "Tok " {
		t.Errorf("WithAuthHeader: got %q / %q", c.authHeader, c.authPrefix)
	}
	if !c.staticModelsSet || len(c.staticModels) != 1 {
		t.Errorf("WithStaticModelList: not applied (set=%v len=%d)",
			c.staticModelsSet, len(c.staticModels))
	}
	if c.metrics != m {
		t.Error("WithMetrics: not applied")
	}
	// WithLogger has no readable comparison hook beyond field
	// presence; setting it without panic is the assertion.
}

// SetMetrics is a separate post-construction code path (the
// container wires metrics after the client is built). The non-nil
// branch hits the label pre-registration. We use a real *Metrics so
// the prometheus vector ops succeed.
func TestClient_SetMetrics_NilAndReal(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "gpt-stub")
	c.SetMetrics(nil) // nil-safe
	if c.metrics != nil {
		t.Error("SetMetrics(nil): metrics should remain nil")
	}
	m := NewMetrics(prometheus.NewRegistry())
	c.SetMetrics(m)
	if c.metrics != m {
		t.Error("SetMetrics(m): not applied")
	}
}

// -- CodexCLIClient option sweep -----------------------------------------

func TestCodexCLIClient_OptionSweep(t *testing.T) {
	catalog := []ModelInfo{{ID: "gpt-5.4"}, {ID: "gpt-5.4-mini"}}
	c := NewCodexCLIClient("gpt-5.4-mini",
		WithCodexBinary("/usr/local/bin/codex"),
		WithCodexTimeout(33*time.Second),
		WithCodexLogger(zerolog.Nop()),
		WithCodexModelCatalog(catalog),
	)
	if c.binary != "/usr/local/bin/codex" {
		t.Errorf("binary: got %q", c.binary)
	}
	if c.timeout != 33*time.Second {
		t.Errorf("timeout: got %v, want 33s", c.timeout)
	}
	if c.Model() != "gpt-5.4-mini" {
		t.Errorf("Model: got %q", c.Model())
	}
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListModels: got %d entries, want 2", len(got))
	}
}

func TestCodexCLIClient_ListModelsEmpty(t *testing.T) {
	c := NewCodexCLIClient("gpt-5.4-mini")
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != nil {
		t.Errorf("empty catalog: got %+v, want nil", got)
	}
}

func TestCodexCLIClient_SetMetrics_NilAndReal(t *testing.T) {
	c := NewCodexCLIClient("gpt-5.4-mini")
	c.SetMetrics(nil) // nil branch
	if c.metrics != nil {
		t.Error("SetMetrics(nil): expected nil")
	}
	m := NewMetrics(prometheus.NewRegistry())
	c.SetMetrics(m)
	if c.metrics != m {
		t.Error("SetMetrics: not applied")
	}
}

// -- CodexSubscriptionClient option sweep ---------------------------------

func TestCodexSubscriptionClient_OptionSweep(t *testing.T) {
	catalog := []ModelInfo{{ID: "gpt-5.4"}}
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionAuthPath("/tmp/codex-auth.json"),
		WithCodexSubscriptionLogger(zerolog.Nop()),
		WithCodexSubscriptionTimeout(77*time.Second),
		WithCodexSubscriptionEffortLevel("high"),
		WithCodexSubscriptionModelCatalog(catalog),
	)
	if c.timeout != 77*time.Second {
		t.Errorf("timeout: got %v", c.timeout)
	}
	if c.effortLevel != "high" {
		t.Errorf("effortLevel: got %q", c.effortLevel)
	}
	if c.Model() != "gpt-5.4" {
		t.Errorf("Model: got %q", c.Model())
	}

	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gpt-5.4" {
		t.Errorf("ListModels: got %+v", got)
	}
}

func TestCodexSubscriptionClient_DefaultsModel(t *testing.T) {
	c := NewCodexSubscriptionClient("")
	if c.Model() == "" {
		t.Error("empty model should fall back to a default, got empty")
	}
}

func TestCodexSubscriptionClient_ListModels_EmptyCatalog(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4-mini")
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got != nil {
		t.Errorf("empty catalog: got %+v, want nil", got)
	}
}

func TestCodexSubscriptionClient_WithModel_ClonesAndOverrides(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4",
		WithCodexSubscriptionEffortLevel("medium"))
	clone := c.WithModel("gpt-5.4-mini")
	if clone == nil {
		t.Fatal("WithModel returned nil")
	}
	if clone.Model() != "gpt-5.4-mini" {
		t.Errorf("clone Model: got %q", clone.Model())
	}
	if c.Model() != "gpt-5.4" {
		t.Errorf("original Model mutated: got %q", c.Model())
	}
}

func TestCodexSubscriptionClient_WithModelOnNilSafe(t *testing.T) {
	var c *CodexSubscriptionClient
	clone := c.WithModel("x")
	// nil receiver returns the typed nil — caller can still ascribe
	// it to Provider without panic at the assignment.
	_ = clone
}

// -- Conversation accessor coverage ---------------------------------------

func TestConversation_CreatedAtMonotonic(t *testing.T) {
	conv := NewConversation("c1", 32)
	before := time.Now().Add(-time.Second)
	ts := conv.CreatedAt()
	if ts.Before(before) {
		t.Errorf("CreatedAt unexpectedly old: %v", ts)
	}
}

func TestConversation_LastUsedFallsBackToCreated(t *testing.T) {
	conv := NewConversation("c1", 32)
	if got := conv.LastUsed(); !got.Equal(conv.CreatedAt()) {
		t.Errorf("LastUsed without AddMessage = %v, want CreatedAt %v",
			got, conv.CreatedAt())
	}
	conv.AddMessage(Message{Role: "user", Content: "hi"})
	if got := conv.LastUsed(); got.Before(conv.CreatedAt()) {
		t.Errorf("LastUsed after AddMessage = %v, before CreatedAt %v",
			got, conv.CreatedAt())
	}
}

func TestConversation_ReplaceMessages(t *testing.T) {
	conv := NewConversation("c1", 32)
	conv.AddMessage(Message{Role: "user", Content: "a"})
	conv.AddMessage(Message{Role: "assistant", Content: "b"})
	if got := conv.Len(); got != 2 {
		t.Fatalf("setup: Len = %d, want 2", got)
	}
	conv.ReplaceMessages([]Message{
		{Role: "assistant", Content: "[summary]"},
	})
	if got := conv.Len(); got != 1 {
		t.Errorf("after Replace: Len = %d, want 1", got)
	}
	last, err := conv.LastMessage()
	if err != nil {
		t.Fatalf("LastMessage: %v", err)
	}
	if last.Content != "[summary]" {
		t.Errorf("Replace did not install summary: got %q", last.Content)
	}
}

func TestConversation_DropLast(t *testing.T) {
	conv := NewConversation("c1", 32)
	for i := 0; i < 5; i++ {
		conv.AddMessage(Message{Role: "user", Content: "msg"})
	}
	if got := conv.DropLast(0); got != 0 {
		t.Errorf("DropLast(0) = %d, want 0", got)
	}
	if got := conv.DropLast(-3); got != 0 {
		t.Errorf("DropLast(-3) = %d, want 0", got)
	}
	if got := conv.DropLast(2); got != 2 {
		t.Errorf("DropLast(2) = %d, want 2", got)
	}
	if got := conv.Len(); got != 3 {
		t.Errorf("Len after DropLast(2) = %d, want 3", got)
	}
	// over-large n clamps to current length.
	if got := conv.DropLast(99); got != 3 {
		t.Errorf("DropLast(99) over 3 msgs = %d, want 3", got)
	}
	if got := conv.Len(); got != 0 {
		t.Errorf("Len after clamp drop = %d, want 0", got)
	}
}
