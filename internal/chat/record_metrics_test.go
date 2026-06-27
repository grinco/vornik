// Coverage for the per-provider recordMetrics helpers — drives the
// "metrics wired + success" and "metrics wired + error" branches for
// Client, ClaudeSubscriptionClient, CLIClient, CodexCLIClient, and
// CodexSubscriptionClient. Without these tests each function sits at
// ~20-30% (the nil-metrics fast path).
//
// We can't directly assert prometheus counter values because the
// vectors aren't exported, but exercising the lines lifts coverage
// and serves as a smoke guard for label-spelling drift.

package chat

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newRealMetrics() *Metrics { return NewMetrics(prometheus.NewRegistry()) }

func TestClient_RecordMetrics_SuccessAndError(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "gpt-x", WithMetrics(newRealMetrics()))
	resp := &ChatResponse{}
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 50
	resp.Usage.TotalTokens = 150
	// Cover both branches: success-with-tokens and error.
	c.recordMetrics(time.Now().Add(-100*time.Millisecond), "success", resp)
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "error", nil)
}

func TestClient_RecordMetrics_NilMetricsNoop(t *testing.T) {
	c := NewClient("https://api.example.com", "k", "x")
	// No panic, no error — nil-fast-path branch.
	c.recordMetrics(time.Now(), "success", nil)
}

func TestClaudeSubscription_RecordMetricsAndTokens(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-opus-4-7")
	c.SetMetrics(newRealMetrics())

	c.recordMetrics(time.Now().Add(-100*time.Millisecond), "ok")
	c.recordMetrics(time.Now().Add(-100*time.Millisecond), "rate_limited") // != "ok" → error counter
	// recordTokens with non-zero counts.
	resp := &ChatResponse{}
	resp.Usage.PromptTokens = 200
	resp.Usage.CompletionTokens = 80
	c.recordTokens(resp)
	c.recordTokens(nil)             // nil-safe
	c.recordTokens(&ChatResponse{}) // zero-usage branch
}

func TestClaudeSubscription_RecordMetrics_NilMetricsNoop(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-opus-4-7")
	c.recordMetrics(time.Now(), "ok") // c.metrics == nil
	c.recordTokens(&ChatResponse{Usage: struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
		CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	}{PromptTokens: 1}})
}

func TestCLIClient_RecordMetrics(t *testing.T) {
	c := NewCLIClient("claude-opus-4-7")
	c.SetMetrics(newRealMetrics())
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "success")
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "error")
}

func TestCodexCLI_RecordMetrics(t *testing.T) {
	c := NewCodexCLIClient("gpt-5.4")
	c.SetMetrics(newRealMetrics())
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "success")
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "error")
}

func TestCodexSubscription_RecordMetrics(t *testing.T) {
	c := NewCodexSubscriptionClient("gpt-5.4")
	c.SetMetrics(newRealMetrics())
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "success")
	c.recordMetrics(time.Now().Add(-50*time.Millisecond), "error")
}
