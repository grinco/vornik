package watchdog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubReservationSweeper records every SweepTerminalAndStale call so
// tests can assert the cutoff math and exercise the error path. n is
// the count the sweep claims to have settled; err forces the failure
// branch.
type stubReservationSweeper struct {
	mu          sync.Mutex
	n           int64
	err         error
	calls       int
	lastCutoff  time.Time
	lastNow     time.Time
	gotCutoffNs []int64
}

func (s *stubReservationSweeper) SweepTerminalAndStale(_ context.Context, staleCutoff, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastCutoff = staleCutoff
	s.lastNow = now
	s.gotCutoffNs = append(s.gotCutoffNs, staleCutoff.UnixNano())
	if s.err != nil {
		return 0, s.err
	}
	return s.n, nil
}

func (s *stubReservationSweeper) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stubLeaderGate is a togglable LeaderGate for the runLoop gate branch.
type stubLeaderGate struct{ leader bool }

func (g *stubLeaderGate) IsLeader() bool { return g.leader }

// noRowsExecRepo is the common "nothing RUNNING" exec repo used by the
// sweep-focused tests that don't care about the stuck-RUNNING path.
func noRowsExecRepo() *stubExecRepo {
	return &stubExecRepo{
		listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil
		},
	}
}

// ---------------------------------------------------------------------------
// Enabled() — currently 0% covered.
// ---------------------------------------------------------------------------

// TestEnabled_ReflectsConfigAndNilSafe — Enabled() must mirror the
// effective config flag and survive a nil receiver (the daemon's
// initWatchdog wiring can hand callers a nil watchdog when execRepo was
// missing; Enabled() is the diagnostic those callers poke).
func TestEnabled_ReflectsConfigAndNilSafe(t *testing.T) {
	on := New(Config{Enabled: true}, &stubExecRepo{}, nil, zerolog.Nop(), nil)
	require.NotNil(t, on)
	assert.True(t, on.Enabled(), "Enabled must report true when config armed")

	off := New(Config{Enabled: false}, &stubExecRepo{}, nil, zerolog.Nop(), nil)
	require.NotNil(t, off)
	assert.False(t, off.Enabled(), "Enabled must report false when config disabled")

	var nilWD *Watchdog
	assert.False(t, nilWD.Enabled(), "nil watchdog must report disabled, not panic")
}

// ---------------------------------------------------------------------------
// Setters — SetLeaderGate / SetReservationSweeper, both 0% covered, both
// nil-receiver safe.
// ---------------------------------------------------------------------------

// TestSetters_WireAndNilSafe verifies the post-construction wiring
// setters install their collaborator and don't panic on a nil receiver.
func TestSetters_WireAndNilSafe(t *testing.T) {
	w := New(DefaultConfig(), &stubExecRepo{}, nil, zerolog.Nop(), nil)
	require.NotNil(t, w)

	gate := &stubLeaderGate{leader: true}
	w.SetLeaderGate(gate)
	assert.Same(t, gate, w.leaderGate, "SetLeaderGate must install the gate")

	sweeper := &stubReservationSweeper{}
	w.SetReservationSweeper(sweeper)
	assert.Same(t, sweeper, w.reservationSweeper, "SetReservationSweeper must install the sweeper")

	var nilWD *Watchdog
	assert.NotPanics(t, func() { nilWD.SetLeaderGate(gate) }, "nil receiver must be safe")
	assert.NotPanics(t, func() { nilWD.SetReservationSweeper(sweeper) }, "nil receiver must be safe")
}

// ---------------------------------------------------------------------------
// sweepBudgetReservations — 14.3% covered. Default-stale fallback,
// explicit-stale cutoff math, metric increment, error path, nil-sweeper
// no-op, zero-settled no-op.
// ---------------------------------------------------------------------------

// TestSweepBudgetReservations_NilSweeperNoop — with no sweeper wired the
// sweep is a silent no-op (the executor's prompt settle still runs).
func TestSweepBudgetReservations_NilSweeperNoop(t *testing.T) {
	w := New(DefaultConfig(), noRowsExecRepo(), nil, zerolog.Nop(), nil)
	w.ctx = context.Background()
	assert.NotPanics(t, func() { w.sweepBudgetReservations(context.Background()) })
}

