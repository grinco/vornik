package watchdog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// fakePendingOutcomeRepo records the reconciler's arguments so the test can
// assert the guard values, and can be told to fail.
type fakePendingOutcomeRepo struct {
	calls     int
	olderThan time.Duration
	limit     int
	swept     int64
	err       error
}

func (f *fakePendingOutcomeRepo) SweepPendingForTerminalExecutions(_ context.Context, olderThan time.Duration, limit int) (int64, error) {
	f.calls++
	f.olderThan = olderThan
	f.limit = limit
	return f.swept, f.err
}

// Measured 2026-08-18 in production: 393 of `adaptive`'s 673 step outcomes over
// 30 days sat at `pending_validation` — documented as "never the final state" —
// and 391 of 392 belonged to a CANCELLED execution whose TASK had COMPLETED.
//
// SweepPending is scoped to the execution its caller is closing, and only three
// paths call it. cascadeOrphanExecutions, supersedeStaleExecutions and this
// watchdog's own orphan-PAUSED backstop all terminalise a task's OTHER
// executions at the executions-table level without sweeping the outcome rows
// underneath, so adaptive's delegating `route` step was never finalized by
// anything. doctor's model-health query excludes pending_validation from both
// the ok and the failure count, so the fleet's highest-volume workflow had no
// usable step-outcome record at all.
//
// The backstop belongs here, beside reconcileOrphanPaused: converging on the
// invariant repairs rows already stranded — which no call-site hook can — and
// covers crash recovery and any terminal path added later.
func TestReconcilePendingOutcomes_SweepsWithGuardsAndCountsMetric(t *testing.T) {
	repo := &fakePendingOutcomeRepo{swept: 7}
	w := New(Config{}, &stubExecRepo{}, nil, zerolog.Nop(), nil).
		WithStepOutcomeRepository(repo)

	w.reconcilePendingOutcomes(context.Background())

	assert.Equal(t, 1, repo.calls, "the reconciler must run on a scan")
	assert.Equal(t, pendingOutcomeSettleGrace, repo.olderThan,
		"a settle grace is required: a terminal path writes its execution status "+
			"around its own SweepPending, so a just-closed execution's rows are not "+
			"stranded and must not be relabelled superseded")
	assert.Equal(t, pendingOutcomeBatchLimit, repo.limit,
		"the batch must be bounded so a large backlog cannot monopolise a scan")
}

// A repository error is logged and swallowed, exactly like the orphan-PAUSED
// leg: the reconciler is best-effort and the next tick retries. A panic or a
// propagated error here would take down the whole scan, including the stuck-
// execution detection that is the watchdog's primary job.
func TestReconcilePendingOutcomes_RepoErrorIsBestEffort(t *testing.T) {
	repo := &fakePendingOutcomeRepo{err: errors.New("sweep failed")}
	w := New(Config{}, &stubExecRepo{}, nil, zerolog.Nop(), nil).
		WithStepOutcomeRepository(repo)

	w.reconcilePendingOutcomes(context.Background())

	assert.Equal(t, 1, repo.calls)
}

// Unwired is a no-op, so a deployment that has not passed the repository keeps
// a working watchdog rather than a nil dereference on every scan.
func TestReconcilePendingOutcomes_NilRepoIsNoop(_ *testing.T) {
	w := New(Config{}, &stubExecRepo{}, nil, zerolog.Nop(), nil)
	w.reconcilePendingOutcomes(context.Background())
}
