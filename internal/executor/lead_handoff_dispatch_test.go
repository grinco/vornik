package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestHandleLeadHandoff_Dispatch — handleLeadHandoff routes to the
// per-outcome handler based on outcome.Outcome. Pinning each path
// here makes a regression in the switch obvious (an unhandled
// outcome silently returning an `unhandled outcome` error would
// look like a generic lead failure on the dashboard).
func TestHandleLeadHandoff_Dispatch_Checkpoint(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)

	task := &persistence.Task{ID: "t-dc", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "x-dc", TaskID: task.ID}
	out := &LeadOutcome{
		Outcome: LeadOutcomeCheckpoint,
		Checkpoint: &CheckpointPayload{
			Kind:  CheckpointKindReview,
			Draft: "draft text for review",
		},
	}

	require.NoError(t, e.handleLeadHandoff(context.Background(), task, exec, "lead", out))
	require.Len(t, msgs.inserted, 1)
	assert.Equal(t, persistence.TaskMessageKindCheckpoint, msgs.inserted[0].MessageKind)
	require.Len(t, tr.calls, 1)
	assert.Equal(t, persistence.TaskStatusAwaitingInput, tr.calls[0].to)
}

func TestHandleLeadHandoff_Dispatch_ExternalWait(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)

	deadline := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	task := &persistence.Task{ID: "t-dx", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "x-dx", TaskID: task.ID}
	out := &LeadOutcome{
		Outcome: LeadOutcomeExternalWait,
		ExternalWait: &ExternalWaitPayload{
			ExpectedBy: &deadline,
			Reason:     "vendor reply",
		},
	}
	require.NoError(t, e.handleLeadHandoff(context.Background(), task, exec, "lead", out))
	require.Len(t, tr.calls, 1)
	assert.Equal(t, persistence.TaskStatusAwaitingExternal, tr.calls[0].to)
}

func TestHandleLeadHandoff_Dispatch_ClosureRequest(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)

	task := &persistence.Task{ID: "t-dr", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "x-dr", TaskID: task.ID}
	out := &LeadOutcome{
		Outcome: LeadOutcomeClosureRequest,
		ClosureRequest: &ClosureRequestPayload{
			Summary: "done with the work",
		},
	}
	require.NoError(t, e.handleLeadHandoff(context.Background(), task, exec, "lead", out))
	require.Len(t, tr.calls, 1)
	assert.Equal(t, persistence.TaskStatusCompleted, tr.calls[0].to)
}

// TestHandleLeadHandoff_NilOutcome — the dispatcher must reject a
// nil outcome rather than nil-deref on outcome.Outcome.
func TestHandleLeadHandoff_NilOutcome(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)
	err := e.handleLeadHandoff(context.Background(), &persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, "lead", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil outcome")
}

// TestHandleLeadHandoff_UnknownOutcome — outcome=continue would
// be handled at the caller (executePlanStep keeps spawning
// children); the dispatcher in handleLeadHandoff is only invoked
// for non-continue outcomes. An unknown value here means the
// parser allowed a shape the dispatcher doesn't recognise — fail
// loud rather than silently dropping the message.
func TestHandleLeadHandoff_UnknownOutcome(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)
	out := &LeadOutcome{Outcome: LeadOutcomeKind("???")}
	err := e.handleLeadHandoff(context.Background(), &persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, "lead", out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unhandled outcome")
}

// fakeArtifactRepo returns a configured artifact list — used to
// exercise the finalizer's ingest path without standing up a real
// artifact store. The List filter (PageSize, ExecutionID) is
// captured for assertion.
type fakeArtifactRepo struct {
	mu        sync.Mutex
	artifacts []*persistence.Artifact
	listErr   error
	lastQuery persistence.ArtifactFilter
}

func (f *fakeArtifactRepo) Create(_ context.Context, a *persistence.Artifact) error { return nil }
func (f *fakeArtifactRepo) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (f *fakeArtifactRepo) List(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQuery = filter
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.artifacts, nil
}

