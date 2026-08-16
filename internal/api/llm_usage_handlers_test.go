package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestIngestLLMUsage_RejectsTaskKeyWritingOtherTask — Finding B3. A
// per-task key for task X (same project) must not write a usage row for
// task Y. Pre-fix the handler checked task→project but not key→task.
func TestIngestLLMUsage_RejectsTaskKeyWritingOtherTask(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo), WithTaskRepository(taskRepo))

	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-Y","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatalf("Upsert called despite task-key/task_id mismatch: %#v", repo.row)
	}
}

func TestIngestLLMUsage_RejectsExecutionFromOtherTask(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, TaskID: "task-other", ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithLLMUsageRepository(repo),
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
	)

	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-X","execution_id":"exec-other","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatalf("Upsert called despite execution/task mismatch: %#v", repo.row)
	}
}

// TestIngestLLMUsage_AcceptsTaskKeyWritingOwnTask — the same per-task key
// writing its OWN task's usage row succeeds (B3).
func TestIngestLLMUsage_AcceptsTaskKeyWritingOwnTask(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo), WithTaskRepository(taskRepo))

	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-X","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.TaskID == nil || *repo.row.TaskID != "task-X" {
		t.Fatalf("own-task usage row not persisted: %#v", repo.row)
	}
}

// TestIngestLLMUsage_NonTaskScopedKeyKeepsProjectBehavior — an
// admin/operator key (not task-scoped) keeps project-level behavior (B3
// must not regress non-task-scoped keys).
func TestIngestLLMUsage_NonTaskScopedKeyKeepsProjectBehavior(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo), WithTaskRepository(taskRepo))

	id := &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: &persistence.APIKey{Name: "operator-key", ProjectID: "proj-a"}}}
	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-any","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), identityKey, id)
	ctx = context.WithValue(ctx, projectIDKey, []string{"proj-a"})
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req.WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
}

type capturingLLMUsageRepo struct {
	mockLLMUsageRepo
	row      *persistence.TaskLLMUsage
	recorded []*persistence.TaskLLMUsage
	err      error
}

func (m *capturingLLMUsageRepo) Upsert(_ context.Context, row *persistence.TaskLLMUsage) error {
	m.row = row
	return m.err
}

func (m *capturingLLMUsageRepo) Record(_ context.Context, row *persistence.TaskLLMUsage) error {
	clone := *row
	m.recorded = append(m.recorded, &clone)
	return m.err
}

func (m *capturingLLMUsageRepo) lastRecorded() *persistence.TaskLLMUsage {
	if len(m.recorded) == 0 {
		return nil
	}
	return m.recorded[len(m.recorded)-1]
}

func TestIngestLLMUsage_UpsertsWorkflowStepUsage(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))

	body := `{
		"usage_id":"tu_task1_step1_coder",
		"project_id":"p1",
		"task_id":"task1",
		"execution_id":"exec1",
		"step_id":"step1",
		"role":"coder",
		"model":"gpt-test",
		"prompt_tokens":123,
		"completion_tokens":45,
		"cache_creation_tokens":12,
		"cache_read_tokens":34,
		"iterations":2,
		"cost_usd":0.0123
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.IngestLLMUsage(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil {
		t.Fatal("expected Upsert row")
	}
	// The id is re-derived from the validated task/execution/step/role, NOT
	// taken from the agent's usage_id (which omits the execution).
	if repo.row.ID != "tu_task1_exec1_step1_coder" || repo.row.ProjectID != "p1" || repo.row.StepID != "step1" {
		t.Fatalf("unexpected row identity: %#v", repo.row)
	}
	if repo.row.TaskID == nil || *repo.row.TaskID != "task1" {
		t.Fatalf("TaskID = %#v", repo.row.TaskID)
	}
	if repo.row.ExecutionID == nil || *repo.row.ExecutionID != "exec1" {
		t.Fatalf("ExecutionID = %#v", repo.row.ExecutionID)
	}
	if repo.row.Role != "coder" || repo.row.Model != "gpt-test" {
		t.Fatalf("unexpected role/model: %#v", repo.row)
	}
	if repo.row.PromptTokens != 123 || repo.row.CompletionTokens != 45 || repo.row.Iterations != 2 {
		t.Fatalf("unexpected usage counts: %#v", repo.row)
	}
	if repo.row.CacheCreationTokens != 12 || repo.row.CacheReadTokens != 34 {
		t.Fatalf("unexpected cache counts: %#v", repo.row)
	}
	if repo.row.CostUSD != 0.0123 {
		t.Fatalf("CostUSD = %v", repo.row.CostUSD)
	}
	if repo.row.Source != persistence.TaskLLMUsageSourceWorkflowStep {
		t.Fatalf("Source = %q", repo.row.Source)
	}
}

// Regression, 2026-08-16: a retry's stream erased the spend of the execution it
// retried.
//
// The agent sends the SAME usage_id for both attempts — it is built from
// (task, step, role) and knows nothing about executions — so trusting it made
// the second attempt's upsert overwrite the first attempt's row, rewriting its
// execution_id along the way. On a preserved ledger that erased 1,158 rows.
//
// Pre-fix this fails: both requests land on tu_task1_step1_coder.
func TestIngestLLMUsage_RetryDoesNotOverwriteEarlierExecution(t *testing.T) {
	post := func(execID string) string {
		repo := &capturingLLMUsageRepo{}
		server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))
		body := `{
			"usage_id":"tu_task1_step1_coder",
			"project_id":"p1",
			"task_id":"task1",
			"execution_id":"` + execID + `",
			"step_id":"step1",
			"role":"coder",
			"model":"gpt-test",
			"prompt_tokens":10,
			"iterations":1
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		server.IngestLLMUsage(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
		}
		if repo.row == nil {
			t.Fatal("expected Upsert row")
		}
		return repo.row.ID
	}

	first, retry := post("exec1"), post("exec2")
	if first == retry {
		t.Fatalf("retry collided with the execution it retried; both wrote %q", first)
	}
}