// TestSweepBudgetReservations_DefaultStaleCutoff — when
// Config.ReservationStaleAfter is unset the sweep must pass now minus
// defaultReservationStaleAfter (6h) as the cutoff. This is the backstop
// that guarantees a leaked reservation can't block a hard cap forever,
// so the exact bound is load-bearing.
func TestSweepBudgetReservations_DefaultStaleCutoff(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sw := &stubReservationSweeper{n: 0}
	w := New(DefaultConfig(), noRowsExecRepo(), nil, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.SetReservationSweeper(sw)

	w.sweepBudgetReservations(context.Background())

	require.Equal(t, 1, sw.callCount())
	assert.True(t, sw.lastCutoff.Equal(now.Add(-defaultReservationStaleAfter)),
		"unset stale bound must fall back to the 6h default cutoff, got %s", sw.lastCutoff)
	assert.True(t, sw.lastNow.Equal(now), "now must be threaded through unchanged")
}

// TestSweepBudgetReservations_ExplicitStaleCutoff — a configured stale
// window must be honored exactly (not the default).
func TestSweepBudgetReservations_ExplicitStaleCutoff(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sw := &stubReservationSweeper{n: 0}
	cfg := DefaultConfig()
	cfg.ReservationStaleAfter = 2 * time.Hour
	w := New(cfg, noRowsExecRepo(), nil, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.SetReservationSweeper(sw)

	w.sweepBudgetReservations(context.Background())

	require.Equal(t, 1, sw.callCount())
	assert.True(t, sw.lastCutoff.Equal(now.Add(-2*time.Hour)),
		"explicit stale bound must be used verbatim, got %s", sw.lastCutoff)
}

// TestSweepBudgetReservations_MetricOnSettle — a positive settle count
// increments the reservationsSwept counter; the success-log path runs.
func TestSweepBudgetReservations_MetricOnSettle(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sw := &stubReservationSweeper{n: 4}
	reg := &countingRegisterer{}
	m := NewMetrics(reg)
	w := New(DefaultConfig(), noRowsExecRepo(), nil, zerolog.Nop(), m)
	w.now = func() time.Time { return now }
	w.SetReservationSweeper(sw)

	w.sweepBudgetReservations(context.Background())
	assert.Equal(t, float64(4), readCounter(t, m.reservationsSwept),
		"settled count must land on the reservationsSwept counter")
}

// TestSweepBudgetReservations_ErrorIsNonFatal — a sweep error is logged
// and swallowed; no metric increment, no panic.
func TestSweepBudgetReservations_ErrorIsNonFatal(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sw := &stubReservationSweeper{err: errors.New("settle blew up")}
	reg := &countingRegisterer{}
	m := NewMetrics(reg)
	w := New(DefaultConfig(), noRowsExecRepo(), nil, zerolog.Nop(), m)
	w.now = func() time.Time { return now }
	w.SetReservationSweeper(sw)

	assert.NotPanics(t, func() { w.sweepBudgetReservations(context.Background()) })
	assert.Equal(t, float64(0), readCounter(t, m.reservationsSwept),
		"a failed sweep must not increment the settled counter")
}

// TestScanOnce_RunsBudgetSweep — the sweep is wired into the scan loop,
// so scanOnce must invoke it once per tick alongside the other backstops.
func TestScanOnce_RunsBudgetSweep(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sw := &stubReservationSweeper{n: 1}
	w := New(DefaultConfig(), noRowsExecRepo(), nil, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()
	w.SetReservationSweeper(sw)

	w.scanOnce()
	w.scanOnce()
	assert.Equal(t, 2, sw.callCount(), "scanOnce must drive the budget sweep every tick")
}

// ---------------------------------------------------------------------------
// sweepExpiredApprovals — 73.7%: list-error path, nil-task skip,
// boundary at exactly the timeout, no-task-repo no-op, metric increment.
// ---------------------------------------------------------------------------

// TestSweepExpiredApprovals_ListErrorIsNonFatal — a List failure is
// logged and the sweep returns without cancelling anything.
func TestSweepExpiredApprovals_ListErrorIsNonFatal(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	tasks := newStubTaskRepo()
	tasks.listErr = errors.New("list down")
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	assert.NotPanics(t, func() { w.sweepExpiredApprovals(context.Background()) })
	assert.Empty(t, tasks.cancelledIDs(), "list error must cancel nothing")
}

// TestSweepExpiredApprovals_NoTaskRepoNoop — approval sweep is a no-op
// when no task repo is wired even though a timeout is configured.
func TestSweepExpiredApprovals_NoTaskRepoNoop(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	w := New(cfg, noRowsExecRepo(), nil, zerolog.Nop(), nil) // taskRepo nil
	w.now = func() time.Time { return now }
	w.ctx = context.Background()
	assert.NotPanics(t, func() { w.sweepExpiredApprovals(context.Background()) })
}

// TestSweepExpiredApprovals_NilTaskRowSkipped — a nil entry in the List
// result (defensive against repo bugs) must be skipped, not dereferenced.
func TestSweepExpiredApprovals_NilTaskRowSkipped(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	tasks := newStubTaskRepo()
	tasks.listResult = []*persistence.Task{
		nil,
		{ID: "stale", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
	}
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	require.NotPanics(t, func() { w.sweepExpiredApprovals(context.Background()) })
	got := tasks.cancelledIDs()
	assert.Equal(t, persistence.TaskStatusCancelled, got["stale"],
		"the real stale task must still be cancelled despite the nil neighbor")
	assert.Len(t, got, 1)
}

// TestSweepExpiredApprovals_BoundaryAtExactTimeout — the comparison is
// UpdatedAt.Before(cutoff), so a task whose age is EXACTLY the timeout
// (updated_at == cutoff) is NOT past it and must survive; one nanosecond
// older must be cancelled. This pins the strict-inequality boundary.
func TestSweepExpiredApprovals_BoundaryAtExactTimeout(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	timeout := 96 * time.Hour
	tasks := newStubTaskRepo()
	tasks.listResult = []*persistence.Task{
		// exactly at the cutoff — updated_at == now-timeout — not Before → survives
		{ID: "exact", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-timeout)},
		// one ns older than the cutoff → Before → cancelled
		{ID: "justover", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-timeout - time.Nanosecond)},
	}
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = timeout
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.sweepExpiredApprovals(context.Background())
	got := tasks.cancelledIDs()
	_, exactCancelled := got["exact"]
	assert.False(t, exactCancelled, "a task at EXACTLY the timeout is not past it — must survive")
	assert.Equal(t, persistence.TaskStatusCancelled, got["justover"],
		"one tick past the timeout must be cancelled")
}

// TestSweepExpiredApprovals_CancelErrorContinues — when UpdateStatus
// fails for one task the sweep logs it and continues to the next; the
// metric only counts the successes.
func TestSweepExpiredApprovals_CancelErrorContinues(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	tasks := &errOnUpdateStatusTaskRepo{
		stubTaskRepo: newStubTaskRepo(),
		failID:       "boom",
	}
	tasks.listResult = []*persistence.Task{
		{ID: "boom", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
		{ID: "ok", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
	}
	reg := &countingRegisterer{}
	m := NewMetrics(reg)
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), m)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.sweepExpiredApprovals(context.Background())
	assert.Equal(t, persistence.TaskStatusCancelled, tasks.cancelledIDs()["ok"],
		"the second task must still be cancelled after the first errored")
	assert.Equal(t, float64(1), readCounter(t, m.approvalsExpired),
		"only the successful cancel must count toward approvalsExpired")
}

// TestSweepExpiredApprovals_MetricCountsCancellations — happy path
// metric: every successful approval cancel increments approvalsExpired.
func TestSweepExpiredApprovals_MetricCountsCancellations(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	tasks := newStubTaskRepo()
	tasks.listResult = []*persistence.Task{
		{ID: "a", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
		{ID: "b", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-200 * time.Hour)},
		{ID: "fresh", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-1 * time.Hour)},
	}
	reg := &countingRegisterer{}
	m := NewMetrics(reg)
	cfg := DefaultConfig()
	cfg.ApprovalTimeout = 96 * time.Hour
	w := New(cfg, noRowsExecRepo(), tasks, zerolog.Nop(), m)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.sweepExpiredApprovals(context.Background())
	assert.Equal(t, float64(2), readCounter(t, m.approvalsExpired),
		"two stale approvals cancelled → counter at 2; the fresh one is left")
	assert.Len(t, tasks.cancelledIDs(), 2)
}

// ---------------------------------------------------------------------------
// runLoop leader-gate branch — 70%: a non-leader tick must skip scanning.
// ---------------------------------------------------------------------------

// TestRunLoop_NonLeaderSkipsScan — when a leader gate is wired and this
// daemon is not the leader, the ticker-driven scan must be skipped so two
// daemons don't both act on the same stuck row. The initial pre-ticker
// scan still runs once (it's not gated). We assert no further scans occur
// while non-leader.
func TestRunLoop_NonLeaderSkipsScan(t *testing.T) {
	var scanCount int
	var mu sync.Mutex
	exec := &stubExecRepo{
		listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			mu.Lock()
			scanCount++
			mu.Unlock()
			return nil, nil
		},
	}
	cfg := DefaultConfig()
	cfg.Interval = 5 * time.Millisecond
	cfg.Action = ActionWarn
	w := New(cfg, exec, nil, zerolog.Nop(), nil)
	w.SetLeaderGate(&stubLeaderGate{leader: false})

	require.NoError(t, w.Start())
	time.Sleep(60 * time.Millisecond) // many ticks would have fired if not gated
	require.NoError(t, w.Stop())

	mu.Lock()
	got := scanCount
	mu.Unlock()
	// Only the un-gated startup scan should have listed executions.
	assert.LessOrEqual(t, got, 1, "non-leader must skip ticker scans; got %d List calls", got)
}

