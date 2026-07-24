package executor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// captureBudgetNotifier records NotifyBudgetBreach calls.
type captureBudgetNotifier struct {
	mu    sync.Mutex
	calls []struct {
		project, level, period string
	}
}

func (c *captureBudgetNotifier) NotifyBudgetBreach(_ context.Context, projectID, level, period string, _ budget.Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, struct{ project, level, period string }{projectID, level, period})
}

func (c *captureBudgetNotifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// newBudgetGateExecutor wires an Executor for the per-task governor gate tests.
func newBudgetGateExecutor(usage *stubLLMUsageRepo, msg *fakeMessageRepo, tr *fakeTaskRepo, notifier budget.Notifier) *Executor {
	return &Executor{
		llmUsageRepo:    usage,
		taskMessageRepo: msg,
		persistTaskRepo: tr,
		budgetNotifier:  notifier,
		metrics:         NewMetrics(prometheus.NewRegistry()),
		logger:          zerolog.Nop(),
	}
}

func budgetProject(def float64) *registry.Project {
	return &registry.Project{ID: "p1", Budget: registry.ProjectBudget{DefaultTaskBudgetUSD: def}}
}

func usageWithSpend(cost float64) *stubLLMUsageRepo {
	return &stubLLMUsageRepo{rows: []*persistence.TaskLLMUsage{{CostUSD: cost}}}
}

func lastCheckpoint(t *testing.T, msg *fakeMessageRepo) map[string]any {
	t.Helper()
	if len(msg.inserted) == 0 {
		t.Fatalf("no checkpoint message inserted")
	}
	var meta map[string]any
	if err := json.Unmarshal(msg.inserted[len(msg.inserted)-1].Metadata, &meta); err != nil {
		t.Fatalf("unmarshal checkpoint metadata: %v", err)
	}
	return meta
}

// TestWithBudgetNotifier_WiresField — I1 regression: the executor's
// WithBudgetNotifier option actually sets the field (not a no-op), so the
// container's wiring line makes the executor's soft/hard alerts live. The gate
// tests below prove the executor then USES that notifier.
func TestWithBudgetNotifier_WiresField(t *testing.T) {
	e := &Executor{}
	if e.budgetNotifier != nil {
		t.Fatal("precondition: notifier must start nil")
	}
	WithBudgetNotifier(&captureBudgetNotifier{})(e)
	if e.budgetNotifier == nil {
		t.Fatal("WithBudgetNotifier must set a non-nil budgetNotifier (I1)")
	}
}

// TestEnforceTaskBudget_DisabledNoOp — no per-task budget configured: the gate
// is a no-op (no spend read, no park), byte-identical to today's behaviour.
func TestEnforceTaskBudget_DisabledNoOp(t *testing.T) {
	usage := usageWithSpend(1000)
	msg := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newBudgetGateExecutor(usage, msg, tr, nil)
	plan := &executionPlan{project: budgetProject(0)} // disabled

	err := e.enforceTaskBudget(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1"}, &persistence.Execution{ID: "e1"}, plan, "step")
	if err != nil {
		t.Fatalf("disabled governor must return nil, got %v", err)
	}
	if len(msg.inserted) != 0 || len(tr.calls) != 0 {
		t.Fatalf("disabled governor must not park")
	}
}

// TestEnforceTaskBudget_OKProceeds — under soft threshold: proceed, no park,
// but metric increments for the "ok" tier.
func TestEnforceTaskBudget_OKProceeds(t *testing.T) {
	e := newBudgetGateExecutor(usageWithSpend(1.0), &fakeMessageRepo{}, newFakeTaskRepo(), nil)
	plan := &executionPlan{project: budgetProject(10)}
	err := e.enforceTaskBudget(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1"}, &persistence.Execution{ID: "e1"}, plan, "step")
	if err != nil {
		t.Fatalf("OK tier must proceed, got %v", err)
	}
	if got := testutilCounter(t, e, "ok"); got != 1 {
		t.Fatalf("ok metric = %v, want 1", got)
	}
}

// TestEnforceTaskBudget_SoftWarnsOnceDispatches — soft tier proceeds; the
// Notifier fires once per task across repeated evaluations (dedup), but the
// metric increments every evaluation (not deduped).
func TestEnforceTaskBudget_SoftWarnsOnceDispatches(t *testing.T) {
	notifier := &captureBudgetNotifier{}
	e := newBudgetGateExecutor(usageWithSpend(8.5), &fakeMessageRepo{}, newFakeTaskRepo(), notifier)
	plan := &executionPlan{project: budgetProject(10)} // soft at 0.80 → 8.0
	task := &persistence.Task{ID: "t", ProjectID: "p1"}

	for i := 0; i < 3; i++ {
		if err := e.enforceTaskBudget(context.Background(), task, &persistence.Execution{ID: "e1"}, plan, "step"); err != nil {
			t.Fatalf("soft tier must proceed, got %v", err)
		}
	}
	if notifier.count() != 1 {
		t.Fatalf("soft Notifier must fire exactly once per task (dedup), got %d", notifier.count())
	}
	if got := testutilCounter(t, e, "soft"); got != 3 {
		t.Fatalf("soft metric must increment every eval (not deduped), got %v", got)
	}
}

// TestEnforceTaskBudget_HardParks — hard tier parks AWAITING_INPUT via
// TransitionConditional, returns errLeadHandoff, and the checkpoint carries
// decision.kind == "budget".
func TestEnforceTaskBudget_HardParks(t *testing.T) {
	notifier := &captureBudgetNotifier{}
	msg := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newBudgetGateExecutor(usageWithSpend(12.0), msg, tr, notifier)
	plan := &executionPlan{project: budgetProject(10)}

	err := e.enforceTaskBudget(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning}, &persistence.Execution{ID: "e1"}, plan, "step")
	if !IsLeadHandoff(err) {
		t.Fatalf("hard tier must return errLeadHandoff, got %v", err)
	}
	// Parked via TransitionConditional → AWAITING_INPUT.
	if len(tr.calls) != 1 || tr.calls[0].to != persistence.TaskStatusAwaitingInput {
		t.Fatalf("hard tier must transition to AWAITING_INPUT, calls=%+v", tr.calls)
	}
	meta := lastCheckpoint(t, msg)
	if meta["kind"] != "decision" {
		t.Fatalf("checkpoint kind must be decision, got %v", meta["kind"])
	}
	dec, ok := meta["decision"].(map[string]any)
	if !ok || dec["kind"] != "budget" {
		t.Fatalf("checkpoint decision.kind must be budget, got %v", meta["decision"])
	}
	if got := testutilCounter(t, e, "hard"); got != 1 {
		t.Fatalf("hard metric = %v, want 1", got)
	}
	if notifier.count() != 1 {
		t.Fatalf("hard park must notify once, got %d", notifier.count())
	}
}

// TestEnforceTaskBudget_FailClosed — a Check error parks AWAITING_INPUT
// (fail-closed), returns errLeadHandoff, and the checkpoint reason marks it
// "budget check unavailable" (distinct from a real breach).
func TestEnforceTaskBudget_FailClosed(t *testing.T) {
	usage := &stubLLMUsageRepo{sumErr: errors.New("db down")}
	msg := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newBudgetGateExecutor(usage, msg, tr, nil)
	plan := &executionPlan{project: budgetProject(10)}

	err := e.enforceTaskBudget(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning}, &persistence.Execution{ID: "e1"}, plan, "step")
	if !IsLeadHandoff(err) {
		t.Fatalf("fail-closed must return errLeadHandoff (park), got %v", err)
	}
	if len(tr.calls) != 1 || tr.calls[0].to != persistence.TaskStatusAwaitingInput {
		t.Fatalf("fail-closed must park AWAITING_INPUT, calls=%+v", tr.calls)
	}
	meta := lastCheckpoint(t, msg)
	dec, _ := meta["decision"].(map[string]any)
	if dec["kind"] != "budget" || dec["reason"] != "budget_check_unavailable" {
		t.Fatalf("fail-closed checkpoint must be a budget decision with reason budget_check_unavailable, got %v", meta["decision"])
	}
	if dec["fail_closed"] != true {
		t.Fatalf("fail-closed flag must be true, got %v", dec["fail_closed"])
	}
}

// testutilCounter reads the current value of the task-budget tier counter for
// the "p1" project and the given tier.
func testutilCounter(t *testing.T, e *Executor, tier string) float64 {
	t.Helper()
	return testutil.ToFloat64(e.metrics.TaskBudgetTierTotal.WithLabelValues("p1", tier))
}
