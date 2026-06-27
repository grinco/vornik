package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/livepubsub"
)

func TestEmitLLMCallStarted_PublishesWhenExecutionWired(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitLLMCallStarted(context.Background(), "exec_1", "gpt-test")
	if len(sub.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(sub.published))
	}
	got := sub.published[0]
	if got.executionID != "exec_1" || got.kind != livepubsub.KindLLMCallStarted {
		t.Errorf("unexpected event: %+v", got)
	}
	payload, ok := got.payload.(livepubsub.LLMCallStartedPayload)
	if !ok || payload.Model != "gpt-test" {
		t.Errorf("payload wrong: %+v", got.payload)
	}
}

func TestEmitLLMCallStarted_NoOpOnEmptyExecutionID(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitLLMCallStarted(context.Background(), "", "gpt-test")
	if len(sub.published) != 0 {
		t.Errorf("empty execution should not publish; got %d", len(sub.published))
	}
}

func TestEmitLLMCallStarted_NoOpWhenLiveSubUnwired(t *testing.T) {
	srv := &Server{}
	// Should not panic; should also not error since publish is
	// best-effort.
	srv.emitLLMCallStarted(context.Background(), "exec_1", "gpt-test")
}

func TestEmitLLMCallFinished_RoundTripsTokens(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	resp := &chat.ChatResponse{Model: "gpt-test"}
	resp.Usage.PromptTokens = 250
	resp.Usage.CompletionTokens = 80
	srv.emitLLMCallFinished(context.Background(), "exec_1", "gpt-test", resp, 1500*time.Millisecond)
	if len(sub.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(sub.published))
	}
	got := sub.published[0]
	payload, ok := got.payload.(livepubsub.LLMCallFinishedPayload)
	if !ok {
		t.Fatalf("wrong payload type: %T", got.payload)
	}
	if payload.PromptTokens != 250 || payload.CompletionTokens != 80 {
		t.Errorf("token counts wrong: %+v", payload)
	}
	if payload.DurationMs != 1500 {
		t.Errorf("duration wrong: %d", payload.DurationMs)
	}
	if payload.Err != "" {
		t.Errorf("happy path should have empty err: %q", payload.Err)
	}
}

// TestEmitLLMCallFinished_CostUSDPopulatedWhenPricingLoaded —
// regression sentinel for the 2026-05-26 fix: the live header
// cost counter was stuck at $0.00 because the payload didn't
// carry cost_usd. Pre-fix the JS had no input to accumulate. Now
// the chat-proxy emit calls computeChatCallCost and the payload
// carries a non-zero CostUSD whenever pricing is loaded for the
// model. Asserts both that the field flows through AND that
// pricing-absent deployments still publish (just with CostUSD=0).
func TestEmitLLMCallFinished_CostUSDPopulatedWhenPricingLoaded(t *testing.T) {
	sub := &stubLiveSub{}
	// No pricing path → computeChatCallCost returns 0; payload
	// still publishes (the operator just sees $0.00 for that call,
	// not a missing event).
	srv := &Server{liveSub: sub}
	resp := &chat.ChatResponse{Model: "gpt-test"}
	resp.Usage.PromptTokens = 1000
	resp.Usage.CompletionTokens = 500
	srv.emitLLMCallFinished(context.Background(), "exec_1", "gpt-test", resp, 50*time.Millisecond)
	if len(sub.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(sub.published))
	}
	payload := sub.published[0].payload.(livepubsub.LLMCallFinishedPayload)
	// No pricing wired → CostUSD == 0, but the field must be
	// present on the payload type (the JSON tag is omitempty so
	// it stays off the wire, but the field must exist).
	if payload.CostUSD != 0 {
		t.Errorf("no-pricing path: CostUSD should be 0, got %f", payload.CostUSD)
	}
}

func TestEmitLLMCallFinished_ModelFallsBackToResponse(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	resp := &chat.ChatResponse{Model: "bedrock.anthropic.claude-sonnet-4-5"}
	// Caller passes empty model (legacy / no-override path) →
	// payload picks up the response's model so the UI still
	// shows what actually ran.
	srv.emitLLMCallFinished(context.Background(), "exec_1", "", resp, 100*time.Millisecond)
	payload := sub.published[0].payload.(livepubsub.LLMCallFinishedPayload)
	if payload.Model != "bedrock.anthropic.claude-sonnet-4-5" {
		t.Errorf("expected response model fallback, got %q", payload.Model)
	}
}

func TestEmitLLMCallFinishedErr_CarriesErrorString(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitLLMCallFinishedErr(context.Background(), "exec_1", "gpt-test",
		errors.New("upstream: rate-limited"), 200*time.Millisecond)
	payload := sub.published[0].payload.(livepubsub.LLMCallFinishedPayload)
	if payload.Err != "upstream: rate-limited" {
		t.Errorf("err string wrong: %q", payload.Err)
	}
	if payload.PromptTokens != 0 || payload.CompletionTokens != 0 {
		t.Errorf("error path should have zero tokens, got %+v", payload)
	}
}

func TestEmitLLMCallFinished_NoOpOnEmptyExecutionID(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitLLMCallFinished(context.Background(), "", "gpt-test", nil, 0)
	srv.emitLLMCallFinishedErr(context.Background(), "", "gpt-test", errors.New("x"), 0)
	if len(sub.published) != 0 {
		t.Errorf("empty execution should not publish; got %d", len(sub.published))
	}
}

func TestEmitPaused_RoundTripsKindAndBy(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitPaused(context.Background(), "exec_1", "operator", "api_key:abc")
	if len(sub.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(sub.published))
	}
	got := sub.published[0]
	if got.executionID != "exec_1" || got.kind != livepubsub.KindPaused {
		t.Errorf("unexpected event: %+v", got)
	}
	payload, ok := got.payload.(livepubsub.PausedPayload)
	if !ok {
		t.Fatalf("payload type wrong: %T", got.payload)
	}
	if payload.PauseKind != "operator" || payload.By != "api_key:abc" {
		t.Errorf("payload fields wrong: %+v", payload)
	}
}

func TestEmitResumed_CarriesActor(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitResumed(context.Background(), "exec_1", "api_key:abc")
	payload, ok := sub.published[0].payload.(livepubsub.ResumedPayload)
	if !ok {
		t.Fatalf("payload type wrong: %T", sub.published[0].payload)
	}
	if payload.By != "api_key:abc" {
		t.Errorf("by wrong: %q", payload.By)
	}
}

func TestEmitPaused_NoOpOnEmptyExecutionID(t *testing.T) {
	sub := &stubLiveSub{}
	srv := &Server{liveSub: sub}
	srv.emitPaused(context.Background(), "", "operator", "")
	srv.emitResumed(context.Background(), "", "")
	if len(sub.published) != 0 {
		t.Errorf("empty execution should not publish; got %d", len(sub.published))
	}
}

func TestEmitPaused_NoOpWhenUnwired(t *testing.T) {
	srv := &Server{}
	srv.emitPaused(context.Background(), "exec_1", "operator", "")
	srv.emitResumed(context.Background(), "exec_1", "")
	// Should not panic.
}
