package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubAuditRepoY records every Log call so tests can assert on the
// persisted entries. List + CountByTool are placeholder
// implementations — persistToolAuditFromResult only invokes Log.
type stubAuditRepoY struct {
	mu      sync.Mutex
	entries []*persistence.ToolAuditEntry
	logErr  error
}

func (s *stubAuditRepoY) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logErr != nil {
		return s.logErr
	}
	cp := *e
	s.entries = append(s.entries, &cp)
	return nil
}
func (s *stubAuditRepoY) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, nil
}
func (s *stubAuditRepoY) CountByTool(_ context.Context, _ string) (map[string]int64, error) {
	return nil, nil
}

// TestPersistToolAuditFromResult_InvalidJSON — corrupt result
// returns "" without writing anything. The warn log message is
// observable in the daemon log; we just want the function not
// to crash.
func TestPersistToolAuditFromResult_InvalidJSON(t *testing.T) {
	ar := &stubAuditRepoY{}
	e := &Executor{auditRepo: ar, logger: zerolog.Nop()}
	_, loop := e.persistToolAuditFromResult(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"},
		"step-1", []byte("not json {{"))
	assert.Equal(t, "", loop)
	assert.Empty(t, ar.entries)
}

// TestPersistToolAuditFromResult_NoEntries — well-formed JSON
// but no toolAudit field → silent no-op.
func TestPersistToolAuditFromResult_NoEntries(t *testing.T) {
	ar := &stubAuditRepoY{}
	e := &Executor{auditRepo: ar, logger: zerolog.Nop()}
	_, loop := e.persistToolAuditFromResult(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"},
		"step-1", []byte(`{"other":"field"}`))
	assert.Equal(t, "", loop)
	assert.Empty(t, ar.entries)
}

// TestPersistToolAuditFromResult_NoAuditRepo — when the executor
// runs without an audit repo wired (test deployments), the loop
// detector still runs but no DB writes happen. Pinning this
// behaviour matters because the loop detail is used to
// reclassify the step outcome independently of audit logging.
func TestPersistToolAuditFromResult_DetectsLoopWithoutRepo(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	body := mustMarshal(map[string]any{
		"toolAudit": []map[string]any{
			{"tool": "read_file", "input": "/tmp/x"},
			{"tool": "read_file", "input": "/tmp/x"},
			{"tool": "read_file", "input": "/tmp/x"},
		},
	})
	_, loop := e.persistToolAuditFromResult(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"},
		"step", body)
	assert.NotEmpty(t, loop,
		"three consecutive identical (tool, input) calls must be classified as degenerate loop")
}

// TestPersistToolAuditFromResult_HappyPath_PersistsEntries — the
// standard path: parse + write each entry + emit per-tool
// metrics. Asserts the audit_id reuse contract (entries that
// carry an audit_id are persisted with that id; missing audit_id
// gets a fresh one).
func TestPersistToolAuditFromResult_HappyPath(t *testing.T) {
	ar := &stubAuditRepoY{}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	e := &Executor{
		auditRepo: ar,
		metrics:   m,
		logger:    zerolog.Nop(),
	}
	body := mustMarshal(map[string]any{
		"toolAudit": []map[string]any{
			{"audit_id": "ta-1", "tool": "read_file", "input": "/a", "output": "ok", "duration_ms": 12},
			{"tool": "edit_file", "input": "/b", "output": "ok", "duration_ms": 30}, // no audit_id → fresh one
		},
	})
	_, loop := e.persistToolAuditFromResult(context.Background(),
		&persistence.Task{ID: "t-x", ProjectID: "p-1"},
		&persistence.Execution{ID: "exec-1"},
		"step-9", body)
	assert.Empty(t, loop, "no loop expected with diverse (tool, input) pairs")

	require.Len(t, ar.entries, 2)
	e0 := ar.entries[0]
	assert.Equal(t, "ta-1", e0.ID, "agent-supplied audit_id must round-trip into the DB row")
	assert.Equal(t, "p-1", e0.ProjectID)
	assert.Equal(t, "t-x", e0.TaskID)
	assert.Equal(t, "exec-1", e0.ExecutionID)
	assert.Equal(t, "step-9", e0.StepID)
	assert.Equal(t, "read_file", e0.ToolName)
	assert.Equal(t, int64(12), e0.DurationMs)

	e1 := ar.entries[1]
	assert.NotEmpty(t, e1.ID, "missing audit_id must be replaced with a fresh ID")
	assert.NotEqual(t, e0.ID, e1.ID)

	// Tool-call metrics: one per tool.
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("p-1", "read_file")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("p-1", "edit_file")))
}

// TestPersistToolAuditFromResult_RepoLogError — a single Log
// failure must not abort the loop; remaining entries still get
// attempted. The repo error is logged but persistToolAudit
// returns the loop detail (still "" in this case).
func TestPersistToolAuditFromResult_RepoLogError(t *testing.T) {
	ar := &stubAuditRepoY{logErr: errors.New("db down")}
	e := &Executor{auditRepo: ar, logger: zerolog.Nop()}
	body := mustMarshal(map[string]any{
		"toolAudit": []map[string]any{
			{"tool": "read", "input": "x"},
			{"tool": "write", "input": "y"},
		},
	})
	require.NotPanics(t, func() {
		_, _ = e.persistToolAuditFromResult(context.Background(),
			&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"},
			"step", body)
	})
}

// mustMarshal is a tiny inline helper — Marshal can't fail on
// the well-formed map literals these tests use, and t.Fatal on
// error keeps the test bodies readable.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("test marshal: %v", err))
	}
	return b
}

