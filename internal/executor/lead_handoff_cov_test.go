package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// nopLoggerCov is a silent logger for the handoff coverage tests that
// construct a bare Executor (the shared newHandoffExecutor already wires one,
// but the scratchpad/phase tests build minimal executors directly).
func nopLoggerCov() zerolog.Logger { return zerolog.Nop() }

// TestLeadHandoffCov_HandleLeadHandoff_NilAndUnknown — the dispatch guard
// rejects a nil outcome and an unhandled outcome kind.
func TestLeadHandoffCov_HandleLeadHandoff_NilAndUnknown(t *testing.T) {
	e := newHandoffExecutor(&fakeMessageRepo{}, newFakeTaskRepo())
	task := &persistence.Task{ID: "t", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "x", TaskID: "t"}

	require.Error(t, e.handleLeadHandoff(context.Background(), task, exec, "lead", nil))

	err := e.handleLeadHandoff(context.Background(), task, exec, "lead",
		&LeadOutcome{Outcome: LeadOutcomeKind("bogus")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unhandled outcome")
}

// TestLeadHandoffCov_Checkpoint_SerializeError — a nil checkpoint payload
// makes SerializeCheckpointMetadata fail before any dereference.
func TestLeadHandoffCov_Checkpoint_SerializeError(t *testing.T) {
	e := newHandoffExecutor(&fakeMessageRepo{}, newFakeTaskRepo())
	task := &persistence.Task{ID: "t", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "x", TaskID: "t"}
	err := e.handleCheckpointOutcome(context.Background(), task, exec, "lead",
		&LeadOutcome{Outcome: LeadOutcomeCheckpoint, Checkpoint: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "serialize checkpoint")
}

// TestLeadHandoffCov_Checkpoint_BodyFallbacks — when Message is empty the
// body falls back to Question, then TaskForHuman, then Draft. Also exercises
// the plan-phase CurrentPhase wiring on the transition opts.
func TestLeadHandoffCov_Checkpoint_BodyFallbacks(t *testing.T) {
	t.Run("falls back to question", func(t *testing.T) {
		msgs := &fakeMessageRepo{}
		e := newHandoffExecutor(msgs, newFakeTaskRepo())
		task := &persistence.Task{ID: "t", Status: persistence.TaskStatusRunning}
		exec := &persistence.Execution{ID: "x", TaskID: "t"}
		out := &LeadOutcome{
			Outcome: LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{
				Kind: CheckpointKindDecision, Question: "which way?",
				Options: []CheckpointOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
			},
			Plan: &PlanShape{Phase: "phase-9"},
		}
		require.NoError(t, e.handleCheckpointOutcome(context.Background(), task, exec, "lead", out))
		require.Len(t, msgs.inserted, 1)
		assert.Equal(t, "which way?", msgs.inserted[0].Content)
	})

	t.Run("falls back to task_for_human then draft", func(t *testing.T) {
		// TaskForHuman path.
		msgs := &fakeMessageRepo{}
		e := newHandoffExecutor(msgs, newFakeTaskRepo())
		task := &persistence.Task{ID: "t", Status: persistence.TaskStatusRunning}
		exec := &persistence.Execution{ID: "x", TaskID: "t"}
		out := &LeadOutcome{
			Outcome:    LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindActionRequired, TaskForHuman: "go sign in"},
		}
		require.NoError(t, e.handleCheckpointOutcome(context.Background(), task, exec, "lead", out))
		assert.Equal(t, "go sign in", msgs.inserted[0].Content)

		// Draft path.
		msgs2 := &fakeMessageRepo{}
		e2 := newHandoffExecutor(msgs2, newFakeTaskRepo())
		out2 := &LeadOutcome{
			Outcome:    LeadOutcomeCheckpoint,
			Checkpoint: &CheckpointPayload{Kind: CheckpointKindReview, Draft: "the draft text"},
		}
		require.NoError(t, e2.handleCheckpointOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out2))
		assert.Equal(t, "the draft text", msgs2.inserted[0].Content)
	})
}

// TestLeadHandoffCov_Checkpoint_InsertError — the message insert fails; the
// handler surfaces the error and does NOT attempt a transition.
func TestLeadHandoffCov_Checkpoint_InsertError(t *testing.T) {
	msgs := &fakeMessageRepo{insertErr: errors.New("db down")}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)
	out := &LeadOutcome{
		Outcome: LeadOutcomeCheckpoint,
		Checkpoint: &CheckpointPayload{
			Kind: CheckpointKindDecision, Question: "?",
			Options: []CheckpointOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
		},
	}
	err := e.handleCheckpointOutcome(context.Background(),
		&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
		&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "insert checkpoint")
	assert.Empty(t, tr.calls, "no transition attempted when the message insert failed")
}

