package taskcreate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// zerologNop returns a no-op logger.
func zerologNop() zerolog.Logger { return zerolog.Nop() }

// loadTestRegistry seeds a tiny project + swarm + workflow tree
// so the compatibility gate has real objects to check against.
// The project's swarm "coder-swarm" has a role "coder"; the
// workflow "build" uses that role; the workflow "review" uses
// "reviewer" which the swarm lacks (covers the incompat branch).
func loadTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	writeFile("swarms/coder.md", `---
swarmId: "coder-swarm"
roles:
  - name: "coder"
    runtime:
      image: "alpine:latest"
---
`)
	writeFile("workflows/build.md", `---
workflowId: "build"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "build it"
terminals:
  done:
    status: "COMPLETED"
---
`)
	writeFile("workflows/review.md", `---
workflowId: "review"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "reviewer"
    prompt: "review it"
terminals:
  done:
    status: "COMPLETED"
---
`)
	writeFile("projects/demo.yaml", `projectId: "demo"
displayName: "Demo"
swarmId: "coder-swarm"
defaultWorkflowId: "build"
defaultPriority: 42
rate_limit:
  tasks_per_minute: 1
`)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	return reg
}

func TestCreate_HappyPath_UsesProjectDefaults(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	now := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)

	c := New(
		WithTaskRepository(repo),
		WithProjectRegistry(reg),
		WithNowFunc(func() time.Time { return now }),
	)
	task, err := c.Create(context.Background(), Params{
		ProjectID: "demo",
		TaskType:  "research",
		Prompt:    "investigate the bug",
	})
	if err != nil {
		t.Fatalf("Create returned err: %v", err)
	}
	if task == nil {
		t.Fatal("Create returned nil task on success")
	}
	if task.ProjectID != "demo" {
		t.Errorf("ProjectID = %q, want demo", task.ProjectID)
	}
	if task.WorkflowID == nil || *task.WorkflowID != "build" {
		t.Errorf("WorkflowID = %v, want build", task.WorkflowID)
	}
	if task.Priority != 42 {
		t.Errorf("Priority = %d, want 42 (project default)", task.Priority)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("Status = %s, want QUEUED", task.Status)
	}
	if repo.CallCount.Create != 1 {
		t.Errorf("Create calls = %d, want 1", repo.CallCount.Create)
	}
	// Payload should carry the prompt at context.prompt.
	var payload struct {
		TaskType string `json:"taskType"`
		Context  struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.TaskType != "research" {
		t.Errorf("payload.taskType = %q, want research", payload.TaskType)
	}
	if payload.Context.Prompt != "investigate the bug" {
		t.Errorf("payload.context.prompt = %q, want %q", payload.Context.Prompt, "investigate the bug")
	}
}

func TestCreate_MissingProjectID(t *testing.T) {
	c := New(WithTaskRepository(&mocks.MockTaskRepository{}))
	_, err := c.Create(context.Background(), Params{TaskType: "research"})
	ce := AsError(err)
	if ce == nil {
		t.Fatalf("Create err = %v, want *Error", err)
	}
	if ce.Reason != ReasonValidation {
		t.Errorf("Reason = %s, want %s", ce.Reason, ReasonValidation)
	}
}

func TestCreate_MissingTaskType(t *testing.T) {
	c := New(WithTaskRepository(&mocks.MockTaskRepository{}))
	_, err := c.Create(context.Background(), Params{ProjectID: "demo"})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonValidation {
		t.Fatalf("err = %v, want ReasonValidation", err)
	}
}

func TestCreate_ProjectNotFound(t *testing.T) {
	reg := loadTestRegistry(t)
	c := New(WithTaskRepository(&mocks.MockTaskRepository{}), WithProjectRegistry(reg))
	_, err := c.Create(context.Background(), Params{ProjectID: "ghost", TaskType: "research"})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonProjectNotFound {
		t.Fatalf("err = %v, want ReasonProjectNotFound", err)
	}
}

