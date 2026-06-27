package executor

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestResumePaused_RefusesEmptyID — defensive validation: a UI
// handler bug that lets through an empty execution ID must not
// crash the executor or silently no-op.
func TestResumePaused_RefusesEmptyID(t *testing.T) {
	e, _, _, _, _ := setup()
	err := e.ResumePaused("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "execution ID required")
}

// TestResumePaused_RefusesUnknownExecution — a stale browser tab
// or a typo in a CLI call lands here; must surface a non-fatal
// error rather than a nil-deref.
func TestResumePaused_RefusesUnknownExecution(t *testing.T) {
	e, _, _, _, _ := setup()
	err := e.ResumePaused("exec_does_not_exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestResumePaused_RefusesNonPausedExecution — only Paused
// executions are resumable. A RUNNING execution shouldn't be
// re-resumed (that would race the existing goroutine).
func TestResumePaused_RefusesNonPausedExecution(t *testing.T) {
	e, _, er, _, _ := setup()
	er.execs["e1"] = &persistence.Execution{
		ID:        "e1",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusRunning,
	}
	err := e.ResumePaused("e1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not paused",
		"resuming a non-Paused execution must refuse so the operator can't accidentally race the scheduler")
}

// TestResumePaused_RefusesNonResumablePauseReason — operator-paused
// and awaiting-children executions need explicit signals to resume.
// The shutdown-paused / retry-from-step subset auto-resumes; others
// stay paused.
func TestResumePaused_RefusesNonResumablePauseReason(t *testing.T) {
	e, _, er, _, _ := setup()
	er.execs["e1"] = &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"pausedReason":"operator"}`),
	}
	err := e.ResumePaused("e1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not auto-resumable",
		"operator-paused executions need an explicit resume signal, not the auto path")
}

// TestResumePaused_HappyPath_ShutdownReason — the canonical Recover()
// loop equivalent: a shutdown-paused execution flips to Running and
// gets handed to recoverExecution. Asserts the status transition;
// recoverExecution itself is exercised by the broader test suite.
func TestResumePaused_HappyPath_ShutdownReason(t *testing.T) {
	e, _, er, _, tr := setup()
	stepID := "implement"
	er.execs["e1"] = &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		ProjectID:     "p1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"pausedReason":"shutdown","currentStepId":"implement"}`),
		CurrentStepID: &stepID,
	}
	// recoverExecution loads the task; make sure it exists in a
	// non-terminal state so it doesn't short-circuit as orphaned.
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now(),
	})

	// ResumePaused triggers recoverExecution which spawns a
	// goroutine; that goroutine may fail downstream because we
	// don't wire a workflow resolver. The behaviour we're pinning
	// here is the Paused→Running flip + the absence of an error
	// before the goroutine spawn. recoverExecution's own internal
	// failure modes are covered by sibling tests.
	err := e.ResumePaused("e1")
	require.NoError(t, err, "shutdown-paused execution must flip to Running and hand off to recoverExecution without error")
	// snapshotStatus locks the mock's mutex so the read doesn't
	// race against the runExecution goroutine recoverExecution
	// spawns. Plain er.execs["e1"].Status is a data race.
	assert.Equal(t, persistence.ExecutionStatusRunning, er.snapshotStatus("e1"),
		"execution status must flip Paused→Running before recoverExecution is called")
}

// TestResumePaused_HappyPath_RetryFromStepReason — the 2026.6.0
// retry-from-step pause reason is the second auto-resumable case.
// Same flip semantics as shutdown.
func TestResumePaused_HappyPath_RetryFromStepReason(t *testing.T) {
	e, _, er, _, tr := setup()
	stepID := "review"
	er.execs["e1"] = &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		ProjectID:     "p1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"pausedReason":"retry_from_step","currentStepId":"review"}`),
		CurrentStepID: &stepID,
	}
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now(),
	})

	err := e.ResumePaused("e1")
	require.NoError(t, err)
	assert.Equal(t, persistence.ExecutionStatusRunning, er.snapshotStatus("e1"))
}

// TestResumePaused_FlipStatusErrorIsSurfaced — if the
// Paused→Running flip fails (DB hiccup), the handler must surface
// the error rather than continuing into recoverExecution (which
// would see a still-Paused row).
func TestResumePaused_FlipStatusErrorIsSurfaced(t *testing.T) {
	e, _, er, _, _ := setup()
	er.execs["e1"] = &persistence.Execution{
		ID:            "e1",
		TaskID:        "t1",
		Status:        persistence.ExecutionStatusPaused,
		StateSnapshot: []byte(`{"pausedReason":"shutdown"}`),
	}
	er.updateStatusErr = errors.New("db transient")
	err := e.ResumePaused("e1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flip paused→running")
}