// TestHandleLeadHandoffFinalization_NotifierFiresWithDefaultMessage —
// when result.json doesn't carry a top-level "message" field, the
// finalizer falls back to the awaiting-input default text.
func TestHandleLeadHandoffFinalization_NotifierFiresWithDefaultMessage(t *testing.T) {
	notifier := &recordingNotifier{}
	ar := &fakeArtifactRepo{}
	outcomes := newStubStepOutcomeRepo()
	e := &Executor{
		notifier:     notifier,
		artifactRepo: ar,
		outcomeRepo:  outcomes,
		logger:       zerolog.Nop(),
	}

	task := &persistence.Task{ID: "t-f1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "x-f1", TaskID: task.ID}
	// result without "message" key → finalizer uses default.
	e.handleLeadHandoffFinalization(context.Background(), task, exec, "container-1", []byte(`{"other":"field"}`))

	require.Len(t, notifier.calls, 1)
	assert.True(t, notifier.calls[0].success, "handoff notifications use success=true (the task is healthy, just waiting)")
	assert.Equal(t, "t-f1", notifier.calls[0].taskID)
	assert.Contains(t, notifier.calls[0].message, "AWAITING_INPUT")
	// sweepPendingOutcomes runs via the stub; absence of a panic is the
	// observable signal here (the stub's SweepPending is no-op when no
	// pending rows exist). The notifier was wired, so verify the call.
}

// TestHandleLeadHandoffFinalization_NotifierFiresWithResultMessage —
// when result.json carries a non-empty "message", that text is
// used as the notification body instead of the default. This is
// the path the lead uses to deliver checkpoint context to
// Telegram/email.
func TestHandleLeadHandoffFinalization_NotifierFiresWithResultMessage(t *testing.T) {
	notifier := &recordingNotifier{}
	ar := &fakeArtifactRepo{}
	outcomes := newStubStepOutcomeRepo()
	e := &Executor{
		notifier:     notifier,
		artifactRepo: ar,
		outcomeRepo:  outcomes,
		logger:       zerolog.Nop(),
	}

	task := &persistence.Task{ID: "t-f2", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "x-f2", TaskID: task.ID}
	e.handleLeadHandoffFinalization(context.Background(), task, exec, "c1", []byte(`{"message":"please review the draft"}`))

	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "please review the draft", notifier.calls[0].message)
}

// TestHandleLeadHandoffFinalization_NoNotifierWired — when the
// notifier is nil (test deployments / minimum-viable executor),
// the finalizer must still run the outcome sweep without
// crashing.
func TestHandleLeadHandoffFinalization_NoNotifier(t *testing.T) {
	ar := &fakeArtifactRepo{}
	outcomes := newStubStepOutcomeRepo()
	e := &Executor{
		artifactRepo: ar,
		outcomeRepo:  outcomes,
		logger:       zerolog.Nop(),
	}
	task := &persistence.Task{ID: "t-f3"}
	exec := &persistence.Execution{ID: "x-f3", TaskID: task.ID}
	require.NotPanics(t, func() {
		e.handleLeadHandoffFinalization(context.Background(), task, exec, "", nil)
	})
	// stubStepOutcomeRepo's SweepPending no-ops when no pending rows
	// are present — observable signal is just "no panic + no notifier
	// call". The latter is true by construction (notifier is nil).
	_ = outcomes
}