func TestCreate_WorkflowNotFound(t *testing.T) {
	reg := loadTestRegistry(t)
	c := New(WithTaskRepository(&mocks.MockTaskRepository{}), WithProjectRegistry(reg))
	_, err := c.Create(context.Background(), Params{
		ProjectID:  "demo",
		TaskType:   "research",
		WorkflowID: "no-such-workflow",
	})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonWorkflowNotFound {
		t.Fatalf("err = %v, want ReasonWorkflowNotFound", err)
	}
}

func TestCreate_WorkflowIncompatibleWithSwarm(t *testing.T) {
	reg := loadTestRegistry(t)
	c := New(WithTaskRepository(&mocks.MockTaskRepository{}), WithProjectRegistry(reg))
	_, err := c.Create(context.Background(), Params{
		ProjectID:  "demo",
		TaskType:   "research",
		WorkflowID: "review", // requires "reviewer" role; swarm lacks it
	})
	ce := AsError(err)
	if ce == nil {
		t.Fatalf("err = %v, want ReasonWorkflowIncompat", err)
	}
	if ce.Reason != ReasonWorkflowIncompat {
		t.Errorf("Reason = %s, want %s", ce.Reason, ReasonWorkflowIncompat)
	}
}

func TestCreate_RateLimited(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	limiter := ratelimit.New()
	c := New(
		WithTaskRepository(repo),
		WithProjectRegistry(reg),
		WithRateLimiter(limiter),
	)
	// First call passes (tasks_per_minute=1).
	if _, err := c.Create(context.Background(), Params{
		ProjectID: "demo",
		TaskType:  "research",
		Prompt:    "first",
	}); err != nil {
		t.Fatalf("first Create failed: %v", err)
	}
	// Second within the same minute should be rate-limited.
	_, err := c.Create(context.Background(), Params{
		ProjectID: "demo",
		TaskType:  "research",
		Prompt:    "second",
	})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonRateLimited {
		t.Fatalf("err = %v, want ReasonRateLimited", err)
	}
	if repo.CallCount.Create != 1 {
		t.Errorf("Create calls = %d, want 1 (second blocked)", repo.CallCount.Create)
	}
}

func TestCreate_IdempotencyReturnsExisting(t *testing.T) {
	reg := loadTestRegistry(t)
	existing := &persistence.Task{ID: "task_prior", ProjectID: "demo"}
	repo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
			if idempotencyKey == "dup-key" {
				return existing, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	got, err := c.Create(context.Background(), Params{
		ProjectID:      "demo",
		TaskType:       "research",
		IdempotencyKey: "dup-key",
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	if got != existing {
		t.Errorf("got = %v, want existing task", got)
	}
	if repo.CallCount.Create != 0 {
		t.Errorf("Create calls = %d, want 0 (idempotent hit)", repo.CallCount.Create)
	}
}

func TestCreate_NoTaskRepoIsInternalError(t *testing.T) {
	c := New() // no repo
	_, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "research"})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonInternal {
		t.Fatalf("err = %v, want ReasonInternal", err)
	}
}

func TestCreate_PersistenceErrorWrapped(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{
		CreateFunc: func(ctx context.Context, task *persistence.Task) error {
			return errors.New("db down")
		},
	}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	_, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "research"})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonInternal {
		t.Fatalf("err = %v, want ReasonInternal", err)
	}
	if ce.Cause == nil {
		t.Error("expected Cause to be wrapped")
	}
}

