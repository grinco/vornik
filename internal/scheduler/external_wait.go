package scheduler

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Phase 30 — re-execution trigger #4 (LLD §7.1):
//
//   AWAITING_EXTERNAL + expected_by passed → re-queue.
//
// Implemented as a periodic scan inside the scheduler. Tasks
// without expected_by stay parked indefinitely until something
// else (operator answer, webhook event, or a future fixed
// expected_by) re-queues them.

// ExternalWaitMonitor scans tasks in AWAITING_EXTERNAL and re-
// queues those whose expected_by deadline has passed. Runs as a
// goroutine started by Scheduler.Start.
//
// Why a separate goroutine vs. piggybacking on the lease-recovery
// scan: lease recovery is "this task should be running but its
// container vanished". External-wait is "this task is parked
// pending a real-world event with a stored deadline". Different
// invariants, different cadence (recovery polls every 30s by
// default; external-wait can poll every 60s without missing
// anything because expected_by is wall-clock, not lease-clock).
type ExternalWaitMonitor struct {
	// taskRepo is the wider persistence.TaskRepository so we can
	// List() without extending the scheduler's narrow internal
	// interface. Same concrete type as persistRepo in production.
	taskRepo    persistence.TaskRepository
	persistRepo persistence.TaskRepository // for TransitionConditional
	msgRepo     persistence.TaskMessageRepository
	wake        WakeSource
	logger      zerolog.Logger
	interval    time.Duration

	// leaderGate gates the scan on the elected leader. nil → run
	// every tick (single-process default). See
	// https://docs.vornik.io §3.
	leaderGate LeaderGate

	stopMu sync.Mutex
	stopCh chan struct{}
	doneCh chan struct{}
}

// LeaderGate is the narrow contract the monitor consults before
// each scan in multi-instance deployments. Defined locally so
// scheduler doesn't pull internal/leaderelection into its
// dependency set; *leaderelection.Elector satisfies structurally.
type LeaderGate interface {
	IsLeader() bool
}

// WakeSource is the narrow interface ExternalWaitMonitor uses to
// nudge the scheduler after a re-queue. *Scheduler implements it
// through Wake().
type WakeSource interface {
	Wake()
}

// NewExternalWaitMonitor builds the monitor. interval == 0 picks
// 60s. nil msgRepo skips the audit-message write but still does
// the transition (keeps tests cheap).
func NewExternalWaitMonitor(
	taskRepo persistence.TaskRepository,
	persistRepo persistence.TaskRepository,
	msgRepo persistence.TaskMessageRepository,
	wake WakeSource,
	interval time.Duration,
	logger zerolog.Logger,
) *ExternalWaitMonitor {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &ExternalWaitMonitor{
		taskRepo:    taskRepo,
		persistRepo: persistRepo,
		msgRepo:     msgRepo,
		wake:        wake,
		logger:      logger,
		interval:    interval,
	}
}

// SetLeaderGate attaches the leader gate. Safe to call before
// Start; calling after Start is racy and not supported.
func (m *ExternalWaitMonitor) SetLeaderGate(g LeaderGate) {
	if m == nil {
		return
	}
	m.leaderGate = g
}

// Start launches the scan loop. Idempotent; second call is a no-op.
func (m *ExternalWaitMonitor) Start(ctx context.Context) {
	m.stopMu.Lock()
	if m.stopCh != nil {
		m.stopMu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	m.stopCh = stop
	m.doneCh = done
	m.stopMu.Unlock()

	// Capture stop/done locally — Stop nils the struct fields
	// under the mutex, so reading m.stopCh inside the select on
	// every iteration was racy: the goroutine could wake up
	// after Stop nilled the field, observe a nil channel, and
	// the closed-channel signal would be lost (reads from a nil
	// channel block forever).
	go func() {
		defer close(done)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				if m.leaderGate != nil && !m.leaderGate.IsLeader() {
					continue
				}
				m.scanOnce(ctx)
			}
		}
	}()
	m.logger.Info().Dur("interval", m.interval).Msg("external_wait monitor started")
}

