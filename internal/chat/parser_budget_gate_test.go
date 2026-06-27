package chat

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// fakeGateUsageRepo satisfies budget.Repo with zero committed spend so the
// reservation decision is driven entirely by fakeGateReservRepo.
type fakeGateUsageRepo struct{}

func (fakeGateUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

// fakeGateReservRepo records Reserve calls and returns a canned block decision
// so the chat gate's allow/refuse behavior can be exercised without a DB.
type fakeGateReservRepo struct {
	blocked      bool
	reserveCalls int
}

func (f *fakeGateReservRepo) Reserve(_ context.Context, _ persistence.ReserveRequest) (persistence.ReserveResult, error) {
	f.reserveCalls++
	if f.blocked {
		return persistence.ReserveResult{Blocked: true, Period: "daily"}, nil
	}
	return persistence.ReserveResult{Reserved: true}, nil
}
func (f *fakeGateReservRepo) SettleByTask(_ context.Context, _ string, _ time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeGateReservRepo) SweepTerminalAndStale(_ context.Context, _, _ time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeGateReservRepo) UnsettledSumByProject(_ context.Context, _ string) (float64, error) {
	return 0, nil
}

func gateForProject(res *fakeGateReservRepo) *BudgetGate {
	return &BudgetGate{
		Reservations: res,
		Usage:        fakeGateUsageRepo{},
		Project:      &registry.Project{ID: "p1", Budget: registry.ProjectBudget{DailyHardUSD: 50}},
	}
}

// TestExecuteCreateTask_BudgetGateBlocks — a blocked reservation refuses the
// create and NO task row is written (the chat-path hard-cap gate,
// trading-hardening §1).
func TestExecuteCreateTask_BudgetGateBlocks(t *testing.T) {
	created := false
	taskRepo := &mocks.MockTaskRepository{CreateFunc: func(_ context.Context, _ *persistence.Task) error {
		created = true
		return nil
	}}
	res := &fakeGateReservRepo{blocked: true}

	out, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell"},
		taskRepo, nil, gateForProject(res))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Success {
		t.Fatalf("blocked reservation must yield an unsuccessful result")
	}
	if created {
		t.Fatalf("task must NOT be created when the budget reservation blocks")
	}
	if res.reserveCalls != 1 {
		t.Fatalf("reserve calls = %d, want 1", res.reserveCalls)
	}
}

// TestExecuteCreateTask_BudgetGateAllows — an allowed reservation lets the
// create proceed.
func TestExecuteCreateTask_BudgetGateAllows(t *testing.T) {
	created := false
	taskRepo := &mocks.MockTaskRepository{CreateFunc: func(_ context.Context, _ *persistence.Task) error {
		created = true
		return nil
	}}
	res := &fakeGateReservRepo{blocked: false}

	out, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell"},
		taskRepo, nil, gateForProject(res))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Success {
		t.Fatalf("allowed reservation should create the task; got: %s", out.Message)
	}
	if !created {
		t.Fatalf("task should be created when the reservation allows")
	}
}

// TestExecuteCreateTask_NilGate — without a gate the create proceeds
// unchanged (legacy callers / no budget wiring).
func TestExecuteCreateTask_NilGate(t *testing.T) {
	created := false
	taskRepo := &mocks.MockTaskRepository{CreateFunc: func(_ context.Context, _ *persistence.Task) error {
		created = true
		return nil
	}}
	out, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell"},
		taskRepo, nil, nil)
	if err != nil || !out.Success || !created {
		t.Fatalf("nil gate must not block creation: err=%v success=%v created=%v", err, out.Success, created)
	}
}
