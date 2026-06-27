package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/executor/livepubsub"
)

// TestEmitPaused_PublishesInputRequiredSignal is the A2A-steering-callback
// regression: the AWAITING_INPUT transition emits a KindPaused live event,
// which the A2A SSE bridge (internal/conversation/a2a/sse.go statusFromKind)
// maps to the "input-required" task state — the only way an A2A caller, which
// can't receive a chat/DM steering push, learns the task is waiting on a
// human. Before this, KindPaused had no producer and the SSE mapping was dead.
func TestEmitPaused_PublishesInputRequiredSignal(t *testing.T) {
	stub := &stubLivePub{}
	e := &Executor{livePub: stub}

	e.emitPaused(context.Background(), "exec-1", "awaiting_input")

	paused := stub.byKind(livepubsub.KindPaused)
	if len(paused) != 1 {
		t.Fatalf("want 1 KindPaused event, got %d", len(paused))
	}
	if paused[0].ExecutionID != "exec-1" {
		t.Errorf("execution id = %q, want exec-1", paused[0].ExecutionID)
	}
	pp, ok := paused[0].Payload.(livepubsub.PausedPayload)
	if !ok {
		t.Fatalf("payload type = %T, want livepubsub.PausedPayload", paused[0].Payload)
	}
	if pp.PauseKind != "awaiting_input" {
		t.Errorf("pause_kind = %q, want awaiting_input", pp.PauseKind)
	}
}

// TestEmitPaused_NilPublisherSafe — no publisher wired is a no-op, not a panic.
func TestEmitPaused_NilPublisherSafe(t *testing.T) {
	e := &Executor{}
	e.emitPaused(context.Background(), "exec-1", "awaiting_input") // must not panic
}