// TestHandleLeadHandoffFinalization_GarbledResult — corrupt JSON
// in the result must not skip the notifier; the default message
// is used and the notifier still fires.
func TestHandleLeadHandoffFinalization_GarbledResult(t *testing.T) {
	notifier := &recordingNotifier{}
	ar := &fakeArtifactRepo{}
	outcomes := newStubStepOutcomeRepo()
	e := &Executor{
		notifier:     notifier,
		artifactRepo: ar,
		outcomeRepo:  outcomes,
		logger:       zerolog.Nop(),
	}
	task := &persistence.Task{ID: "t-f4"}
	exec := &persistence.Execution{ID: "x-f4", TaskID: task.ID}
	e.handleLeadHandoffFinalization(context.Background(), task, exec, "", []byte("not json {{{"))
	require.Len(t, notifier.calls, 1)
	assert.Contains(t, notifier.calls[0].message, "AWAITING_INPUT",
		"garbled result.json must fall through to the default notification text")
}

// TestApplyPhaseTransitions_WritesOneMessagePerEntry — the helper
// writes one phase_marker per transition. Used by all three
// outcome handlers (checkpoint, external_wait, closure_request).
func TestApplyPhaseTransitions_WritesOneMessagePerEntry(t *testing.T) {
	msgs := &fakeMessageRepo{}
	e := &Executor{taskMessageRepo: msgs, logger: zerolog.Nop()}

	e.applyPhaseTransitions(context.Background(), "task1", "exec1", []PhaseTransition{
		{Phase: "analysis", Status: "exit"},
		{Phase: "review", Status: "enter"},
	})
	require.Len(t, msgs.inserted, 2)
	assert.Equal(t, persistence.TaskMessageKindPhaseMarker, msgs.inserted[0].MessageKind)
	assert.Contains(t, msgs.inserted[0].Content, "analysis")
	assert.Contains(t, msgs.inserted[0].Content, "exit")
}

// TestApplyPhaseTransitions_EmptyAndNilRepo — short-circuit
// branches. Empty transitions slice means "the lead didn't move
// phase"; nil taskMessageRepo means the executor was constructed
// without the conversational-lifecycle wiring (legacy deploys).
func TestApplyPhaseTransitions_EmptyAndNilRepo(t *testing.T) {
	// Empty transitions → no writes.
	msgs := &fakeMessageRepo{}
	e := &Executor{taskMessageRepo: msgs, logger: zerolog.Nop()}
	e.applyPhaseTransitions(context.Background(), "t", "x", nil)
	assert.Empty(t, msgs.inserted)

	// nil repo → must not panic.
	e2 := &Executor{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e2.applyPhaseTransitions(context.Background(), "t", "x", []PhaseTransition{{Phase: "p"}})
	})
}

// TestApplyPhaseTransitions_RepoErrorIsLoggedNotPropagated — the
// helper is best-effort; an insert failure is logged and the
// remaining transitions still attempt to write. Drives the
// log-but-continue branch.
func TestApplyPhaseTransitions_RepoErrorLogged(t *testing.T) {
	msgs := &fakeMessageRepo{insertErr: errors.New("db down")}
	e := &Executor{taskMessageRepo: msgs, logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.applyPhaseTransitions(context.Background(), "t", "x", []PhaseTransition{
			{Phase: "analysis", Status: "exit"},
			{Phase: "review", Status: "enter"},
		})
	})
}

// TestApplyScratchpadUpdate_HappyPath — merges the lead's
// partial update onto an existing row.
func TestApplyScratchpadUpdate_HappyPath(t *testing.T) {
	sp := &recordingScratchpadRepo{}
	e := &Executor{taskScratchpadRepo: sp, logger: zerolog.Nop()}

	out := &LeadOutcome{
		ScratchpadUpdate: &ScratchpadUpdate{
			Summary:       "what we know so far",
			Facts:         []byte(`{"k":"v"}`),
			OpenQuestions: []string{"q1", "q2"},
			CurrentPhase:  "review",
		},
	}
	e.applyScratchpadUpdate(context.Background(), "t-sp", "x-sp", out)
	require.NotNil(t, sp.upserted)
	assert.Equal(t, "what we know so far", sp.upserted.Summary)
	assert.Equal(t, []byte(`{"k":"v"}`), []byte(sp.upserted.Facts))
	require.NotNil(t, sp.upserted.CurrentPhase)
	assert.Equal(t, "review", *sp.upserted.CurrentPhase)
	require.NotNil(t, sp.upserted.LastExecutionID)
	assert.Equal(t, "x-sp", *sp.upserted.LastExecutionID)
}