// A request with no task keeps the caller's own id: the derived shape would
// degenerate to tu__<step>_<role> and collide across every such caller, which is
// strictly worse than what it replaced.
func TestIngestLLMUsage_NoTaskKeepsCallerSuppliedID(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))

	body := `{
		"usage_id":"dispatcher-run-7",
		"project_id":"p1",
		"role":"dispatcher",
		"model":"gpt-test",
		"prompt_tokens":5,
		"iterations":1
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.IngestLLMUsage(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.ID != "dispatcher-run-7" {
		t.Fatalf("taskless request must keep its own id, got %#v", repo.row)
	}
}

func TestIngestLLMUsage_RejectsMissingRequiredFields(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(`{"project_id":"p1"}`))
	rec := httptest.NewRecorder()

	server.IngestLLMUsage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatalf("Upsert called despite validation failure: %#v", repo.row)
	}
}

func TestIngestLLMUsage_RejectsOversizedBody(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	rec := httptest.NewRecorder()

	server.IngestLLMUsage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatalf("Upsert called for oversized body: %#v", repo.row)
	}
}

func TestIngestLLMUsage_RepoNotConfigured(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()

	server.IngestLLMUsage(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503", rec.Code, rec.Body.String())
	}
}

func TestIngestLLMUsage_UpsertFailure(t *testing.T) {
	repo := &capturingLLMUsageRepo{err: errors.New("db down")}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))

	body := `{"usage_id":"u1","project_id":"p1","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.IngestLLMUsage(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", rec.Code, rec.Body.String())
	}
}

// TestIngestLLMUsage_RejectsProjectScopeMismatch is the audit fix:
// an authenticated key with scope ["proj-a"] cannot post a usage row
// claiming project_id="proj-b". Pre-fix the request was admitted and
// the cost row landed under proj-b, poisoning that project's budget
// enforcement.
func TestIngestLLMUsage_RejectsProjectScopeMismatch(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo))

	body := `{"usage_id":"u1","project_id":"proj-b","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a"})
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatalf("Upsert called despite scope mismatch: %#v", repo.row)
	}
}

// TestIngestLLMUsage_RejectsTaskFromOtherProject — the body's
// project_id matches the API key's scope, but task_id resolves to a
// different project's row. Either tampering or a bug — refuse rather
// than upsert into the wrong ledger.
func TestIngestLLMUsage_RejectsTaskFromOtherProject(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-b"}, nil
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithLLMUsageRepository(repo),
		WithTaskRepository(taskRepo),
	)

	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-stolen","role":"coder"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatalf("Upsert called despite task-project mismatch: %#v", repo.row)
	}
}

// TestIngestLLMUsage_CopiesAPIKeyAttributionFromTask — this endpoint is called
// only by the daemon-injected agent-container key, never the original caller's
// key, so a usage row's attribution has to come from the TASK. Raised by the
// 2026-08-04 review of the CE PR #6 intake: the copy existed, nothing pinned it.
func TestIngestLLMUsage_CopiesAPIKeyAttributionFromTask(t *testing.T) {
	creator := "akey-creator"
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a", CreatedByAPIKeyID: &creator}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo), WithTaskRepository(taskRepo))

	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-X","role":"coder","cost_usd":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.APIKeyID == nil {
		t.Fatalf("usage row carries no api-key attribution: %#v", repo.row)
	}
	if *repo.row.APIKeyID != creator {
		t.Fatalf("APIKeyID = %q, want the task's creator %q", *repo.row.APIKeyID, creator)
	}
}

// TestIngestLLMUsage_TaskLookupFailureRecordsUnattributed — a task fetch that
// fails must not drop the row (the cost is real; dropping it understates spend),
// but the row then has no attribution and shows as "Unattributed". Pinned so the
// degradation stays deliberate rather than becoming a silent 500 or a lost row.
func TestIngestLLMUsage_TaskLookupFailureRecordsUnattributed(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("db unavailable")
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(repo), WithTaskRepository(taskRepo))

	body := `{"usage_id":"u1","project_id":"proj-a","task_id":"task-X","role":"coder","cost_usd":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/llm-usage", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestLLMUsage(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204 (the spend must still be recorded)", rec.Code, rec.Body.String())
	}
	if repo.row == nil {
		t.Fatal("usage row was dropped when the task lookup failed")
	}
	if repo.row.APIKeyID != nil {
		t.Fatalf("APIKeyID = %v, want nil — a failed lookup must not invent attribution", *repo.row.APIKeyID)
	}
}
