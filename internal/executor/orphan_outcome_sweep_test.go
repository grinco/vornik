package executor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// The supersede paths terminalise a task's leftover executions and used to
// leave the step outcomes beneath them at pending_validation — counted as
// neither ok nor a failure by every quality query — for a reconciler to relabel
// minutes later, as `superseded`. That is the OPERATOR's word for
// retry-from-step, and it is how 809 of 835 such rows came to say something
// they did not mean (design 2026-09-04-orphaned-step-outcomes).
type recordingOrphanSweeper struct {
	*stubStepOutcomeRepo
	taskIDs []string
	err     error
}

func (r *recordingOrphanSweeper) SweepPendingForTaskOrphans(_ context.Context, taskID string) (int64, error) {
	r.taskIDs = append(r.taskIDs, taskID)
	if r.err != nil {
		return 0, r.err
	}
	return 1, nil
}

func newSweepExecutor(t *testing.T) (*Executor, *recordingOrphanSweeper) {
	t.Helper()
	e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	sw := &recordingOrphanSweeper{stubStepOutcomeRepo: newStubStepOutcomeRepo()}
	e.outcomeRepo = sw
	return e, sw
}

func TestSupersedeStaleExecutions_SweepsItsOutcomes(t *testing.T) {
	e, sw := newSweepExecutor(t)
	e.supersedeStaleExecutions(context.Background(), "task-1")
	if len(sw.taskIDs) != 1 || sw.taskIDs[0] != "task-1" {
		t.Errorf("sweep calls = %v, want one for task-1 — the path that knows the reason "+
			"must record it, not leave it to the backstop", sw.taskIDs)
	}
}

func TestCascadeOrphanExecutions_SweepsItsOutcomes(t *testing.T) {
	e, sw := newSweepExecutor(t)
	e.cascadeOrphanExecutions(context.Background(), "task-2")
	if len(sw.taskIDs) != 1 || sw.taskIDs[0] != "task-2" {
		t.Errorf("sweep calls = %v, want one for task-2 — 163 of the orphan rows come from "+
			"this path, not only from the start-of-run one", sw.taskIDs)
	}
}

// Best-effort, exactly like the supersede sweeps it follows: a failure is
// logged and the backstop still catches the rows.
func TestOrphanOutcomeSweep_IsBestEffort(t *testing.T) {
	e, sw := newSweepExecutor(t)
	sw.err = errors.New("db down")
	e.supersedeStaleExecutions(context.Background(), "task-3") // must not panic
	e.sweepTaskOrphanOutcomes(context.Background(), "", "test")
	if len(sw.taskIDs) != 1 {
		t.Errorf("an empty task id must not reach the repository: %v", sw.taskIDs)
	}
}

// A backend without the capability is a no-op rather than a panic: the sweep is
// an optional capability, not a repository-interface method.
func TestOrphanOutcomeSweep_NoCapabilityIsANoOp(_ *testing.T) {
	e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.outcomeRepo = newStubStepOutcomeRepo()
	e.sweepTaskOrphanOutcomes(context.Background(), "task-4", "test")
}

// The vocabulary must carry the new value distinctly from the operator's one.
func TestOrphanedIsNotSuperseded(t *testing.T) {
	if stepoutcome.Orphaned == stepoutcome.Superseded {
		t.Fatal("orphaned and superseded must be different values — telling them apart is the point")
	}
	if stepoutcome.Orphaned != "orphaned" {
		t.Errorf("orphaned = %q; the literal is what history and dashboards match on", stepoutcome.Orphaned)
	}
}

// THE ORDER IS THE SAFETY PROPERTY.
//
// handleSuccess finalises its OWN execution's pending rows to `ok` BEFORE it
// calls cascadeOrphanExecutions, which now sweeps the task's terminal
// executions to `orphaned`. If those two ever swap, the orphan sweep reaches
// rows that were about to be recorded as passes and relabels a success as an
// absence — silently, and only under the timing that put the rows there.
//
// Asserted on the source rather than by racing goroutines: the ordering is a
// textual property of one function, and a test that tried to observe the race
// would be the flaky kind that gets deleted.
func TestHandleSuccess_SweepsItsOwnOutcomesBeforeCascading(t *testing.T) {
	src, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatalf("read workflow.go: %v", err)
	}
	body := string(src)
	own := strings.Index(body, "e.sweepPendingOutcomes(ctx, execution.ID, string(stepoutcome.OK))")
	cascade := strings.Index(body, "e.cascadeOrphanExecutions(ctx, task.ID)")
	if own < 0 || cascade < 0 {
		t.Fatalf("call sites not found (own=%d cascade=%d) — the test needs updating with the code", own, cascade)
	}
	if own > cascade {
		t.Error("cascadeOrphanExecutions now runs BEFORE this execution's own sweep: the orphan " +
			"sweep would relabel rows that were about to be finalised as ok")
	}
}