// Stop halts the scan loop and waits for it to exit.
func (m *ExternalWaitMonitor) Stop() {
	m.stopMu.Lock()
	if m.stopCh == nil {
		m.stopMu.Unlock()
		return
	}
	close(m.stopCh)
	done := m.doneCh
	m.stopCh = nil
	m.doneCh = nil
	m.stopMu.Unlock()
	if done != nil {
		<-done
	}
}

// scanOnce reads AWAITING_EXTERNAL tasks with an expected_by in
// the past and re-queues them. Best-effort — DB errors log and
// retry next tick. Bounded at 100/scan so a backlog doesn't
// overwhelm the next leasing burst.
//
// Phase 31 — also runs the closure-request auto-close grace
// scan: tasks that have stayed COMPLETED with a closure_request
// message older than the grace window get auto-closed by the
// system (closed_by='system_after_grace_period').
func (m *ExternalWaitMonitor) scanOnce(ctx context.Context) {
	tasks, err := m.findExpired(ctx, 100)
	if err != nil {
		m.logger.Warn().Err(err).Msg("external_wait scan failed")
	} else {
		for _, t := range tasks {
			m.requeue(ctx, t)
		}
	}
	m.scanClosureGrace(ctx)
}

// closureGracePeriod is how long a closure_request can sit in
// COMPLETED status before the system auto-closes the task. 14
// days per LLD §11 row 9.
const closureGracePeriod = 14 * 24 * time.Hour

// scanClosureGrace looks for COMPLETED tasks whose lead emitted a
// closure_request more than closureGracePeriod ago and the
// operator never explicitly closed. Auto-closes them with
// closed_by='system_after_grace_period'.
//
// Cheap implementation: list COMPLETED tasks (limit 500,
// updated_at filter not applied at the SQL level — production
// fleet stays under that ceiling for the foreseeable future).
// For each, fetch its messages, check whether the most recent
// closure_request is older than the grace window with no system-
// kind close after it, and auto-close.
func (m *ExternalWaitMonitor) scanClosureGrace(ctx context.Context) {
	if m.taskRepo == nil || m.persistRepo == nil || m.msgRepo == nil {
		return
	}
	completed := persistence.TaskStatusCompleted
	// 2026-05-29 audit fix: filter at the SQL level on
	// updated_at < cutoff so the scan only fetches stale-enough
	// tasks. Pre-fix the scan pulled every COMPLETED task each
	// tick (PageSize 500, no SQL filter) and rejected most in-
	// memory via t.UpdatedAt.After(cutoff) — fine at small scale
	// but a per-tick latency hazard for the adjacent external_wait
	// re-queue path on a busy deployment.
	cutoff := time.Now().UTC().Add(-closureGracePeriod)
	tasks, err := m.taskRepo.List(ctx, persistence.TaskFilter{
		Status:        &completed,
		UpdatedBefore: &cutoff,
		PageSize:      500,
	})
	if err != nil {
		m.logger.Warn().Err(err).Msg("closure_grace: list COMPLETED failed")
		return
	}
	for _, t := range tasks {
		// SQL-level filter already excluded tasks updated within
		// the grace window — no in-memory re-check needed.
		msgs, err := m.msgRepo.List(ctx, persistence.TaskMessageFilter{
			TaskID:       t.ID,
			MessageKinds: []string{persistence.TaskMessageKindClosureRequest},
			Limit:        5,
		})
		if err != nil || len(msgs) == 0 {
			continue
		}
		// Most recent closure_request — check it's older than
		// the grace window AND nothing operator-actioned has
		// happened since (a directive after closure_request
		// would have re-queued, status wouldn't still be
		// COMPLETED — but verify defensively).
		latest := msgs[len(msgs)-1]
		if latest.CreatedAt.After(cutoff) {
			continue
		}
		closer := "system_after_grace_period"
		ok, err := m.persistRepo.TransitionConditional(ctx, t.ID,
			[]persistence.TaskStatus{persistence.TaskStatusCompleted},
			persistence.TaskStatusClosed,
			persistence.TransitionOpts{
				ClosedBy:       &closer,
				SetClosedAtNow: true,
			})
		if err != nil || !ok {
			continue
		}
		// Audit message recording the auto-close.
		meta, _ := json.Marshal(map[string]any{
			"kind":     "task_closed",
			"closedBy": closer,
			"reason":   "closure_request grace period elapsed",
		})
		_ = m.msgRepo.Insert(ctx, &persistence.TaskMessage{
			TaskID:      t.ID,
			AuthorKind:  persistence.TaskMessageAuthorSystem,
			MessageKind: persistence.TaskMessageKindSystem,
			Content:     "auto-closed after 14d grace period",
			Metadata:    meta,
			CreatedAt:   time.Now().UTC(),
		})
		m.logger.Info().
			Str("task_id", t.ID).
			Time("closure_request_at", latest.CreatedAt).
			Msg("closure_grace: auto-closed task after 14d grace period")
	}
}

