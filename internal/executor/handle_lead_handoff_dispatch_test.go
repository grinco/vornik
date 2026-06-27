package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestHandleLeadHandoff_NilOutcomeIsError pins the nil-outcome guard.
func TestHandleLeadHandoff_NilOutcomeIsError(t *testing.T) {
	e := newHandoffExecutor(&fakeMessageRepo{}, newFakeTaskRepo())
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	err := e.handleLeadHandoff(context.Background(), task, exec, "lead-step", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil outcome")
}

// TestHandleLeadHandoff_DispatchesCheckpoint — happy path via the
// checkpoint arm; downstream handleCheckpointOutcome writes the
// task_message and runs the transition.
func TestHandleLeadHandoff_DispatchesCheckpoint(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec1", TaskID: task.ID}
	outcome := &LeadOutcome{
		Outcome:    LeadOutcomeCheckpoint,
		Checkpoint: &CheckpointPayload{Kind: CheckpointKindActionRequired, TaskForHuman: "do it"},
	}
	require.NoError(t, e.handleLeadHandoff(context.Background(), task, exec, "lead-step", outcome))
	require.Len(t, msgs.inserted, 1)
	assert.Equal(t, persistence.TaskMessageKindCheckpoint, msgs.inserted[0].MessageKind)
}

// TestHandleLeadHandoff_DispatchesExternalWait — routes to the
// external-wait handler with the deadline propagated.
func TestHandleLeadHandoff_DispatchesExternalWait(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)
	deadline := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec1", TaskID: task.ID}
	outcome := &LeadOutcome{
		Outcome:      LeadOutcomeExternalWait,
		ExternalWait: &ExternalWaitPayload{ExpectedBy: &deadline, Reason: "vendor reply"},
	}
	require.NoError(t, e.handleLeadHandoff(context.Background(), task, exec, "lead-step", outcome))
	require.Len(t, tr.calls, 1)
	assert.Equal(t, persistence.TaskStatusAwaitingExternal, tr.calls[0].to)
}

// TestHandleLeadHandoff_DispatchesClosureRequest — routes to the
// closure-request handler with the summary propagated.
func TestHandleLeadHandoff_DispatchesClosureRequest(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec1", TaskID: task.ID}
	outcome := &LeadOutcome{
		Outcome:        LeadOutcomeClosureRequest,
		ClosureRequest: &ClosureRequestPayload{Summary: "wrap-up", DurationDescription: "1h"},
	}
	require.NoError(t, e.handleLeadHandoff(context.Background(), task, exec, "lead-step", outcome))
	// handleClosureRequestOutcome inserts a closure_request message.
	require.Len(t, msgs.inserted, 1)
}

// TestHandleLeadHandoff_UnknownOutcomeIsError — the default branch
// of the switch surfaces an error mentioning the unhandled outcome
// label so the operator can diagnose schema mismatches.
func TestHandleLeadHandoff_UnknownOutcomeIsError(t *testing.T) {
	e := newHandoffExecutor(&fakeMessageRepo{}, newFakeTaskRepo())
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	outcome := &LeadOutcome{Outcome: LeadOutcomeKind("unknown-thing")}
	err := e.handleLeadHandoff(context.Background(), task, exec, "lead-step", outcome)
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unknown-thing"))
}
