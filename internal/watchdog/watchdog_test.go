package watchdog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubExecRepo is a minimal in-memory ExecutionRepository for the
// watchdog tests. ListFunc lets each test return its own canned
// snapshot; recordedFailures captures every (id, msg, class) triple
// so assertions can verify the watchdog wrote the right class.
type stubExecRepo struct {
	mu                 sync.Mutex
	listFunc           func(ctx context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error)
	recordFailureErr   error
	recordedFailures   []recordedFailure
	supersedeOrphanN   int64 // returned by SupersedeOrphanPausedExecutions
	supersedeOrphanErr error
	supersedeCalls     int // how many times the reconcile was invoked
}

type recordedFailure struct {
	id      string
	message string
	code    string
}

func (s *stubExecRepo) List(ctx context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	if s.listFunc == nil {
		return nil, nil
	}
	return s.listFunc(ctx, f)
}

func (s *stubExecRepo) RecordFailure(_ context.Context, id, msg, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordFailureErr != nil {
		return s.recordFailureErr
	}
	s.recordedFailures = append(s.recordedFailures, recordedFailure{id, msg, code})
	return nil
}

func (s *stubExecRepo) SupersedeOrphanPausedExecutions(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.supersedeCalls++
	if s.supersedeOrphanErr != nil {
		return 0, s.supersedeOrphanErr
	}
	return s.supersedeOrphanN, nil
}

func (s *stubExecRepo) orphanReconcileCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.supersedeCalls
}

func (s *stubExecRepo) failures() []recordedFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedFailure, len(s.recordedFailures))
	copy(out, s.recordedFailures)
	return out
}

// stubTaskRepo captures the Update calls the watchdog issues when
// Action=fail. Get returns whatever was previously set via setTask.
type stubTaskRepo struct {
	mu         sync.Mutex
	tasks      map[string]*persistence.Task
	getErr     error
	updates    []*persistence.Task
	listResult []*persistence.Task // returned by List
	listErr    error
	statusSet  map[string]persistence.TaskStatus // id → status set via UpdateStatus
}

func newStubTaskRepo() *stubTaskRepo {
	return &stubTaskRepo{tasks: make(map[string]*persistence.Task)}
}

func (s *stubTaskRepo) setTask(t *persistence.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

func (s *stubTaskRepo) Get(_ context.Context, id string) (*persistence.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if t, ok := s.tasks[id]; ok {
		// return a copy so the watchdog's mutation doesn't bleed
		// across assertions
		cp := *t
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (s *stubTaskRepo) Update(_ context.Context, t *persistence.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.updates = append(s.updates, &cp)
	s.tasks[t.ID] = &cp
	return nil
}

func (s *stubTaskRepo) List(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listResult, s.listErr
}

func (s *stubTaskRepo) UpdateStatus(_ context.Context, id string, status persistence.TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusSet == nil {
		s.statusSet = map[string]persistence.TaskStatus{}
	}
	s.statusSet[id] = status
	return nil
}

func (s *stubTaskRepo) cancelledIDs() map[string]persistence.TaskStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]persistence.TaskStatus{}
	for k, v := range s.statusSet {
		out[k] = v
	}
	return out
}

func (s *stubTaskRepo) updateLog() []*persistence.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.Task, len(s.updates))
	copy(out, s.updates)
	return out
}

// runningExec returns a RUNNING execution row whose updated_at is `age`
// ago relative to `now`. Helper for setting up stuck-vs-fresh fixtures.
func runningExec(id, taskID, projectID string, now time.Time, age time.Duration) *persistence.Execution {
	return &persistence.Execution{
		ID:        id,
		TaskID:    taskID,
		ProjectID: projectID,
		Status:    persistence.ExecutionStatusRunning,
		UpdatedAt: now.Add(-age),
	}
}

