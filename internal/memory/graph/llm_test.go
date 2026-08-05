package graph

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// ctxCapturingProvider records the context each Complete call receives so
// tests can assert on the per-request options the stage set.
type ctxCapturingProvider struct {
	gotCtx  context.Context
	content string
}

func (c *ctxCapturingProvider) Complete(ctx context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	c.gotCtx = ctx
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: c.content}, FinishReason: "stop"})
	return resp, nil
}

func (c *ctxCapturingProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}

func (c *ctxCapturingProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (c *ctxCapturingProvider) Model() string            { return "fake" }
func (c *ctxCapturingProvider) SetMetrics(*chat.Metrics) {}

// TestCompleteWithRetry_CapsReasoningEffort pins the remedy chosen on
// 2026-08-05 for the extractor's 83.3% empty-completion rate.
//
// gpt-oss-120b is a reasoning model and spent the whole token allowance
// thinking rather than answering. Raising the ceiling was tried first and made
// each failure twice as expensive without making it less likely — the very
// next live call consumed all 16384 and still returned nothing. Capping the
// REASONING is the lever that matches the cause, so every graph stage must
// send it; a stage that quietly stops doing so reopens the bug.
func TestCompleteWithRetry_CapsReasoningEffort(t *testing.T) {
	p := &ctxCapturingProvider{content: "[]"}
	if _, err := completeWithRetry(context.Background(), p, []chat.Message{{Role: "user", Content: "x"}}, 1); err != nil {
		t.Fatalf("completeWithRetry: %v", err)
	}
	if got := chat.ReasoningEffortFromContext(p.gotCtx); got != chat.ReasoningEffortLow {
		t.Errorf("reasoning effort = %q, want %q — every graph stage must cap it",
			got, chat.ReasoningEffortLow)
	}
}

// TestCompleteWithRetry_KeepsTheTokenBackstop — the ceiling stays as the
// runaway backstop it was always meant to be. It went 8192 → 16384 → 8192:
// with reasoning capped the extra headroom bought nothing but a more
// expensive failure, and a bound that never binds is not a bound.
func TestCompleteWithRetry_KeepsTheTokenBackstop(t *testing.T) {
	p := &ctxCapturingProvider{content: "[]"}
	if _, err := completeWithRetry(context.Background(), p, []chat.Message{{Role: "user", Content: "x"}}, 1); err != nil {
		t.Fatalf("completeWithRetry: %v", err)
	}
	if got := chat.MaxTokensFromContext(p.gotCtx); got != graphRequestMaxTokens {
		t.Errorf("max tokens = %d, want %d", got, graphRequestMaxTokens)
	}
	if graphRequestMaxTokens != 8192 {
		t.Errorf("backstop = %d; raising it is not the fix for truncation — see completeWithRetry",
			graphRequestMaxTokens)
	}
}

// TestCompleteWithRetry_LabelsTheCallSite — without it these calls log
// call_site="unknown" and vanish from cost attribution.
func TestCompleteWithRetry_LabelsTheCallSite(t *testing.T) {
	p := &ctxCapturingProvider{content: "[]"}
	if _, err := completeWithRetry(context.Background(), p, []chat.Message{{Role: "user", Content: "x"}}, 1); err != nil {
		t.Fatalf("completeWithRetry: %v", err)
	}
	if got := chat.CallSiteFromContext(p.gotCtx); got != "memory.graph" {
		t.Errorf("call site = %q, want memory.graph", got)
	}
}