// TestRunLoop_LeaderScansOnTick — the positive companion: a leader keeps
// scanning on every tick, so the List call count climbs past the single
// startup scan.
func TestRunLoop_LeaderScansOnTick(t *testing.T) {
	var scanCount int
	var mu sync.Mutex
	exec := &stubExecRepo{
		listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			mu.Lock()
			scanCount++
			mu.Unlock()
			return nil, nil
		},
	}
	cfg := DefaultConfig()
	cfg.Interval = 5 * time.Millisecond
	cfg.Action = ActionWarn
	w := New(cfg, exec, nil, zerolog.Nop(), nil)
	w.SetLeaderGate(&stubLeaderGate{leader: true})

	require.NoError(t, w.Start())
	time.Sleep(60 * time.Millisecond)
	require.NoError(t, w.Stop())

	mu.Lock()
	got := scanCount
	mu.Unlock()
	assert.Greater(t, got, 1, "leader must scan on each tick; got only %d List calls", got)
}

// ---------------------------------------------------------------------------
// handleStuck warn-with-metrics branch (84.2%) + scanOnce threshold
// boundary (the at-exactly-threshold edge).
// ---------------------------------------------------------------------------

// TestHandleStuck_WarnIncrementsDetectedMetric — warn action: the
// detected counter fires but failed does NOT (the row is left RUNNING).
// Covers the warn-mode metric branch inside handleStuck.
func TestHandleStuck_WarnIncrementsDetectedMetric(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{
		listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{runningExec("e", "t1", "p1", now, 45*time.Minute)}, nil
		},
	}
	reg := &countingRegisterer{}
	m := NewMetrics(reg)
	cfg := DefaultConfig()
	cfg.Action = ActionWarn
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, nil, zerolog.Nop(), m)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.scanOnce()
	assert.Equal(t, float64(1), readCounter(t, m.detected), "warn must count a detection")
	assert.Equal(t, float64(0), readCounter(t, m.failed), "warn must NOT count a failure")
	assert.Empty(t, exec.failures(), "warn must not write the row")
}