// TestScanOnce_DetectsStuckExecution_WarnAction — the headline
// contract: an execution whose updated_at is older than the stuck
// threshold gets detected and logged exactly once. Warn-only must
// NOT touch the row.
func TestScanOnce_DetectsStuckExecution_WarnAction(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	stuck := runningExec("e-stuck", "t1", "p1", now, 45*time.Minute)
	fresh := runningExec("e-fresh", "t2", "p1", now, 1*time.Minute)

	exec := &stubExecRepo{
		listFunc: func(_ context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			require.NotNil(t, f.Status)
			assert.Equal(t, persistence.ExecutionStatusRunning, *f.Status,
				"watchdog must filter to RUNNING — scanning everything would be wasteful")
			return []*persistence.Execution{stuck, fresh}, nil
		},
	}

	cfg := DefaultConfig()
	cfg.Action = ActionWarn
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, nil, zerolog.Nop(), nil)
	require.NotNil(t, w)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.scanOnce()

	assert.Empty(t, exec.failures(),
		"warn action must not write FAILED — only logging")
}

// TestScanOnce_RunsOrphanReconcile — every scan invokes the orphan-PAUSED
// reconcile backstop, independent of whether any RUNNING row is stuck.
func TestScanOnce_RunsOrphanReconcile(t *testing.T) {
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil // no RUNNING rows — reconcile must still fire
		},
		supersedeOrphanN: 3,
	}
	w := New(DefaultConfig(), exec, nil, zerolog.Nop(), nil)
	require.NotNil(t, w)
	w.ctx = context.Background()

	w.scanOnce()
	assert.Equal(t, 1, exec.orphanReconcileCalls(),
		"each scan must run the orphan-PAUSED reconcile once")

	w.scanOnce()
	assert.Equal(t, 2, exec.orphanReconcileCalls(),
		"reconcile runs every scan (idempotent backstop)")
}

// TestScanOnce_OrphanReconcileErrorIsNonFatal — a reconcile error is logged,
// not propagated; the scan still completes.
func TestScanOnce_OrphanReconcileErrorIsNonFatal(t *testing.T) {
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil
		},
		supersedeOrphanErr: assert.AnError,
	}
	w := New(DefaultConfig(), exec, nil, zerolog.Nop(), nil)
	w.ctx = context.Background()
	assert.NotPanics(t, func() { w.scanOnce() })
	assert.Equal(t, 1, exec.orphanReconcileCalls())
}

// TestScanOnce_DetectsStuckExecution_FailActionMarksTerminal — the
// fail policy: stuck row gets execution.RecordFailure with class
// STUCK_EXECUTION AND task row flipped to FAILED with same class.
func TestScanOnce_DetectsStuckExecution_FailActionMarksTerminal(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	stuck := runningExec("e-stuck", "t1", "p1", now, 45*time.Minute)

	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{stuck}, nil
		},
	}
	tasks := newStubTaskRepo()
	tasks.setTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusRunning,
	})

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.scanOnce()

	failures := exec.failures()
	require.Len(t, failures, 1, "fail action must record exactly one execution failure")
	assert.Equal(t, "e-stuck", failures[0].id)
	assert.Equal(t, persistence.TaskFailureClassStuckExecution, failures[0].code,
		"failure class must be STUCK_EXECUTION so dashboards can group these correctly")
	assert.Contains(t, failures[0].message, "watchdog: execution stuck",
		"failure message must explain why the watchdog acted")

	updates := tasks.updateLog()
	require.Len(t, updates, 1, "task row must be flipped to terminal too — execution-only would leave the UI showing a stuck task")
	assert.Equal(t, persistence.TaskStatusFailed, updates[0].Status)
	require.NotNil(t, updates[0].LastErrorClass)
	assert.Equal(t, persistence.TaskFailureClassStuckExecution, *updates[0].LastErrorClass)
}

// TestScanOnce_DedupesRepeatedDetections — a long-stuck row must not
// re-fire the action on every tick; otherwise warn-mode floods logs
// and fail-mode would write FAILED twice (idempotent at the DB but
// noisy in metrics). Subsequent observations at the SAME updated_at
// are silenced.
func TestScanOnce_DedupesRepeatedDetections(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	stuck := runningExec("e-stuck", "t1", "p1", now, 45*time.Minute)

	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{stuck}, nil
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

	// Three back-to-back ticks — only the first should fire.
	w.scanOnce()
	w.scanOnce()
	w.scanOnce()

	assert.Len(t, exec.failures(), 1,
		"dedupe must hold — three observations of the same updated_at fire one action")
	assert.Len(t, tasks.updateLog(), 1)
}