// TestLeadHandoffCov_Checkpoint_TransitionDrift — TransitionConditional
// returns (false, nil): the task drifted out of RUNNING. The handler logs
// and proceeds without error, and the in-memory status is NOT mutated.
func TestLeadHandoffCov_Checkpoint_TransitionDrift(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	tr.transitionOK = false // drift
	e := newHandoffExecutor(msgs, tr)
	task := &persistence.Task{ID: "t", Status: persistence.TaskStatusRunning}
	out := &LeadOutcome{
		Outcome: LeadOutcomeCheckpoint,
		Checkpoint: &CheckpointPayload{
			Kind: CheckpointKindDecision, Question: "?",
			Options: []CheckpointOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
		},
	}
	require.NoError(t, e.handleCheckpointOutcome(context.Background(), task,
		&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out))
	assert.Equal(t, persistence.TaskStatusRunning, task.Status,
		"drift means the status is not advanced in memory")
}

// TestLeadHandoffCov_Checkpoint_TransitionError — TransitionConditional
// returns a hard error; the handler propagates it.
func TestLeadHandoffCov_Checkpoint_TransitionError(t *testing.T) {
	tr := newFakeTaskRepo()
	tr.transitionErr = errors.New("conflict")
	e := newHandoffExecutor(&fakeMessageRepo{}, tr)
	out := &LeadOutcome{
		Outcome: LeadOutcomeCheckpoint,
		Checkpoint: &CheckpointPayload{
			Kind: CheckpointKindDecision, Question: "?",
			Options: []CheckpointOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
		},
	}
	err := e.handleCheckpointOutcome(context.Background(),
		&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
		&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AWAITING_INPUT")
}

// TestLeadHandoffCov_ExternalWait_BodyFallbackAndInsertError covers the
// external_wait body fallbacks and the insert-error path.
func TestLeadHandoffCov_ExternalWait_BodyFallbackAndInsertError(t *testing.T) {
	deadline := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	t.Run("default body when no message or reason", func(t *testing.T) {
		msgs := &fakeMessageRepo{}
		e := newHandoffExecutor(msgs, newFakeTaskRepo())
		out := &LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			ExternalWait: &ExternalWaitPayload{ExpectedBy: &deadline},
		}
		require.NoError(t, e.handleExternalWaitOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out))
		assert.Equal(t, "waiting on external event", msgs.inserted[0].Content)
	})

	t.Run("reason overrides default", func(t *testing.T) {
		msgs := &fakeMessageRepo{}
		e := newHandoffExecutor(msgs, newFakeTaskRepo())
		out := &LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			Message:      "msg",
			ExternalWait: &ExternalWaitPayload{ExpectedBy: &deadline, Reason: "vendor reply"},
			Plan:         &PlanShape{Phase: "waiting"},
		}
		require.NoError(t, e.handleExternalWaitOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out))
		assert.Equal(t, "vendor reply", msgs.inserted[0].Content)
	})

	t.Run("insert error propagates", func(t *testing.T) {
		msgs := &fakeMessageRepo{insertErr: errors.New("db down")}
		e := newHandoffExecutor(msgs, newFakeTaskRepo())
		out := &LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			ExternalWait: &ExternalWaitPayload{ExpectedBy: &deadline},
		}
		err := e.handleExternalWaitOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "insert external_wait note")
	})

	t.Run("transition drift logs and proceeds", func(t *testing.T) {
		tr := newFakeTaskRepo()
		tr.transitionOK = false
		e := newHandoffExecutor(&fakeMessageRepo{}, tr)
		out := &LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			ExternalWait: &ExternalWaitPayload{ExpectedBy: &deadline},
		}
		require.NoError(t, e.handleExternalWaitOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out))
	})

	t.Run("transition error propagates", func(t *testing.T) {
		tr := newFakeTaskRepo()
		tr.transitionErr = errors.New("conflict")
		e := newHandoffExecutor(&fakeMessageRepo{}, tr)
		out := &LeadOutcome{
			Outcome:      LeadOutcomeExternalWait,
			ExternalWait: &ExternalWaitPayload{ExpectedBy: &deadline},
		}
		err := e.handleExternalWaitOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "AWAITING_EXTERNAL")
	})
}