// TestCreateDelegatedTasks_HappyPath_Y — each spec becomes a child
// task with the expected envelope: project from parent, source
// set to DELEGATION, default mode = sequential, default attempt /
// max_attempts. Priority falls back to the parent's when the
// spec doesn't override.
func TestCreateDelegatedTasks_HappyPath_Y(t *testing.T) {
	tr := NewMockTaskRepo()
	e := &Executor{taskRepo: tr, logger: zerolog.Nop()}
	parent := &persistence.Task{
		ID:        "parent-1",
		ProjectID: "proj",
		Priority:  5,
	}
	specs := []delegatedTaskSpec{
		{Prompt: "Do A", Role: "researcher", Priority: 0}, // priority defaults to parent's 5
		{Prompt: "Do B", Role: "coder", Priority: 7},      // explicit priority overrides
	}
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeSequential))

	// Walk the in-memory task store to find the children.
	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.Len(t, tr.tasks, 2)

	bySpec := map[string]*persistence.Task{}
	for _, task := range tr.tasks {
		var p struct {
			Context map[string]any `json:"context"`
		}
		require.NoError(t, json.Unmarshal(task.Payload, &p))
		prompt, _ := p.Context["prompt"].(string)
		bySpec[prompt] = task
	}

	require.Contains(t, bySpec, "Do A")
	require.Contains(t, bySpec, "Do B")
	a := bySpec["Do A"]
	b := bySpec["Do B"]
	assert.Equal(t, "proj", a.ProjectID)
	assert.Equal(t, "parent-1", *a.ParentTaskID)
	assert.Equal(t, persistence.TaskStatusQueued, a.Status)
	assert.Equal(t, persistence.TaskCreationSourceDelegation, a.CreationSource)
	require.NotNil(t, a.DelegationMode)
	assert.Equal(t, persistence.DelegationModeSequential, *a.DelegationMode)
	assert.Equal(t, 5, a.Priority, "spec.Priority==0 must inherit parent's Priority")
	assert.Equal(t, 7, b.Priority, "explicit priority must override parent's")
	assert.Equal(t, 1, a.Attempt)
	assert.Equal(t, 3, a.MaxAttempts)
}

// TestCreateDelegatedTasks_RollbackOnPartialFailure — when the
// second spec's Create errors, the first child must be deleted
// so the parent isn't stuck waiting for orphan children to
// complete. This is the headline regression test.
func TestCreateDelegatedTasks_RollbackOnPartialFailure(t *testing.T) {
	tr := &failingOnNthTaskRepo{
		inner:  NewMockTaskRepo(),
		failOn: 2, // second Create errors
	}
	e := &Executor{taskRepo: tr, logger: zerolog.Nop()}
	parent := &persistence.Task{ID: "parent", ProjectID: "p"}
	specs := []delegatedTaskSpec{
		{Prompt: "child A", Role: "x"},
		{Prompt: "child B (fails)", Role: "y"},
	}
	err := e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeSequential)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create delegated task")

	// The first child was created then rolled back via Delete.
	tr.inner.mu.Lock()
	defer tr.inner.mu.Unlock()
	assert.Empty(t, tr.inner.tasks,
		"first child must be rolled back on partial failure — leaving zero in-flight children")
}

// failingOnNthTaskRepo wraps MockTaskRepo and makes the Nth (1-indexed)
// Create call fail. Used to drive the partial-failure branch in
// createDelegatedTasks. Other methods passthrough to inner.
type failingOnNthTaskRepo struct {
	inner       *MockTaskRepo
	mu          sync.Mutex
	createCalls int
	failOn      int
}

func (f *failingOnNthTaskRepo) Get(ctx context.Context, id string) (*persistence.Task, error) {
	return f.inner.Get(ctx, id)
}
func (f *failingOnNthTaskRepo) UpdateStatus(ctx context.Context, id string, s persistence.TaskStatus) error {
	return f.inner.UpdateStatus(ctx, id, s)
}
func (f *failingOnNthTaskRepo) Create(ctx context.Context, t *persistence.Task) error {
	f.mu.Lock()
	f.createCalls++
	n := f.createCalls
	f.mu.Unlock()
	if n == f.failOn {
		return errors.New("simulated create failure")
	}
	return f.inner.Create(ctx, t)
}
func (f *failingOnNthTaskRepo) Update(ctx context.Context, t *persistence.Task) error {
	return f.inner.Update(ctx, t)
}
func (f *failingOnNthTaskRepo) GetChildren(ctx context.Context, parentID string) ([]*persistence.Task, error) {
	return f.inner.GetChildren(ctx, parentID)
}
func (f *failingOnNthTaskRepo) Delete(ctx context.Context, id string) error {
	return f.inner.Delete(ctx, id)
}
func (f *failingOnNthTaskRepo) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	return f.inner.ReleaseLease(ctx, taskID, leaseID, newStatus, opts)
}
func (f *failingOnNthTaskRepo) TransitionConditional(ctx context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
	return f.inner.TransitionConditional(ctx, id, from, to, opts)
}

// TestCreateDelegatedTasks_EmptySpecs — degenerate input must
// short-circuit cleanly without touching the repo. Avoids a
// failure mode where an empty plan was reported as success but
// no children spawned, leaving the parent stuck.
func TestCreateDelegatedTasks_EmptySpecs(t *testing.T) {
	tr := NewMockTaskRepo()
	e := &Executor{taskRepo: tr, logger: zerolog.Nop()}
	require.NoError(t, e.createDelegatedTasks(context.Background(),
		&persistence.Task{ID: "parent", ProjectID: "p"},
		nil, persistence.DelegationModeSequential))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	assert.Empty(t, tr.tasks)
}