// TestScanOnce_FreshCheckpointClearsDedupe — when a previously-stuck
// execution unsticks (updated_at advances) and then later sticks
// again, the watchdog must re-fire. Checkpoint-advance is the
// "this might be healthy now" signal; dedupe entry has to clear.
func TestScanOnce_FreshCheckpointClearsDedupe(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	stuck := runningExec("e-flaky", "t1", "p1", now, 45*time.Minute)

	tickRow := stuck
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{tickRow}, nil
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

	// Tick 1: stuck — fires.
	w.scanOnce()
	require.Len(t, exec.failures(), 1)

	// Reset the failure log and the task to RUNNING (simulate the
	// executor recovering — the task wasn't actually marked
	// FAILED; in production the row would be terminal here, but
	// the test hand-resets to exercise the dedupe-clear branch).
	exec.recordedFailures = nil
	tasks.setTask(&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})

	// Tick 2: row is fresh now (recently checkpointed).
	tickRow = runningExec("e-flaky", "t1", "p1", now, 1*time.Minute)
	w.scanOnce()
	assert.Empty(t, exec.failures(), "fresh checkpoint must not fire — the row is healthy")

	// Tick 3: row sticks again — different updated_at than tick 1,
	// dedupe must NOT silence this.
	tickRow = runningExec("e-flaky", "t1", "p1", now, 60*time.Minute)
	w.scanOnce()
	assert.Len(t, exec.failures(), 1,
		"re-stick after a healthy interval must fire again — dedupe is per (id, updated_at) pair, not per id")
}

// TestScanOnce_FreshExecutionsBelowThresholdAreIgnored — the safety
// case: a normally-progressing execution must never be flagged.
// Catches a regression where the threshold comparison flipped sign.
func TestScanOnce_FreshExecutionsBelowThresholdAreIgnored(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{
				runningExec("a", "t1", "p1", now, 0),
				runningExec("b", "t2", "p1", now, 5*time.Minute),
				runningExec("c", "t3", "p1", now, 29*time.Minute), // just under threshold
			}, nil
		},
	}
	cfg := DefaultConfig()
	cfg.Action = ActionFail
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, newStubTaskRepo(), zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.scanOnce()
	assert.Empty(t, exec.failures(), "everything under threshold — watchdog must be silent")
}

// TestScanOnce_PrunesSeenWhenExecutionLeavesRunning — the dedupe
// table must not accumulate forever. Once an execution is no longer
// in the RUNNING list (completed, failed, cancelled), its entry
// gets dropped.
func TestScanOnce_PrunesSeenWhenExecutionLeavesRunning(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	rows := []*persistence.Execution{
		runningExec("e-stuck", "t1", "p1", now, 45*time.Minute),
	}
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return rows, nil
		},
	}
	cfg := DefaultConfig()
	cfg.Action = ActionWarn
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, nil, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.scanOnce()
	assert.Len(t, w.seen, 1, "stuck row recorded in dedupe map")

	// Execution leaves RUNNING (e.g. operator cancelled it).
	rows = nil
	w.scanOnce()
	assert.Empty(t, w.seen, "dedupe entries for non-RUNNING executions must be pruned")
}

// TestNew_DefaultsAndCoercion — zero-valued config fields fall back
// to defaults, an unknown action coerces to the default (fail), and
// a nil execRepo returns nil. The default action flipped warn → fail
// on 2026-05-13 (see DefaultConfig); the coercion target tracks the
// default so a config typo still gets enforcement rather than
// silently dropping rows.
func TestNew_DefaultsAndCoercion(t *testing.T) {
	w := New(Config{Enabled: true, Action: "yolo"}, &stubExecRepo{}, nil, zerolog.Nop(), nil)
	require.NotNil(t, w)
	assert.Equal(t, ActionFail, w.cfg.Action, "unknown action must coerce to the default — config typos must not silently disable enforcement")
	assert.Equal(t, 60*time.Second, w.cfg.Interval, "zero Interval falls to default")
	assert.Equal(t, 30*time.Minute, w.cfg.StuckThreshold, "zero StuckThreshold falls to default")

	assert.Nil(t, New(Config{}, nil, nil, zerolog.Nop(), nil),
		"nil execRepo means there's nothing to scan — return nil so the daemon won't try to Start a useless watchdog")
}