// TestLeadHandoffCov_ClosureRequest_InsertError + transition error/drift.
func TestLeadHandoffCov_ClosureRequest_ErrorPaths(t *testing.T) {
	t.Run("insert error propagates", func(t *testing.T) {
		msgs := &fakeMessageRepo{insertErr: errors.New("db down")}
		e := newHandoffExecutor(msgs, newFakeTaskRepo())
		out := &LeadOutcome{Outcome: LeadOutcomeClosureRequest, ClosureRequest: &ClosureRequestPayload{Summary: "done"}}
		err := e.handleClosureRequestOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "insert closure_request")
	})

	t.Run("transition error propagates", func(t *testing.T) {
		tr := newFakeTaskRepo()
		tr.transitionErr = errors.New("conflict")
		e := newHandoffExecutor(&fakeMessageRepo{}, tr)
		out := &LeadOutcome{Outcome: LeadOutcomeClosureRequest, Message: "all set", ClosureRequest: &ClosureRequestPayload{Summary: "done"}}
		err := e.handleClosureRequestOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "COMPLETED for closure_request")
	})

	t.Run("body composes message and summary", func(t *testing.T) {
		msgs := &fakeMessageRepo{}
		tr := newFakeTaskRepo()
		e := newHandoffExecutor(msgs, tr)
		out := &LeadOutcome{Outcome: LeadOutcomeClosureRequest, Message: "wrap-up", ClosureRequest: &ClosureRequestPayload{Summary: "verified"}}
		require.NoError(t, e.handleClosureRequestOutcome(context.Background(),
			&persistence.Task{ID: "t", Status: persistence.TaskStatusRunning},
			&persistence.Execution{ID: "x", TaskID: "t"}, "lead", out))
		assert.Contains(t, msgs.inserted[0].Content, "wrap-up")
		assert.Contains(t, msgs.inserted[0].Content, "verified")
	})
}

// TestLeadHandoffCov_ApplyScratchpadUpdate covers the merge branches:
// every optional field present, plus the nil-repo and nil-update guards.
func TestLeadHandoffCov_ApplyScratchpadUpdate(t *testing.T) {
	t.Run("nil update is a no-op", func(t *testing.T) {
		sp := &fakeScratchpadRepo{}
		e := &Executor{taskScratchpadRepo: sp, logger: nopLoggerCov()}
		e.applyScratchpadUpdate(context.Background(), "t", "x", &LeadOutcome{})
		assert.Nil(t, sp.upserted)
	})

	t.Run("nil repo is a no-op", func(t *testing.T) {
		e := &Executor{logger: nopLoggerCov()}
		e.applyScratchpadUpdate(context.Background(), "t", "x",
			&LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{Summary: "s"}})
		// No panic, nothing to assert beyond surviving the call.
	})

	t.Run("merges every field onto a new row", func(t *testing.T) {
		sp := &fakeScratchpadRepo{}
		e := &Executor{taskScratchpadRepo: sp, logger: nopLoggerCov()}
		out := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{
			Summary:       "the summary",
			Facts:         []byte(`{"k":"v"}`),
			OpenQuestions: []string{"q1", "q2"},
			CurrentPhase:  "phase-3",
		}}
		e.applyScratchpadUpdate(context.Background(), "task-1", "exec-1", out)
		require.NotNil(t, sp.upserted)
		assert.Equal(t, "the summary", sp.upserted.Summary)
		require.NotNil(t, sp.upserted.CurrentPhase)
		assert.Equal(t, "phase-3", *sp.upserted.CurrentPhase)
		assert.NotEmpty(t, sp.upserted.Facts)
		assert.NotEmpty(t, sp.upserted.OpenQuestions)
		require.NotNil(t, sp.upserted.LastExecutionID)
		assert.Equal(t, "exec-1", *sp.upserted.LastExecutionID)
	})
}

// TestLeadHandoffCov_ApplyPhaseTransitions covers the nil-repo guard and a
// happy multi-transition write.
func TestLeadHandoffCov_ApplyPhaseTransitions(t *testing.T) {
	t.Run("nil repo is a no-op", func(t *testing.T) {
		e := &Executor{logger: nopLoggerCov()}
		e.applyPhaseTransitions(context.Background(), "t", "x",
			[]PhaseTransition{{Phase: "p", Status: "done"}})
	})
	t.Run("writes one phase_marker per transition", func(t *testing.T) {
		msgs := &fakeMessageRepo{}
		e := &Executor{taskMessageRepo: msgs, logger: nopLoggerCov()}
		e.applyPhaseTransitions(context.Background(), "task-1", "exec-1", []PhaseTransition{
			{Phase: "research", Status: "complete"},
			{Phase: "build", Status: "started"},
		})
		require.Len(t, msgs.inserted, 2)
		assert.Equal(t, persistence.TaskMessageKindPhaseMarker, msgs.inserted[0].MessageKind)
		assert.Contains(t, msgs.inserted[0].Content, "research")
	})
}
