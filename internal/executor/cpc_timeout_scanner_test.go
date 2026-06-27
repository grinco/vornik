package executor

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// TestCPCTimeoutScanner_TickResolvesExpired drives a single
// tick against a seeded repo and asserts every step of the
// downstream pipeline fires: status flips, audit row lands,
// live event emits on the caller's stream, caller task wakes
// from WAITING_FOR_CHILDREN.
func TestCPCTimeoutScanner_TickResolvesExpired(t *testing.T) {
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	audit := &stubAdminAuditRepo{}
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.livePub = pub
	e.adminAuditRepo = audit

	// Seed an executor + execution row for the caller so the
	// scanner can resolve the caller's stream.
	if er, ok := e.execRepo.(*MockExecRepo); ok {
		_ = er.Create(context.Background(), &persistence.Execution{
			ID:        "exec-caller",
			TaskID:    "task-caller",
			ProjectID: "marketing",
		})
	}
	tr.AddTask(&persistence.Task{
		ID:     "task-caller",
		Status: persistence.TaskStatusWaitingForChildren,
	})

	// Seed one expired CPC + one fresh one — only the expired
	// row should claim.
	past := time.Now().Add(-1 * time.Minute)
	future := time.Now().Add(10 * time.Minute)
	cpc.rows["ccp_expired"] = &persistence.CrossProjectCall{
		ID:             "ccp_expired",
		CallerTaskID:   "task-caller",
		CallerProject:  "marketing",
		CalleeProject:  "architect",
		CalleeWorkflow: "produce-spec",
		Status:         persistence.CPCStatusRunning,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		TimeoutAt:      &past,
	}
	cpc.rows["ccp_fresh"] = &persistence.CrossProjectCall{
		ID:            "ccp_fresh",
		CallerTaskID:  "task-caller",
		CallerProject: "marketing",
		CalleeProject: "architect",
		Status:        persistence.CPCStatusRunning,
		CreatedAt:     time.Now(),
		TimeoutAt:     &future,
	}

	scanner := NewCPCTimeoutScanner(e)
	if scanner == nil {
		t.Fatal("scanner should be non-nil with cpcRepo wired")
	}
	scanner.tick(context.Background())

	// CPC row flipped.
	if cpc.rows["ccp_expired"].Status != persistence.CPCStatusTimedOut {
		t.Errorf("expired row not flipped, status = %q", cpc.rows["ccp_expired"].Status)
	}
	// Fresh row untouched.
	if cpc.rows["ccp_fresh"].Status != persistence.CPCStatusRunning {
		t.Errorf("fresh row flipped, status = %q", cpc.rows["ccp_fresh"].Status)
	}
	// Live event emitted on caller's stream.
	events := pub.byKind(livepubsub.KindCrossProjectCallResolved)
	if len(events) != 1 {
		t.Fatalf("expected 1 resolved event for the expired row, got %d", len(events))
	}
	payload, _ := events[0].Payload.(livepubsub.CrossProjectCallResolvedPayload)
	if payload.Status != string(persistence.CPCStatusTimedOut) {
		t.Errorf("event status = %q, want timed_out", payload.Status)
	}
	// Audit row written.
	if rows := audit.byAction(auditActionCPCResolve); len(rows) != 1 {
		t.Errorf("expected 1 resolve audit row, got %d", len(rows))
	}
	// Caller task re-queued.
	caller, _ := tr.Get(context.Background(), "task-caller")
	if caller.Status != persistence.TaskStatusQueued {
		t.Errorf("caller status = %q, want QUEUED (woken from WAITING_FOR_CHILDREN)", caller.Status)
	}
}

// TestCPCTimeoutScanner_NilExecutorIsNoop guards the "feature
// off" path: an executor without a cpcRepo wired returns nil
// from NewCPCTimeoutScanner; the daemon's Run loop should
// tolerate a nil scanner without panicking.
func TestCPCTimeoutScanner_NilExecutorIsNoop(t *testing.T) {
	if s := NewCPCTimeoutScanner(nil); s != nil {
		t.Errorf("expected nil scanner for nil executor, got %+v", s)
	}
	// Empty executor (no cpcRepo) also returns nil.
	e, _, _, _, _ := setup()
	if s := NewCPCTimeoutScanner(e); s != nil {
		t.Errorf("expected nil scanner when cpcRepo is unwired")
	}
}