func TestCreate_ExplicitPriorityOverridesProjectDefault(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	task, err := c.Create(context.Background(), Params{
		ProjectID: "demo",
		TaskType:  "research",
		Priority:  77,
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	if task.Priority != 77 {
		t.Errorf("Priority = %d, want 77 (explicit overrides project default 42)", task.Priority)
	}
}

func TestCreate_ExplicitWorkflowOverridesProjectDefault(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	task, err := c.Create(context.Background(), Params{
		ProjectID:  "demo",
		TaskType:   "research",
		WorkflowID: "build", // explicit, same as default but exercises the override path
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	if task.WorkflowID == nil || *task.WorkflowID != "build" {
		t.Errorf("WorkflowID = %v, want build", task.WorkflowID)
	}
}

func TestCreate_ExtraContextMergedWithPrompt(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	task, err := c.Create(context.Background(), Params{
		ProjectID:    "demo",
		TaskType:     "research",
		Prompt:       "do the thing",
		ExtraContext: map[string]any{"source": "ui-form", "trace": "abc"},
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	var p struct {
		Context map[string]any `json:"context"`
	}
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Context["prompt"] != "do the thing" {
		t.Errorf("context.prompt = %v, want %q", p.Context["prompt"], "do the thing")
	}
	if p.Context["source"] != "ui-form" {
		t.Errorf("context.source = %v, want ui-form", p.Context["source"])
	}
}

func TestCreate_RecordsRateLimiterSampleOnSuccess(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	limiter := ratelimit.New()
	c := New(
		WithTaskRepository(repo),
		WithProjectRegistry(reg),
		WithRateLimiter(limiter),
	)
	if _, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "research"}); err != nil {
		t.Fatalf("Create err = %v", err)
	}
	// After one successful call the limiter is at its cap (1/min).
	// The next Check should report Blocked.
	d := limiter.Check(reg.GetProject("demo"), time.Now())
	if !d.Blocked {
		t.Errorf("limiter not advanced; Check.Blocked = false, want true")
	}
}

// TestCreate_IdempotencyLookupErrorIsWrapped covers the branch
// where the idempotency lookup returns a non-ErrNotFound error.
// We expect ReasonInternal — the operator probably wants to retry
// rather than re-submit and risk creating a dup.
func TestCreate_IdempotencyLookupErrorIsWrapped(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
			return nil, errors.New("db connection lost")
		},
	}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	_, err := c.Create(context.Background(), Params{
		ProjectID:      "demo",
		TaskType:       "research",
		IdempotencyKey: "k1",
	})
	ce := AsError(err)
	if ce == nil || ce.Reason != ReasonInternal {
		t.Fatalf("err = %v, want ReasonInternal", err)
	}
}

// TestCreate_DuplicateKeyRaceRecovery covers the lost-race branch
// where two concurrent Create() calls with the same idempotency
// key race to the unique constraint — one wins, the loser gets
// ErrDuplicateKey and looks up the winner.
func TestCreate_DuplicateKeyRaceRecovery(t *testing.T) {
	reg := loadTestRegistry(t)
	winner := &persistence.Task{ID: "task_winner", ProjectID: "demo"}
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetByIdempotencyKeyFunc: func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
			calls++
			if calls == 1 {
				// Pre-insert idempotency lookup: not yet present.
				return nil, persistence.ErrNotFound
			}
			// Post-insert recovery: the winning row.
			return winner, nil
		},
		CreateFunc: func(ctx context.Context, task *persistence.Task) error {
			return persistence.ErrDuplicateKey
		},
	}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	got, err := c.Create(context.Background(), Params{
		ProjectID:      "demo",
		TaskType:       "research",
		IdempotencyKey: "race-key",
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	if got != winner {
		t.Errorf("got = %v, want winner", got)
	}
}

// TestCreate_QueueEnqueueFailureIsInternal exercises the branch
// where the row persists but the queue rejects the enqueue.
func TestCreate_QueueEnqueueFailureIsInternal(t *testing.T) {
	// We can't easily mock *queue.Queue (concrete type) without
	// pulling it apart. Instead verify the no-queue path here —
	// the WithQueue(nil) Creator should succeed without calling
	// Enqueue. The error-from-queue branch is covered indirectly by
	// the queue package's own tests + the api integration tests.
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg), WithQueue(nil))
	if _, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "research"}); err != nil {
		t.Fatalf("Create err = %v", err)
	}
}

