package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// cpcScannerCov_leaderGate is a settable CPCLeaderGate.
type cpcScannerCov_leaderGate struct{ leader bool }

func (g *cpcScannerCov_leaderGate) IsLeader() bool { return g.leader }

// cpcScannerCov_claimErrRepo embeds mockCPCRepo but fails
// ClaimTimedOut so the tick error branch runs.
type cpcScannerCov_claimErrRepo struct {
	*mockCPCRepo
}

func (r *cpcScannerCov_claimErrRepo) ClaimTimedOut(_ context.Context, _ time.Time, _ int) ([]*persistence.CrossProjectCall, error) {
	return nil, errors.New("claim query failed")
}

// TestCPCScannerCov_SetLeaderGateNilSafe covers the nil-receiver
// guard of SetLeaderGate.
func TestCPCScannerCov_SetLeaderGateNilSafe(t *testing.T) {
	var s *CPCTimeoutScanner
	s.SetLeaderGate(&cpcScannerCov_leaderGate{leader: true}) // must not panic
}

// TestCPCScannerCov_NilScannerRunIsNoop covers the nil-receiver
// guard of Run.
func TestCPCScannerCov_NilScannerRunIsNoop(t *testing.T) {
	var s *CPCTimeoutScanner
	if err := s.Run(context.Background()); err != nil {
		t.Errorf("nil scanner Run should return nil, got %v", err)
	}
}

// TestCPCScannerCov_NotLeaderSkipsTick covers the leader-gate
// branch: a non-leader instance does no scan work.
func TestCPCScannerCov_NotLeaderSkipsTick(t *testing.T) {
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	scanner.SetLeaderGate(&cpcScannerCov_leaderGate{leader: false})

	past := time.Now().Add(-time.Minute)
	cpc.rows["ccp_x"] = &persistence.CrossProjectCall{
		ID: "ccp_x", Status: persistence.CPCStatusRunning, TimeoutAt: &past,
	}
	scanner.tick(context.Background())
	// Row should NOT have been claimed (still running) because the
	// gate short-circuited.
	if cpc.rows["ccp_x"].Status != persistence.CPCStatusRunning {
		t.Errorf("non-leader tick should not claim rows, status = %q", cpc.rows["ccp_x"].Status)
	}
}

// TestCPCScannerCov_LeaderRunsTick is the leader=true complement so
// the IsLeader-true arm is covered.
func TestCPCScannerCov_LeaderRunsTick(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	scanner.SetLeaderGate(&cpcScannerCov_leaderGate{leader: true})

	past := time.Now().Add(-time.Minute)
	cpc.rows["ccp_y"] = &persistence.CrossProjectCall{
		ID: "ccp_y", CallerTaskID: "tc", Status: persistence.CPCStatusRunning, TimeoutAt: &past,
	}
	tr.AddTask(&persistence.Task{ID: "tc", Status: persistence.TaskStatusWaitingForChildren})
	scanner.tick(context.Background())
	if cpc.rows["ccp_y"].Status != persistence.CPCStatusTimedOut {
		t.Errorf("leader tick should claim the expired row, status = %q", cpc.rows["ccp_y"].Status)
	}
}

// TestCPCScannerCov_ClaimError covers the ClaimTimedOut error branch
// in tick (logged + return, no panic).
func TestCPCScannerCov_ClaimError(t *testing.T) {
	cpc := &cpcScannerCov_claimErrRepo{mockCPCRepo: newMockCPCRepo()}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	scanner.tick(context.Background()) // must not panic
}

// TestCPCScannerCov_ProcessOneNilCPC covers the nil-row guard.
func TestCPCScannerCov_ProcessOneNilCPC(t *testing.T) {
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	scanner.processOne(context.Background(), nil) // must not panic
}

// TestCPCScannerCov_ProcessOneWithErrorMessage exercises processOne
// with metrics wired + an ErrorMessage set on the row + a resolvable
// caller execution, covering the errMsg + metrics + emit branches.
func TestCPCScannerCov_ProcessOneWithErrorMessage(t *testing.T) {
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	audit := &stubAdminAuditRepo{}
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.livePub = pub
	e.adminAuditRepo = audit
	if er, ok := e.execRepo.(*MockExecRepo); ok {
		_ = er.Create(context.Background(), &persistence.Execution{ID: "ex", TaskID: "caller"})
	}
	tr.AddTask(&persistence.Task{ID: "caller", Status: persistence.TaskStatusWaitingForChildren})

	msg := "deadline hit"
	row := &persistence.CrossProjectCall{
		ID:            "ccp_em",
		CallerTaskID:  "caller",
		CallerProject: "marketing",
		CalleeProject: "architect",
		Status:        persistence.CPCStatusTimedOut,
		CreatedAt:     time.Now().Add(-time.Minute),
		ErrorMessage:  &msg,
	}
	scanner := NewCPCTimeoutScanner(e)
	scanner.processOne(context.Background(), row)

	caller, _ := tr.Get(context.Background(), "caller")
	if caller.Status != persistence.TaskStatusQueued {
		t.Errorf("caller should be re-queued, got %q", caller.Status)
	}
}

// TestCPCScannerCov_CascadeCancelGuards covers the cascade nil-guard
// and already-terminal short-circuit.
func TestCPCScannerCov_CascadeCancelGuards(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	ctx := context.Background()

	// nil callee task id → guard returns early.
	scanner.cascadeCancelCallee(ctx, &persistence.CrossProjectCall{ID: "c1"})

	// callee task already terminal → idempotent skip.
	doneID := "callee-done"
	tr.AddTask(&persistence.Task{ID: doneID, Status: persistence.TaskStatusCompleted})
	scanner.cascadeCancelCallee(ctx, &persistence.CrossProjectCall{ID: "c2", CalleeTaskID: &doneID})
	got, _ := tr.Get(ctx, doneID)
	if got.Status != persistence.TaskStatusCompleted {
		t.Errorf("terminal callee must not be re-cancelled, got %q", got.Status)
	}

	// callee task not found → guard returns early.
	missing := "no-such-callee"
	scanner.cascadeCancelCallee(ctx, &persistence.CrossProjectCall{ID: "c3", CalleeTaskID: &missing})
}

// TestCPCScannerCov_WakeCallerGuards covers wakeCallerForTimeout's
// not-found and not-waiting branches.
func TestCPCScannerCov_WakeCallerGuards(t *testing.T) {
	cpc := newMockCPCRepo()
	e, tr := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	scanner := NewCPCTimeoutScanner(e)
	ctx := context.Background()

	// Caller task not found → guard returns early (no panic).
	scanner.wakeCallerForTimeout(ctx, &persistence.CrossProjectCall{ID: "c1", CallerTaskID: "ghost"})

	// Caller task not in WAITING_FOR_CHILDREN → left untouched.
	tr.AddTask(&persistence.Task{ID: "caller-running", Status: persistence.TaskStatusRunning})
	scanner.wakeCallerForTimeout(ctx, &persistence.CrossProjectCall{ID: "c2", CallerTaskID: "caller-running"})
	got, _ := tr.Get(ctx, "caller-running")
	if got.Status != persistence.TaskStatusRunning {
		t.Errorf("non-waiting caller must not be re-queued, got %q", got.Status)
	}
}