// TestScanOnce_ThresholdBoundaryExact — the stuck comparison is
// !UpdatedAt.Before(threshold) → fresh. A row whose updated_at is exactly
// now-StuckThreshold is NOT before the threshold and must be treated as
// fresh (not stuck). One nanosecond older crosses into stuck. Pins the
// boundary so a future >= vs > swap is caught.
func TestScanOnce_ThresholdBoundaryExact(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Minute

	t.Run("exactly at threshold is fresh", func(t *testing.T) {
		exec := &stubExecRepo{
			listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
				return []*persistence.Execution{runningExec("e", "t1", "p1", now, threshold)}, nil
			},
		}
		cfg := DefaultConfig()
		cfg.Action = ActionFail
		cfg.StuckThreshold = threshold
		w := New(cfg, exec, newStubTaskRepo(), zerolog.Nop(), nil)
		w.now = func() time.Time { return now }
		w.ctx = context.Background()
		w.scanOnce()
		assert.Empty(t, exec.failures(), "updated_at == now-threshold is exactly at the bound — must be fresh")
	})

	t.Run("one ns past threshold is stuck", func(t *testing.T) {
		exec := &stubExecRepo{
			listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
				return []*persistence.Execution{runningExec("e", "t1", "p1", now, threshold+time.Nanosecond)}, nil
			},
		}
		tasks := newStubTaskRepo()
		tasks.setTask(&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})
		cfg := DefaultConfig()
		cfg.Action = ActionFail
		cfg.StuckThreshold = threshold
		w := New(cfg, exec, tasks, zerolog.Nop(), nil)
		w.now = func() time.Time { return now }
		w.ctx = context.Background()
		w.scanOnce()
		assert.Len(t, exec.failures(), 1, "one ns past the threshold must be classified stuck")
	})
}