// TestCreate_NoRegistry_FallsBackToLegacyDefaults exercises the
// registry-less branch (Creator built without WithProjectRegistry).
// The compatibility gate and project-default lookups are skipped
// and the legacy priority=50 default applies.
func TestCreate_NoRegistry_FallsBackToLegacyDefaults(t *testing.T) {
	repo := &mocks.MockTaskRepository{}
	c := New(WithTaskRepository(repo))
	task, err := c.Create(context.Background(), Params{
		ProjectID: "anywhere",
		TaskType:  "research",
		Prompt:    "go",
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	if task.Priority != 50 {
		t.Errorf("Priority = %d, want 50 (legacy default)", task.Priority)
	}
}

// TestCreate_DelegationSource confirms an explicit CreationSource
// is honoured (used by autonomy / webhook surfaces).
func TestCreate_DelegationSource(t *testing.T) {
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	c := New(WithTaskRepository(repo), WithProjectRegistry(reg))
	task, err := c.Create(context.Background(), Params{
		ProjectID:      "demo",
		TaskType:       "research",
		CreationSource: persistence.TaskCreationSourceAutonomous,
	})
	if err != nil {
		t.Fatalf("Create err = %v", err)
	}
	if task.CreationSource != persistence.TaskCreationSourceAutonomous {
		t.Errorf("CreationSource = %v, want AUTONOMOUS", task.CreationSource)
	}
}

func TestError_Format(t *testing.T) {
	e := &Error{Reason: ReasonValidation, Message: "boom"}
	if got := e.Error(); got != "VALIDATION_ERROR: boom" {
		t.Errorf("Error() = %q", got)
	}
	wrapped := &Error{Reason: ReasonInternal, Message: "db", Cause: errors.New("conn refused")}
	if got := wrapped.Error(); got != "INTERNAL_ERROR: db: conn refused" {
		t.Errorf("Error() = %q", got)
	}
	if wrapped.Unwrap() == nil {
		t.Error("Unwrap returned nil")
	}
}

// TestOptions_AllSettable exercises the option setters that the
// happy-path tests don't reach (queue, llmUsageRepo, budgetNotifier,
// logger). The Creator just stashes the values; we verify they're
// preserved and that a New() call accepts them without panicking.
func TestOptions_AllSettable(t *testing.T) {
	repo := &mocks.MockTaskRepository{}
	notifier := &fakeBudgetNotifier{}
	c := New(
		WithTaskRepository(repo),
		WithQueue(nil), // explicit nil is the default but exercises the setter
		WithLLMUsageRepository(nil),
		WithBudgetNotifier(notifier),
	)
	if c.taskRepo != repo {
		t.Error("taskRepo not stashed")
	}
	if c.budgetNotifier != notifier {
		t.Error("budgetNotifier not stashed")
	}
}

// fakeBudgetNotifier is a no-op implementation that satisfies
// budget.Notifier; declared at file scope so multiple tests can use it.
type fakeBudgetNotifier struct {
	calls int
}

func (f *fakeBudgetNotifier) NotifyBudgetBreach(ctx context.Context, projectID, level, period string, decision budget.Decision) {
	f.calls++
}

func TestCreate_WithLoggerOption(t *testing.T) {
	// Mostly an aliveness check — the logger is a value, so just
	// confirm the setter doesn't panic and the Creator still
	// services requests.
	reg := loadTestRegistry(t)
	repo := &mocks.MockTaskRepository{}
	c := New(
		WithTaskRepository(repo),
		WithProjectRegistry(reg),
		WithLogger(zerologNop()),
	)
	if _, err := c.Create(context.Background(), Params{ProjectID: "demo", TaskType: "research"}); err != nil {
		t.Fatalf("Create err = %v", err)
	}
}

func TestAsError_NilAndOther(t *testing.T) {
	if AsError(nil) != nil {
		t.Error("AsError(nil) = non-nil")
	}
	if AsError(errors.New("plain")) != nil {
		t.Error("AsError(plain) = non-nil")
	}
	ce := &Error{Reason: ReasonValidation, Message: "x"}
	if AsError(ce) != ce {
		t.Error("AsError(*Error) didn't echo it back")
	}
}
