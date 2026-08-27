package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// The residual bucket (`container_non_zero_exit`) was 3,027 of 5,791
// classified step failures on 2026-08-26 and carried the container's exit code
// in eleven of them. The code IS known at the point the step fails — it is in
// scope where agentError is built — it simply was not carried out to the row.
//
// It travels as a typed error rather than a new return value because the
// failure already crosses several wrappers before it is recorded, and the
// codebase already uses this shape: ClassifyExecutionFailure reads a
// `FailureClass() string` off the error for exactly the same reason.
//
// Design: https://docs.vornik.io (D2)

func TestContainerExitCodeFromError_ExtractsTypedCode(t *testing.T) {
	err := newContainerExitError(137, "container exited with code 137")
	got := containerExitCodeFromError(err)
	require.NotNil(t, got, "a typed container error must yield its exit code")
	assert.Equal(t, 137, *got)
}

// The error is wrapped before it reaches the recording site, so extraction
// must survive %w — this is the case a direct type assertion would miss.
func TestContainerExitCodeFromError_SurvivesWrapping(t *testing.T) {
	inner := newContainerExitError(125, "container exited with code 125")
	wrapped := fmt.Errorf("step %q failed: %w", "research", inner)
	got := containerExitCodeFromError(wrapped)
	require.NotNil(t, got, "wrapping must not hide the exit code")
	assert.Equal(t, 125, *got)
}

// Zero is a real exit code, not an absence. A container that exited 0 and
// still failed the step (a verifier rejection) must be distinguishable from a
// step that never ran a container at all.
func TestContainerExitCodeFromError_ZeroIsNotAbsent(t *testing.T) {
	got := containerExitCodeFromError(newContainerExitError(0, "verifier rejected the result"))
	require.NotNil(t, got, "exit code 0 must not be reported as 'no container'")
	assert.Equal(t, 0, *got)
}

func TestContainerExitCodeFromError_PlainErrorYieldsNil(t *testing.T) {
	assert.Nil(t, containerExitCodeFromError(errors.New("something else went wrong")))
}

func TestContainerExitCodeFromError_NilErrorYieldsNil(t *testing.T) {
	assert.Nil(t, containerExitCodeFromError(nil))
}

// The typed error must not change what operators and the classifier read:
// ClassifyExecutionFailure and refineAgentFailureOutcome both match on the
// message text, so a wrapper that altered it would silently reclassify every
// container failure.
func TestContainerExitError_PreservesMessageVerbatim(t *testing.T) {
	msg := "agent reported FAILED status: LLM call failed: upstream provider returned an error"
	assert.Equal(t, msg, newContainerExitError(1, msg).Error())
}

// The exit code must reach the row, not just the error. This is the seam the
// whole of D2 exists for: a code that stays inside the error chain leaves the
// residual bucket exactly as undiagnosable as it was.
func TestRecordStepOutcome_StampsContainerExitCode(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	code := 137
	e.recordStepOutcomeWithSignalsAndBudget(context.Background(), task, exec,
		"step_0", "coder", "m", string(stepoutcome.Failed),
		stepoutcome.ClassUnclassified, "boom", nil, nil, nil,
		agentBudgetStamp{ContainerExitCode: &code}, taintStamp{})

	require.Len(t, repo.rows, 1)
	require.NotNil(t, repo.rows[0].ContainerExitCode, "exit code did not reach the row")
	assert.Equal(t, 137, *repo.rows[0].ContainerExitCode)
}

// A non-agent step never ran a container, so its column stays NULL rather
// than claiming a clean exit.
func TestRecordStepOutcome_NonAgentStepLeavesExitCodeNull(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	e.recordStepOutcome(context.Background(), task, exec, "step_0", "system", "",
		string(stepoutcome.Failed), stepoutcome.ClassGateEvalFailed, "gate rejected", nil, nil)

	require.Len(t, repo.rows, 1)
	assert.Nil(t, repo.rows[0].ContainerExitCode,
		"a step that never ran a container must leave the column NULL")
}