// TestCPCTimeoutScanner_TickWithoutRowsIsClean asserts a tick
// against a repo with no expired rows does no work + emits
// no events. Important because the scanner ticks every 30s
// regardless of whether there's anything to do.
func TestCPCTimeoutScanner_TickWithoutRowsIsClean(t *testing.T) {
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	audit := &stubAdminAuditRepo{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.livePub = pub
	e.adminAuditRepo = audit

	scanner := NewCPCTimeoutScanner(e)
	scanner.tick(context.Background())

	if len(pub.events) != 0 {
		t.Errorf("expected no events, got %d", len(pub.events))
	}
	if len(audit.rows) != 0 {
		t.Errorf("expected no audit rows, got %d", len(audit.rows))
	}
}

// TestCPCTimeoutScanner_CascadeCancelsCallee asserts the
// cancel_on_timeout=true path cascades the cancel to the
// callee task. Closes the LLD §8.1 "expensive workflows
// don't want to keep burning budget after the caller has
// moved on" use case.
func TestCPCTimeoutScanner_CascadeCancelsCallee(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)

	past := time.Now().Add(-1 * time.Minute)
	calleeID := "task-callee"
	cpc.rows["ccp_cascade"] = &persistence.CrossProjectCall{
		ID:              "ccp_cascade",
		CallerTaskID:    "task-caller",
		CallerProject:   "marketing",
		CalleeProject:   "architect",
		Status:          persistence.CPCStatusRunning,
		CalleeTaskID:    &calleeID,
		TimeoutAt:       &past,
		CancelOnTimeout: true,
	}
	tr.AddTask(&persistence.Task{ID: "task-caller", Status: persistence.TaskStatusWaitingForChildren})
	tr.AddTask(&persistence.Task{ID: calleeID, Status: persistence.TaskStatusRunning})

	scanner := NewCPCTimeoutScanner(e)
	scanner.tick(context.Background())

	if cpc.rows["ccp_cascade"].Status != persistence.CPCStatusTimedOut {
		t.Errorf("CPC status = %q, want timed_out", cpc.rows["ccp_cascade"].Status)
	}
	caller, _ := tr.Get(context.Background(), "task-caller")
	if caller.Status != persistence.TaskStatusQueued {
		t.Errorf("caller status = %q, want QUEUED", caller.Status)
	}
	callee, _ := tr.Get(context.Background(), calleeID)
	if callee.Status != persistence.TaskStatusCancelled {
		t.Errorf("callee status = %q, want CANCELLED (cascade fired)", callee.Status)
	}
}

// TestCPCTimeoutScanner_NoCascadeWhenFlagOff pins the
// default behaviour: cancel_on_timeout=false leaves the
// callee running. LLD §8.1 default contract.
func TestCPCTimeoutScanner_NoCascadeWhenFlagOff(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)

	past := time.Now().Add(-1 * time.Minute)
	calleeID := "task-callee"
	cpc.rows["ccp_keep"] = &persistence.CrossProjectCall{
		ID:              "ccp_keep",
		CallerTaskID:    "task-caller",
		CallerProject:   "marketing",
		CalleeProject:   "architect",
		Status:          persistence.CPCStatusRunning,
		CalleeTaskID:    &calleeID,
		TimeoutAt:       &past,
		CancelOnTimeout: false,
	}
	tr.AddTask(&persistence.Task{ID: "task-caller", Status: persistence.TaskStatusWaitingForChildren})
	tr.AddTask(&persistence.Task{ID: calleeID, Status: persistence.TaskStatusRunning})

	scanner := NewCPCTimeoutScanner(e)
	scanner.tick(context.Background())

	callee, _ := tr.Get(context.Background(), calleeID)
	if callee.Status != persistence.TaskStatusRunning {
		t.Errorf("callee status = %q, want RUNNING preserved (no cascade)", callee.Status)
	}
}

// TestEmitCrossProjectCallReceived_OnceOnly asserts the
// inbound-edge live event fires exactly once per execution.
// A second call for the same execution must NOT re-emit
// (retry / scheduler-recovery scenario).
func TestEmitCrossProjectCallReceived_OnceOnly(t *testing.T) {
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.livePub = pub
	if e.callReceivedDedup == nil {
		e.callReceivedDedup = newLiveCallReceivedTracker()
	}

	cpcID := "ccp_in"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{
		ID:             cpcID,
		CallerTaskID:   "task-marketing",
		CallerStepID:   "step-handoff",
		CallerProject:  "marketing",
		ExpectedSchema: "spec_envelope.v1",
		Status:         persistence.CPCStatusRunning,
	}
	task := &persistence.Task{ID: "task-callee", CrossProjectCallID: &cpcID}
	exec := &persistence.Execution{ID: "exec-callee"}

	// First call → emits.
	e.emitCrossProjectCallReceivedIfCallee(context.Background(), task, exec)
	if got := pub.byKind(livepubsub.KindCrossProjectCallReceived); len(got) != 1 {
		t.Fatalf("first call: expected 1 event, got %d", len(got))
	}
	// Second call → dedup'd.
	e.emitCrossProjectCallReceivedIfCallee(context.Background(), task, exec)
	if got := pub.byKind(livepubsub.KindCrossProjectCallReceived); len(got) != 1 {
		t.Errorf("dedup failed: got %d events on second call", len(got))
	}
}

// TestEmitCrossProjectCallReceived_NotACalleeIsNoop covers
// the common case — the executing task isn't a CPC callee,
// so the helper short-circuits without touching the repo.
func TestEmitCrossProjectCallReceived_NotACalleeIsNoop(t *testing.T) {
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.livePub = pub
	e.callReceivedDedup = newLiveCallReceivedTracker()

	// No CrossProjectCallID → no-op.
	task := &persistence.Task{ID: "regular-task"}
	exec := &persistence.Execution{ID: "exec-1"}
	e.emitCrossProjectCallReceivedIfCallee(context.Background(), task, exec)

	if len(pub.events) != 0 {
		t.Errorf("expected no events for non-callee task, got %d", len(pub.events))
	}
}

// TestCPCTimeoutScanner_RunRespectsContextCancel asserts the
// goroutine exits cleanly on ctx cancel — important so the
// daemon shutdown path doesn't leak the scanner.
func TestCPCTimeoutScanner_RunRespectsContextCancel(t *testing.T) {
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	scanner.interval = 10 * time.Millisecond // tighter for the test

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scanner.Run(ctx) }()

	time.Sleep(30 * time.Millisecond) // let it tick at least once
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Run should return ctx.Err() on cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("scanner didn't exit within 1s of ctx cancel")
	}
}
