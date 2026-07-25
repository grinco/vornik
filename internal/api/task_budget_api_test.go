package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// forecastStubUsageRepo drives ForecastTask to a controllable per-step cost via
// AggregateByRoleModel while satisfying the full TaskLLMUsageRepository.
type forecastStubUsageRepo struct {
	perStepCost float64
	sumErr      error
	forecastErr error
}

func (f *forecastStubUsageRepo) Record(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (f *forecastStubUsageRepo) Upsert(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (f *forecastStubUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (f *forecastStubUsageRepo) SumCostByTask(context.Context, string) (float64, error) {
	return 0, nil
}
func (f *forecastStubUsageRepo) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, f.sumErr
}
func (f *forecastStubUsageRepo) SumCost(context.Context, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (f *forecastStubUsageRepo) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	if f.forecastErr != nil {
		return nil, f.forecastErr
	}
	return []persistence.RoleModelSpend{{Role: "coder", Model: "m", CostUSD: f.perStepCost, StepCount: 1}}, nil
}
func (f *forecastStubUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (f *forecastStubUsageRepo) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (f *forecastStubUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (f *forecastStubUsageRepo) TopTasks(context.Context, time.Time, time.Time, int, string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (f *forecastStubUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}
func (f *forecastStubUsageRepo) SumCostByAPIKey(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (f *forecastStubUsageRepo) MeanCostByWorkflow(context.Context, string, string, time.Time, time.Time) (float64, int, error) {
	return 0, 0, nil
}

// budgetRegistry builds a p1 project whose build workflow runs one coder agent
// step on model "m", with the given per-task budget YAML fragment.
func budgetRegistry(t *testing.T, budgetYAML string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	write("swarms/coder.md", `---
swarmId: "task-swarm"
roles:
  - name: "coder"
    model: "m"
    runtime:
      image: "test:latest"
---
`)
	write("workflows/build.md", `---
workflowId: "build"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "do it"
terminals:
  done:
    status: "COMPLETED"
---
`)
	write("projects/p1.yaml", "projectId: \"p1\"\ndisplayName: \"P1\"\nswarmId: \"task-swarm\"\ndefaultWorkflowId: \"build\"\n"+budgetYAML)
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

// TestWebhookAdmit_ForecastRefusesOverPerTaskBudget — regression: the webhook
// path (which bypasses the shared Creator) enforces the pre-flight forecast +
// per-task budget via admitWebhookTask, previously ungated.
func TestWebhookAdmit_ForecastRefusesOverPerTaskBudget(t *testing.T) {
	reg := budgetRegistry(t, "budget:\n  default_task_budget_usd: 0.5\n")
	server := NewServer(
		WithProjectRegistry(reg),
		WithLLMUsageRepository(&forecastStubUsageRepo{perStepCost: 10.0}),
	)
	proj := reg.GetProject("p1")
	adm := server.admitWebhookTask(context.Background(), proj, "build")
	if adm.ok {
		t.Fatalf("webhook admit must refuse over per-task budget")
	}
	if adm.code != "BUDGET_EXCEEDED" {
		t.Fatalf("webhook refusal code = %q, want BUDGET_EXCEEDED", adm.code)
	}
}

// TestWebhookAdmit_ShortCircuitsWhenNothingConfigured — M6: no project cap AND
// no per-task budget ⇒ the forecast gate short-circuits (admit ok) even with a
// large forecast.
func TestWebhookAdmit_ShortCircuitsWhenNothingConfigured(t *testing.T) {
	reg := budgetRegistry(t, "") // no budget block
	server := NewServer(
		WithProjectRegistry(reg),
		WithLLMUsageRepository(&forecastStubUsageRepo{perStepCost: 10.0}),
	)
	adm := server.admitWebhookTask(context.Background(), reg.GetProject("p1"), "build")
	if !adm.ok {
		t.Fatalf("nothing configured → admit must be ok, got %+v", adm)
	}
}

func TestWebhookAdmit_ConfiguredBudgetFailsClosedWhenSpendUnavailable(t *testing.T) {
	reg := budgetRegistry(t, "budget:\n  daily_hard_usd: 5\n")
	server := NewServer(
		WithProjectRegistry(reg),
		WithLLMUsageRepository(&forecastStubUsageRepo{sumErr: errors.New("database unavailable")}),
	)
	adm := server.admitWebhookTask(context.Background(), reg.GetProject("p1"), "build")
	if adm.ok || adm.code != "BUDGET_EXCEEDED" {
		t.Fatalf("configured budget must fail closed when spend is unavailable, got %+v", adm)
	}
}

func TestWebhookAdmit_ConfiguredBudgetFailsClosedWhenForecastUnavailable(t *testing.T) {
	reg := budgetRegistry(t, "budget:\n  default_task_budget_usd: 5\n")
	server := NewServer(
		WithProjectRegistry(reg),
		WithLLMUsageRepository(&forecastStubUsageRepo{forecastErr: errors.New("history unavailable")}),
	)
	adm := server.admitWebhookTask(context.Background(), reg.GetProject("p1"), "build")
	if adm.ok || adm.code != "BUDGET_EXCEEDED" {
		t.Fatalf("configured budget must fail closed when forecast is unavailable, got %+v", adm)
	}
}

// --- AnswerCheckpoint budget-resume branch (I2/I3/I5) ---

// budgetCheckpointMsg returns an open budget-decision checkpoint message.
func budgetCheckpointMsg(id string) *persistence.TaskMessage {
	meta, _ := json.Marshal(map[string]any{
		"kind":     "decision",
		"decision": map[string]any{"kind": "budget", "spent_usd": 12.0, "budget_usd": 10.0},
		"question": "over budget",
		"options": []map[string]any{
			{"id": "increase", "label": "Increase budget & resume"},
			{"id": "reduce_scope", "label": "Reduce scope & resume"},
			{"id": "abandon", "label": "Abandon"},
		},
	})
	return &persistence.TaskMessage{ID: id, TaskID: tcTaskID, MessageKind: persistence.TaskMessageKindCheckpoint, Metadata: meta}
}

// authOff stamps the request context as auth-disabled → trusted local operator
// passes the admin-class check.
func authOff(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authEnabledKey, false))
}

func budgetTaskRepo(cp string, raiseFn func(context.Context, string, float64, bool) (bool, error), transFn func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error)) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			tk := tcTask(persistence.TaskStatusAwaitingInput)
			tk.OpenCheckpointID = &cp
			return tk, nil
		},
		RaiseTaskBudgetFunc:       raiseFn,
		TransitionConditionalFunc: transFn,
	}
}

// TestAnswerCheckpoint_BudgetIncrease_NonAdminForbidden — I5/authority: a
// non-admin answering "increase" on a budget checkpoint is 403'd BEFORE any
// write (no resolve, no raise).
func TestAnswerCheckpoint_BudgetIncrease_NonAdminForbidden(t *testing.T) {
	cp := "cp1"
	raiseCalled := false
	taskRepo := budgetTaskRepo(cp,
		func(context.Context, string, float64, bool) (bool, error) { raiseCalled = true; return true, nil },
		nil)
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	// auth-enabled (default), no admin key/session → non-admin.
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"increase","metadata":{"budget_usd":20}}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin increase must be 403, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if raiseCalled {
		t.Fatalf("no budget raise on a rejected non-admin attempt")
	}
	if msgRepo.resolvedCheckpoint != "" {
		t.Fatalf("checkpoint must NOT be resolved on a rejected attempt")
	}
}

