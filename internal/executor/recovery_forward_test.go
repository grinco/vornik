package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// TestForwardPendingRecovery_NoPending: with no stashed recovery context
// the helper is a no-op and reports it did nothing, so the normal
// (non-recovery) step path is untouched.
func TestForwardPendingRecovery_NoPending(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	opts := &agentInputOpts{}

	if got := e.forwardPendingRecovery(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "e1"}, &executionState{}, opts); got {
		t.Fatalf("forwardPendingRecovery with no PendingRecovery = true, want false")
	}
	if opts.RecoveryContext != nil {
		t.Fatalf("opts.RecoveryContext should stay nil, got %+v", opts.RecoveryContext)
	}
	// nil state must also be safe.
	if got := e.forwardPendingRecovery(context.Background(), nil, nil, nil, opts); got {
		t.Fatalf("forwardPendingRecovery with nil state = true, want false")
	}
}

// TestForwardPendingRecovery_AgentRecoverGetsLearnedOverlay is the
// regression test for the dev-pipeline gap: the learned-remediation
// overlay (Consumer A) was wired only in the plan-step recover path
// (plan_step.go), so an AGENT-type recover step — e.g. dev-pipeline's
// `recover-checkpoint` (type:agent, role:analyst) — forwarded the base
// RecoveryContext but never got the overlay. This asserts the agent path
// now forwards AND attaches the overlay, reaching the same instinct
// surface as the plan-step path.
func TestForwardPendingRecovery_AgentRecoverGetsLearnedOverlay(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		// Keyed on the FAILED step's recorded class+role (a coder
		// `implement` step that hit a tool error), not the recover
		// step's role — matches attachLearnedRemediations' lookup.
		activeRecoveryInstinct(t, "i1", "coder", "TOOL_ERROR", "swapping the build tool resolved it", 0.9),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true, // failure_playbooks gate ON
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "TOOL_ERROR", Role: "coder"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "implement", FailureClass: "tool_error"}
	state := &executionState{PendingRecovery: rc}
	opts := &agentInputOpts{}

	got := e.forwardPendingRecovery(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "e1"}, state, opts)

	if !got {
		t.Fatalf("forwardPendingRecovery = false, want true (it forwarded a pending recovery)")
	}
	if opts.RecoveryContext != rc {
		t.Fatalf("opts.RecoveryContext not set to the pending context")
	}
	if state.PendingRecovery != nil {
		t.Fatalf("state.PendingRecovery must be cleared after forwarding, got %+v", state.PendingRecovery)
	}
	if len(opts.RecoveryContext.LearnedRemediations) != 1 {
		t.Fatalf("agent recover step got %d learned remediations, want 1 (overlay must reach agent steps)",
			len(opts.RecoveryContext.LearnedRemediations))
	}
	if len(repo.applications) != 1 {
		t.Fatalf("expected 1 lead_recovery application row recorded, got %d", len(repo.applications))
	}
}

// TestForwardPendingRecovery_GateOffForwardsWithoutOverlay: with the
// failure_playbooks gate off, the helper still forwards the base context
// (recovery itself is independent of the instinct layer) but attaches no
// overlay — byte-for-byte parity with the pre-instinct behaviour.
func TestForwardPendingRecovery_GateOffForwardsWithoutOverlay(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "coder", "TOOL_ERROR", "swapping the build tool resolved it", 0.9),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: false, // gate OFF
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "TOOL_ERROR", Role: "coder"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "implement", FailureClass: "tool_error"}
	state := &executionState{PendingRecovery: rc}
	opts := &agentInputOpts{}

	if got := e.forwardPendingRecovery(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "e1"}, state, opts); !got {
		t.Fatalf("forwardPendingRecovery = false, want true")
	}
	if opts.RecoveryContext != rc {
		t.Fatalf("base recovery context must still forward with gate off")
	}
	if opts.RecoveryContext.LearnedRemediations != nil {
		t.Fatalf("gate off: no overlay expected, got %+v", opts.RecoveryContext.LearnedRemediations)
	}
	if len(repo.applications) != 0 {
		t.Fatalf("gate off: no application rows, got %d", len(repo.applications))
	}
}