// TestApplyScratchpadUpdate_NilUpdateOrRepo — nil ScratchpadUpdate
// (lead didn't emit one) and nil taskScratchpadRepo (legacy deploy)
// both short-circuit silently.
func TestApplyScratchpadUpdate_NilEarlyReturns(t *testing.T) {
	sp := &recordingScratchpadRepo{}
	e := &Executor{taskScratchpadRepo: sp, logger: zerolog.Nop()}

	// outcome=nil
	e.applyScratchpadUpdate(context.Background(), "t", "x", nil)
	assert.Nil(t, sp.upserted)

	// outcome with no ScratchpadUpdate
	e.applyScratchpadUpdate(context.Background(), "t", "x", &LeadOutcome{})
	assert.Nil(t, sp.upserted)

	// repo unwired
	e2 := &Executor{logger: zerolog.Nop()}
	out := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{Summary: "x"}}
	require.NotPanics(t, func() {
		e2.applyScratchpadUpdate(context.Background(), "t", "x", out)
	})
}

// recordingScratchpadRepo captures Upsert. Get returns either the
// pre-seeded row (for merge-onto-existing tests) or nil (the
// first-execution path).
type recordingScratchpadRepo struct {
	mu       sync.Mutex
	existing *persistence.TaskScratchpad
	upserted *persistence.TaskScratchpad
	getErr   error
}

func (r *recordingScratchpadRepo) Get(_ context.Context, _ string) (*persistence.TaskScratchpad, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.existing, r.getErr
}
func (r *recordingScratchpadRepo) Upsert(_ context.Context, sp *persistence.TaskScratchpad) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *sp
	r.upserted = &cp
	return nil
}

// TestApplyScratchpadUpdate_MergesOntoExisting — Summary in the
// delta replaces the existing value; an absent field leaves the
// pre-existing one intact. Pinning the merge semantics matters
// because a regression to "overwrite-all" would silently lose
// scratchpad state across executions.
func TestApplyScratchpadUpdate_MergesOntoExisting(t *testing.T) {
	prevPhase := "draft"
	sp := &recordingScratchpadRepo{
		existing: &persistence.TaskScratchpad{
			TaskID:       "t",
			Summary:      "previous",
			CurrentPhase: &prevPhase,
		},
	}
	e := &Executor{taskScratchpadRepo: sp, logger: zerolog.Nop()}

	// Delta only carries CurrentPhase; Summary must survive.
	e.applyScratchpadUpdate(context.Background(), "t", "x", &LeadOutcome{
		ScratchpadUpdate: &ScratchpadUpdate{CurrentPhase: "review"},
	})

	require.NotNil(t, sp.upserted)
	assert.Equal(t, "previous", sp.upserted.Summary, "absent field must not blank the existing value")
	require.NotNil(t, sp.upserted.CurrentPhase)
	assert.Equal(t, "review", *sp.upserted.CurrentPhase)
}

// TestApplyScratchpadUpdate_GetError_SkipsUpsert — if the repo
// fails to fetch existing state, the helper logs and bails
// without writing — avoids clobbering a row we couldn't read.
func TestApplyScratchpadUpdate_GetError(t *testing.T) {
	sp := &recordingScratchpadRepo{getErr: errors.New("db blip")}
	e := &Executor{taskScratchpadRepo: sp, logger: zerolog.Nop()}
	e.applyScratchpadUpdate(context.Background(), "t", "x", &LeadOutcome{
		ScratchpadUpdate: &ScratchpadUpdate{Summary: "x"},
	})
	assert.Nil(t, sp.upserted, "Upsert must not run when Get errored — would clobber a row we couldn't read")
}
