package api

import (
	"context"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/livepubsub"
)

// emitLLMCallStarted publishes llm_call_started for the given
// execution. No-op when the live publisher isn't wired or the
// chat call has no execution context (external clients hitting
// the proxy without an agent header). Feature #3 Phase B
// follow-up — coarse-grained "agent is thinking" signal.
func (s *Server) emitLLMCallStarted(ctx context.Context, executionID, model string) {
	if s == nil || s.liveSub == nil {
		return
	}
	if executionID == "" {
		return
	}
	s.liveSub.Publish(ctx, executionID, livepubsub.KindLLMCallStarted,
		livepubsub.LLMCallStartedPayload{Model: model})
}

// emitLLMCallFinished publishes llm_call_finished on the happy
// path, carrying token counts AND the per-call USD cost from the
// upstream response so the UI's running spend counter updates
// live (2026-05-26 fix — cost was emitted as 0 on step_completed
// and never reconciled into the live header, leaving the operator
// staring at $0.00 while the task page showed $0.348).
//
// Cost is computed via s.computeChatCallCost using the same
// pricing table the chat-audit / usage-recording paths use, so the
// live header tracks the canonical number the spend dashboard will
// later show. Zero when no pricing entry exists for the model.
func (s *Server) emitLLMCallFinished(ctx context.Context, executionID, model string, resp *chat.ChatResponse, dur time.Duration) {
	if s == nil || s.liveSub == nil {
		return
	}
	if executionID == "" {
		return
	}
	payload := livepubsub.LLMCallFinishedPayload{
		Model:      model,
		DurationMs: dur.Milliseconds(),
	}
	if resp != nil {
		payload.PromptTokens = resp.Usage.PromptTokens
		payload.CompletionTokens = resp.Usage.CompletionTokens
		if payload.Model == "" {
			payload.Model = resp.Model
		}
		payload.CostUSD = s.computeChatCallCost(model, resp)
	}
	s.liveSub.Publish(ctx, executionID, livepubsub.KindLLMCallFinished, payload)
}

// emitLLMCallFinishedErr publishes llm_call_finished on the
// failure path with the upstream error string. Token fields stay
// zero — the call didn't complete.
func (s *Server) emitLLMCallFinishedErr(ctx context.Context, executionID, model string, err error, dur time.Duration) {
	if s == nil || s.liveSub == nil {
		return
	}
	if executionID == "" {
		return
	}
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	s.liveSub.Publish(ctx, executionID, livepubsub.KindLLMCallFinished,
		livepubsub.LLMCallFinishedPayload{
			Model:      model,
			DurationMs: dur.Milliseconds(),
			Err:        errStr,
		})
}

// emitPaused publishes a `paused` event for the live observation
// surface (Feature #3 Phase C-2). pauseKind matches the
// execution.pause_kind taxonomy ("operator", "shutdown",
// "awaiting_children"); the UI uses it to render an appropriate
// badge ("paused by operator" vs "shutting down" vs "waiting on
// children"). `by` carries the operator identifier when known.
func (s *Server) emitPaused(ctx context.Context, executionID, pauseKind, by string) {
	if s == nil || s.liveSub == nil {
		return
	}
	if executionID == "" {
		return
	}
	s.liveSub.Publish(ctx, executionID, livepubsub.KindPaused,
		livepubsub.PausedPayload{
			PauseKind: pauseKind,
			By:        by,
		})
}

// emitResumed publishes a `resumed` event when an operator (or
// the scheduler's awaiting-children unblock) clears the pause.
// The UI flips Pause→Resume back to Pause + the "paused" badge
// clears.
func (s *Server) emitResumed(ctx context.Context, executionID, by string) {
	if s == nil || s.liveSub == nil {
		return
	}
	if executionID == "" {
		return
	}
	s.liveSub.Publish(ctx, executionID, livepubsub.KindResumed,
		livepubsub.ResumedPayload{By: by})
}