// TestStart_DisabledIsNoop — a daemon can call Start unconditionally
// and the Enabled flag does the gating. No goroutine is spawned.
func TestStart_DisabledIsNoop(t *testing.T) {
	w := New(Config{Enabled: false}, &stubExecRepo{}, nil, zerolog.Nop(), nil)
	require.NotNil(t, w)
	require.NoError(t, w.Start())
	assert.False(t, w.started, "Start with Enabled=false must not flip started — Stop() relies on this state")
	require.NoError(t, w.Stop(), "Stop on a disabled watchdog must be a no-op")
}

// TestScanOnce_RecordFailureErrorLeavesRowWarned — when the fail
// action's DB write errors, the dedupe IS still recorded (a transient
// blip shouldn't cause the watchdog to spam the next tick) but the
// metric increment for "failed" must NOT fire because the row wasn't
// actually flipped.
func TestScanOnce_RecordFailureErrorLeavesRowWarned(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{runningExec("e", "t1", "p1", now, 45*time.Minute)}, nil
		},
		recordFailureErr: errors.New("db unavailable"),
	}
	cfg := DefaultConfig()
	cfg.Action = ActionFail
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, newStubTaskRepo(), zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	w.scanOnce()
	// No failures recorded (the stub errored out).
	assert.Empty(t, exec.failures())
	assert.Empty(t, w.seen, "failed enforcement must not mark the row seen — the next tick must retry the stuck execution")
	w.scanOnce()
	assert.Empty(t, exec.failures())
	assert.Empty(t, w.seen, "repeated enforcement errors should keep retrying instead of permanently silencing the stuck row")
}

func TestScanOnce_WithoutStartUsesBackgroundContext(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{
		listFunc: func(ctx context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			require.NotNil(t, ctx)
			return []*persistence.Execution{runningExec("e", "t1", "p1", now, 45*time.Minute)}, nil
		},
	}
	cfg := DefaultConfig()
	cfg.Action = ActionWarn
	cfg.StuckThreshold = 30 * time.Minute
	w := New(cfg, exec, nil, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	require.NotPanics(t, w.scanOnce)
}

// TestStartStop_EnabledLifecycle covers the happy-path Start+Stop pair
// and the "double Start returns an error" branch. The interval is set
// to a long duration so the ticker doesn't fire during the test — we
// only want to exercise the goroutine start/stop machinery.
func TestStartStop_EnabledLifecycle(t *testing.T) {
	exec := &stubExecRepo{
		listFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return nil, nil
		},
	}
	cfg := DefaultConfig()
	cfg.Interval = 24 * time.Hour
	cfg.Action = ActionWarn // bypass markFailed path so no taskRepo is required
	w := New(cfg, exec, nil, zerolog.Nop(), nil)
	require.NoError(t, w.Start())
	assert.True(t, w.started)

	// Double Start is rejected so a caller bug doesn't quietly spawn
	// two scan loops.
	require.Error(t, w.Start(), "second Start must error")

	require.NoError(t, w.Stop())
	assert.False(t, w.started)
	// Stop after Stop is a no-op.
	require.NoError(t, w.Stop())
}

// TestNewMetrics_RegistersOrNoopsOnNil covers the prometheus
// registration path — happy registration plus the nil-registry guard
// that lets tests opt out cleanly.
func TestNewMetrics_RegistersOrNoopsOnNil(t *testing.T) {
	assert.Nil(t, NewMetrics(nil), "nil registry returns nil — tests opt out by passing nil")

	reg := &countingRegisterer{}
	m := NewMetrics(reg)
	require.NotNil(t, m)
	assert.GreaterOrEqual(t, reg.registered, 2, "two counters must be registered (detected, failed)")
}

type countingRegisterer struct {
	registered int
}

func (c *countingRegisterer) Register(_ prometheus.Collector) error {
	c.registered++
	return nil
}

func (c *countingRegisterer) MustRegister(cs ...prometheus.Collector) {
	c.registered += len(cs)
}

func (c *countingRegisterer) Unregister(_ prometheus.Collector) bool { return true }