// findExpired returns AWAITING_EXTERNAL tasks whose expected_by
// has passed. Pulls via the standard List filter + in-memory
// time check (no Status+ExpectedBy compound filter on the repo
// today; the partial index introduced in v25 keeps the table
// scan cheap).
func (m *ExternalWaitMonitor) findExpired(ctx context.Context, limit int) ([]*persistence.Task, error) {
	if m.taskRepo == nil {
		return nil, nil
	}
	awaiting := persistence.TaskStatusAwaitingExternal
	tasks, err := m.taskRepo.List(ctx, persistence.TaskFilter{
		Status:   &awaiting,
		PageSize: limit,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]*persistence.Task, 0, len(tasks))
	for _, t := range tasks {
		if t.ExpectedBy != nil && !t.ExpectedBy.After(now) {
			out = append(out, t)
		}
	}
	return out, nil
}

// requeue does the per-task work: write a system message
// recording the deadline reached, transition AWAITING_EXTERNAL →
// QUEUED, wake the scheduler.
func (m *ExternalWaitMonitor) requeue(ctx context.Context, t *persistence.Task) {
	// Validate transition once for symmetry with API handlers,
	// even though the conditional UPDATE below also gates on
	// status. Catches a stale read here cheaply.
	if err := ValidateTransition(t.Status, persistence.TaskStatusQueued, TriggerExternalDeadline); err != nil {
		return
	}
	if m.msgRepo != nil {
		meta, _ := json.Marshal(map[string]any{
			"kind":        "external_deadline_reached",
			"expected_by": t.ExpectedBy,
		})
		body := "external deadline reached; resuming task"
		msg := &persistence.TaskMessage{
			TaskID:      t.ID,
			AuthorKind:  persistence.TaskMessageAuthorSystem,
			MessageKind: persistence.TaskMessageKindSystem,
			Content:     body,
			Metadata:    meta,
			CreatedAt:   time.Now().UTC(),
		}
		if err := m.msgRepo.Insert(ctx, msg); err != nil {
			m.logger.Warn().Err(err).Str("task_id", t.ID).Msg("external_wait: insert system message failed")
		}
	}
	if m.persistRepo == nil {
		return
	}
	ok, err := m.persistRepo.TransitionConditional(ctx, t.ID,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingExternal},
		persistence.TaskStatusQueued,
		persistence.TransitionOpts{ClearLease: true},
	)
	if err != nil {
		m.logger.Warn().Err(err).Str("task_id", t.ID).Msg("external_wait: transition failed")
		return
	}
	if !ok {
		// Lost the race — task drifted (operator answered, etc.).
		// Not an error.
		return
	}
	m.logger.Info().
		Str("task_id", t.ID).
		Time("expected_by", *t.ExpectedBy).
		Msg("external_wait: deadline reached, task re-queued")
	if m.wake != nil {
		m.wake.Wake()
	}
}