// TestScanOnce_NilRowInListSkipped — a nil entry in the RUNNING list
// (defensive against a repo returning a sparse slice) must be skipped,
// and the real stuck row beside it must still be acted on.
func TestScanOnce_NilRowInListSkipped(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{
		listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{nil, runningExec("e", "t1", "p1", now, 45*time.Minute)}, nil
		},
	}
	tasks := newStubTaskRepo()
	tasks.setTask(&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})
	cfg := DefaultConfig()
	cfg.Action = ActionFail
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	require.NotPanics(t, func() { w.scanOnce() })
	require.Len(t, exec.failures(), 1, "nil row skipped, real stuck row still failed")
	assert.Equal(t, "e", exec.failures()[0].id)
}

// TestScanOnce_ListErrorAbortsScan — a List error short-circuits the
// scan: no failures, and (critically) the orphan/approval/budget
// backstops downstream of List are NOT reached on this tick.
func TestScanOnce_ListErrorAbortsScan(t *testing.T) {
	exec := &stubExecRepo{
		listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, errors.New("executions table unreachable")
		},
	}
	w := New(DefaultConfig(), exec, nil, zerolog.Nop(), nil)
	w.ctx = context.Background()

	require.NotPanics(t, func() { w.scanOnce() })
	assert.Empty(t, exec.failures())
	assert.Equal(t, 0, exec.orphanReconcileCalls(),
		"a List failure must abort before the orphan reconcile backstop")
}

// ---------------------------------------------------------------------------
// markFailed task-Update error path (89.5%) + nil-task branch.
// ---------------------------------------------------------------------------