// TestAnswerCheckpoint_BudgetIncrease_AdminRaisesResumesResolves — I2/I3: admin
// "increase" raises (resume mode) AND the checkpoint resolves via the normal
// path (MarkCheckpointResolved).
func TestAnswerCheckpoint_BudgetIncrease_AdminRaisesResumesResolves(t *testing.T) {
	cp := "cp1"
	var gotBudget float64
	var gotResume bool
	taskRepo := budgetTaskRepo(cp,
		func(_ context.Context, _ string, n float64, resume bool) (bool, error) {
			gotBudget, gotResume = n, resume
			return true, nil
		}, nil)
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	req := authOff(httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"increase","metadata":{"budget_usd":20}}`)))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin increase want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if gotBudget != 20 || !gotResume {
		t.Fatalf("RaiseTaskBudget got budget=%v resume=%v, want 20/true", gotBudget, gotResume)
	}
	if msgRepo.resolvedCheckpoint != cp {
		t.Fatalf("I3: checkpoint must be resolved via normal path, got %q", msgRepo.resolvedCheckpoint)
	}
}

// TestAnswerCheckpoint_BudgetIncrease_MissingValue — admin "increase" without a
// budget_usd is 400.
func TestAnswerCheckpoint_BudgetIncrease_MissingValue(t *testing.T) {
	cp := "cp1"
	taskRepo := budgetTaskRepo(cp, func(context.Context, string, float64, bool) (bool, error) { return true, nil }, nil)
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	req := authOff(httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"increase"}`)))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("increase without budget_usd want 400, got %d", rec.Code)
	}
}

// TestAnswerCheckpoint_BudgetAbandon_Cancels — I2: "abandon" is not inert; it
// cancels the parked task (AWAITING_INPUT→CANCELLED) and resolves the checkpoint.
func TestAnswerCheckpoint_BudgetAbandon_Cancels(t *testing.T) {
	cp := "cp1"
	var toStatus persistence.TaskStatus
	taskRepo := budgetTaskRepo(cp, nil,
		func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			toStatus = to
			return true, nil
		})
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"abandon","content":"stop"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("abandon want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if toStatus != persistence.TaskStatusCancelled {
		t.Fatalf("abandon must transition to CANCELLED, got %v", toStatus)
	}
	if msgRepo.resolvedCheckpoint != cp {
		t.Fatalf("abandon must resolve the checkpoint, got %q", msgRepo.resolvedCheckpoint)
	}
}

// TestAnswerCheckpoint_BudgetReduceScope_RequeuesNoRaise — "reduce scope"
// resumes (AWAITING_INPUT→QUEUED) WITHOUT raising the budget and needs no
// elevated auth.
func TestAnswerCheckpoint_BudgetReduceScope_RequeuesNoRaise(t *testing.T) {
	cp := "cp1"
	raiseCalled := false
	var toStatus persistence.TaskStatus
	taskRepo := budgetTaskRepo(cp,
		func(context.Context, string, float64, bool) (bool, error) { raiseCalled = true; return true, nil },
		func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			toStatus = to
			return true, nil
		})
	msgRepo := &tcStubMessageRepo{getOpenFn: func(context.Context, string) (*persistence.TaskMessage, error) { return budgetCheckpointMsg(cp), nil }}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"reduce_scope","content":"trim it"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reduce_scope want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if raiseCalled {
		t.Fatalf("reduce_scope must NOT raise the budget")
	}
	if toStatus != persistence.TaskStatusQueued {
		t.Fatalf("reduce_scope must re-queue (QUEUED), got %v", toStatus)
	}
}