// TestMarkFailed_HonorsTerminalTaskRace verifies the defensive branch
// in markFailed that refuses to regress a task already in a terminal
// status. The race is real: the executor's own handleFailure might
// commit Status=FAILED a millisecond before the watchdog reads the
// row, and a second flip would clobber the executor's classification.
func TestMarkFailed_HonorsTerminalTaskRace(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	tasks := newStubTaskRepo()
	// Pre-seed a task already terminal — the watchdog must NOT
	// re-Update it (Update is what would regress LastErrorClass).
	tasks.setTask(&persistence.Task{
		ID:     "t-already-done",
		Status: persistence.TaskStatusCompleted,
	})

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }
	w.ctx = context.Background()

	err := w.markFailed(context.Background(), runningExec("e1", "t-already-done", "p1", now, 45*time.Minute), 45*time.Minute)
	require.NoError(t, err)
	assert.Empty(t, tasks.updates, "watchdog must not Update a task already terminal — would regress executor's classification")
	assert.Len(t, exec.failures(), 1, "the execution row IS marked failed even when the task was already terminal")
}

// TestMarkFailed_MissingTaskIDSkipsTaskWrite — a stuck execution with
// an empty TaskID (legacy data, or a backfill edge case) must still
// flip the execution row but skip the task-side update entirely.
// Without the guard the watchdog would call taskRepo.Get("") and
// likely crash or return ErrNotFound, neither of which we want
// surfacing as a watchdog error.
func TestMarkFailed_MissingTaskIDSkipsTaskWrite(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	tasks := newStubTaskRepo()

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	row := runningExec("e1", "", "p1", now, 45*time.Minute)
	require.NoError(t, w.markFailed(context.Background(), row, 45*time.Minute))
	assert.Len(t, exec.failures(), 1)
	assert.Empty(t, tasks.updates, "empty TaskID must short-circuit before taskRepo.Update")
}

// TestMarkFailed_TaskGetErrorSurfaces — when taskRepo.Get returns a
// non-nil error the caller surfaces it so the metric-increment +
// success-log path doesn't fire. The execution row IS already
// flipped before Get is called, mirroring real production semantics.
func TestMarkFailed_TaskGetErrorSurfaces(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	exec := &stubExecRepo{}
	tasks := newStubTaskRepo()
	tasks.getErr = errors.New("db blip")

	cfg := DefaultConfig()
	cfg.Action = ActionFail
	w := New(cfg, exec, tasks, zerolog.Nop(), nil)
	w.now = func() time.Time { return now }

	err := w.markFailed(context.Background(), runningExec("e1", "t1", "p1", now, 45*time.Minute), 45*time.Minute)
	require.Error(t, err, "Get error must surface")
	assert.Contains(t, err.Error(), "task Get")
	assert.Len(t, exec.failures(), 1, "execution row was flipped before the Get error")
}

// TestScanOnce_ApprovalTimeoutSweep — tasks parked in AWAITING_APPROVAL past
// cfg.ApprovalTimeout are cancelled; fresher ones are left; disabled = no-op.
func TestScanOnce_ApprovalTimeoutSweep(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	mk := func() (*stubExecRepo, *stubTaskRepo) {
		exec := &stubExecRepo{listFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) { return nil, nil }}
		tasks := &stubTaskRepo{listResult: []*persistence.Task{
			{ID: "stale", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-100 * time.Hour)},
			{ID: "fresh", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, UpdatedAt: now.Add(-1 * time.Hour)},
		}}
		return exec, tasks
	}

	t.Run("cancels past-timeout, leaves fresh", func(t *testing.T) {
		exec, tasks := mk()
		cfg := DefaultConfig()
		cfg.ApprovalTimeout = 96 * time.Hour
		w := New(cfg, exec, tasks, zerolog.Nop(), nil)
		w.now = func() time.Time { return now }
		w.ctx = context.Background()
		w.scanOnce()
		got := tasks.cancelledIDs()
		if got["stale"] != persistence.TaskStatusCancelled {
			t.Errorf("stale approval not cancelled: %v", got)
		}
		if _, ok := got["fresh"]; ok {
			t.Errorf("fresh approval must NOT be cancelled: %v", got)
		}
	})

	t.Run("disabled is a no-op", func(t *testing.T) {
		exec, tasks := mk()
		cfg := DefaultConfig()
		cfg.ApprovalTimeout = 0 // disabled
		w := New(cfg, exec, tasks, zerolog.Nop(), nil)
		w.now = func() time.Time { return now }
		w.ctx = context.Background()
		w.scanOnce()
		if len(tasks.cancelledIDs()) != 0 {
			t.Errorf("disabled sweep must cancel nothing, got %v", tasks.cancelledIDs())
		}
	})
}