// TestMarkFailed_TaskUpdateErrorSurfaces — when the execution flip
// succeeds but the task-side Update errors, markFailed surfaces it so the
// caller skips the success metric. The execution row is already flipped.
func TestMarkFailed_TaskUpdateErrorSurfaces(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	tasks := &errOnUpdateTaskRepo{stubTaskRepo: newStubTaskRepo()}
	tasks.setTask(&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	err := w.markFailed(context.Background(), runningExec("e1", "t1", "p1", now, 45*time.Minute), 45*time.Minute)
	require.Error(t, err, "task Update error must surface")
	assert.Contains(t, err.Error(), "task Update")
	assert.Len(t, exec.failures(), 1, "execution row was flipped before the Update error")
}

// TestMarkFailed_NilTaskShortCircuits — Get returning (nil, nil) (task
// vanished between scan and Get) must short-circuit cleanly: execution
// flipped, no Update attempted, no error.
func TestMarkFailed_NilTaskShortCircuits(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	tasks := &nilGetTaskRepo{stubTaskRepo: newStubTaskRepo()}

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	err := w.markFailed(context.Background(), runningExec("e1", "t1", "p1", now, 45*time.Minute), 45*time.Minute)
	require.NoError(t, err, "a vanished (nil) task must not error the watchdog")
	assert.Len(t, exec.failures(), 1, "execution still flipped")
	assert.Empty(t, tasks.updates, "no Update on a nil task")
}

// TestMarkFailed_FlipsTaskFromPending — defensive-status switch: a task
// in PENDING (not yet terminal) is legitimately flipped to FAILED. Pairs
// with the existing terminal-race test which asserts the no-regress side.
func TestMarkFailed_FlipsTaskFromPending(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	tasks := newStubTaskRepo()
	tasks.setTask(&persistence.Task{ID: "t1", Status: persistence.TaskStatusPending})

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	require.NoError(t, w.markFailed(context.Background(), runningExec("e1", "t1", "p1", now, 45*time.Minute), 45*time.Minute))
	updates := tasks.updateLog()
	require.Len(t, updates, 1, "a non-terminal task must be flipped")
	assert.Equal(t, persistence.TaskStatusFailed, updates[0].Status)
	require.NotNil(t, updates[0].LastError, "LastError must be populated for the dashboard")
	assert.Contains(t, *updates[0].LastError, "watchdog: execution stuck")
}

// ---------------------------------------------------------------------------
// markSeen repeat branch (83.3%): the second call at the same updated_at
// returns false (already seen); a different updated_at returns true.
// ---------------------------------------------------------------------------

// TestMarkSeen_RepeatVsFresh exercises the dedupe primitive directly:
// the first mark at an updated_at is "fire" (true), a repeat at the same
// stamp is silenced (false), and a new stamp fires again (true).
func TestMarkSeen_RepeatVsFresh(t *testing.T) {
	w := New(DefaultConfig(), &stubExecRepo{}, nil, zerolog.Nop(), nil)
	require.NotNil(t, w)
	stamp := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

	assert.True(t, w.markSeen("e1", stamp), "first observation must fire")
	assert.True(t, w.hasSeen("e1", stamp), "and be recorded")
	assert.False(t, w.markSeen("e1", stamp), "repeat at same updated_at must be silenced")

	newer := stamp.Add(time.Minute)
	assert.True(t, w.markSeen("e1", newer), "a fresh updated_at re-arms detection")
	assert.False(t, w.hasSeen("e1", stamp), "the old stamp is no longer the recorded one")
}

// ---------------------------------------------------------------------------
// Test-only repo variants for the error/nil branches above.
// ---------------------------------------------------------------------------

// errOnUpdateTaskRepo errors on Update (the task-side flip) while Get
// succeeds — exercises markFailed's "task Update" error wrap.
type errOnUpdateTaskRepo struct{ *stubTaskRepo }

func (r *errOnUpdateTaskRepo) Update(_ context.Context, _ *persistence.Task) error {
	return errors.New("update failed")
}

// nilGetTaskRepo returns (nil, nil) from Get — the vanished-task race.
type nilGetTaskRepo struct{ *stubTaskRepo }

func (r *nilGetTaskRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return nil, nil
}

// errOnUpdateStatusTaskRepo fails UpdateStatus for one specific id and
// records the rest, so the approval sweep's continue-on-error path runs.
type errOnUpdateStatusTaskRepo struct {
	*stubTaskRepo
	failID string
}

func (r *errOnUpdateStatusTaskRepo) UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error {
	if id == r.failID {
		return errors.New("cancel failed")
	}
	return r.stubTaskRepo.UpdateStatus(ctx, id, status)
}

// readCounter extracts the current float value of a prometheus counter
// via its Write hook — no registry scrape needed.
func readCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}
