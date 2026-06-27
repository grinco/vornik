package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"

	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/workspacelock"
)

// TestWithWorkspaceLock_InjectsSharedInstance — the option installs the
// supplied Locker, and the executor's per-project lock sites take THAT
// instance (so an external holder of the same project key blocks them).
func TestWithWorkspaceLock_InjectsSharedInstance(t *testing.T) {
	shared := workspacelock.New()
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithWorkspaceLock(shared))

	// The executor must reach the SAME instance via wsLock().
	if e.wsLock() != shared {
		t.Fatal("WithWorkspaceLock did not install the shared instance")
	}
	// Holding the shared lock for a project must block a TryLock on the
	// same key obtained through the executor's lock.
	unlock := shared.Lock("proj")
	if _, ok := e.wsLock().TryLock("proj"); ok {
		t.Fatal("executor lock is not the shared instance — TryLock succeeded while shared Lock held")
	}
	unlock()
}

// TestWithWorkspaceLock_NilIgnored — passing a nil Locker leaves the
// constructor's fallback in place (never nil).
func TestWithWorkspaceLock_NilIgnored(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithWorkspaceLock(nil))
	if e.wsLock() == nil {
		t.Fatal("constructor fallback should leave a non-nil workspace lock")
	}
}

// TestPruneAllWorktrees_LocksPerProject — the startup prune is a
// workspace writer; it must take the per-project lock around EACH
// project (not one hold across all). We pre-hold one project's lock on
// the SAME injected Locker; the prune must block on that project until
// released, proving per-project lock-on-mutation.
func TestPruneAllWorktrees_LocksPerProject(t *testing.T) {
	shared := workspacelock.New()
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithWorkspaceLock(shared))

	tmp := t.TempDir()
	e.config.ProjectWorkspacePath = tmp
	if err := os.MkdirAll(filepath.Join(tmp, "proj-a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Hold proj-a's lock; prune must block until we release.
	release := shared.Lock("proj-a")
	done := make(chan struct{})
	go func() {
		e.pruneAllWorktrees(context.Background(), nil)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("prune completed while proj-a's workspace lock was held — prune did not lock per-project")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("prune did not finish after releasing proj-a's lock")
	}
}

// TestWithLogger_Executor — pin the option lands the supplied logger
// on the executor so production wiring's structured logger reaches
// dispatch / step / retry paths.
func TestWithLogger_Executor(t *testing.T) {
	want := zerolog.Nop().With().Str("test", "x").Logger()
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithLogger(want))
	assert.Equal(t, want.GetLevel(), e.logger.GetLevel())
}

func TestWithTracer_Executor(t *testing.T) {
	tracer := otel.Tracer("test-tracer-executor")
	e, _, _, _, _ := setup()
	WithTracer(tracer)(e)
	assert.Equal(t, tracer, e.tracer)
}

// TestSetCircuitBreaker — late-binding setter pairs with
// WithCircuitBreaker for the case where the breaker is built after
// the executor.
func TestSetCircuitBreaker(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.Nil(t, e.circuitBreaker)

	autonomy := &stubAutonomyController{}
	cb := newCircuitBreaker(&mocks.MockTaskRepository{}, autonomy, nil, 3, time.Minute, nil, zerolog.Nop())
	e.SetCircuitBreaker(cb)
	assert.Same(t, cb, e.circuitBreaker)

	// nil receiver is a no-op.
	var eNil *Executor
	assert.NotPanics(t, func() { eNil.SetCircuitBreaker(cb) })
}

// stubAutonomyController is the minimal AutonomyController for tests
// that need a non-nil instance.
type stubAutonomyController struct{}

func (*stubAutonomyController) SetProjectAutonomyEnabled(string, bool) error { return nil }

// TestSetPricing — same pattern as SetCircuitBreaker for the
// pricing table.
func TestSetPricing(t *testing.T) {
	e, _, _, _, _ := setup()
	tab := &pricing.Table{}
	e.SetPricing(tab)
	assert.Same(t, tab, e.pricing)

	// Nil receiver — safe no-op.
	var eNil *Executor
	assert.NotPanics(t, func() { eNil.SetPricing(tab) })

	// Nil table — disables pricing.
	e.SetPricing(nil)
	assert.Nil(t, e.pricing)
}

func TestSetMetrics_Executor(t *testing.T) {
	e, _, _, _, _ := setup()
	m := NewMetrics(prometheus.NewRegistry())
	e.SetMetrics(m)
	assert.Same(t, m, e.metrics)
	// nil overrides cleanly.
	e.SetMetrics(nil)
	assert.Nil(t, e.metrics)
}

func TestSetHallucinationMetrics_Executor(t *testing.T) {
	e, _, _, _, _ := setup()
	m := &hallucination.Metrics{}
	e.SetHallucinationMetrics(m)
	assert.Same(t, m, e.hallucinationMetrics)

	// Nil receiver — safe no-op.
	var eNil *Executor
	assert.NotPanics(t, func() { eNil.SetHallucinationMetrics(m) })
}

func TestSetCompletionNotifier(t *testing.T) {
	e, _, _, _, _ := setup()
	n := &captureNotifier{}
	e.SetCompletionNotifier(n)
	assert.NotNil(t, e.notifier)

	// Nil receiver — safe no-op.
	var eNil *Executor
	assert.NotPanics(t, func() { eNil.SetCompletionNotifier(n) })
}

// captureNotifier records every NotifyTaskCompleted call so the
// SetCompletionNotifier test can assert the notifier round-tripped.
type captureNotifier struct {
	calls int
}

func (c *captureNotifier) NotifyTaskCompleted(_ context.Context, _ *persistence.Task, _ bool, _ string) {
	c.calls++
}

func TestNewCircuitBreakerForExecutor(t *testing.T) {
	repo := &mocks.MockTaskRepository{}
	autonomy := &stubAutonomyController{}

	// threshold=0 → returns nil so the executor's circuit-check path no-ops.
	cb := NewCircuitBreakerForExecutor(repo, autonomy, nil, 0, time.Minute, nil, zerolog.Nop())
	assert.Nil(t, cb)

	// Missing autonomy controller → nil.
	cb = NewCircuitBreakerForExecutor(repo, nil, nil, 3, time.Minute, nil, zerolog.Nop())
	assert.Nil(t, cb)

	// Zero window → nil.
	cb = NewCircuitBreakerForExecutor(repo, autonomy, nil, 3, 0, nil, zerolog.Nop())
	assert.Nil(t, cb)

	// Missing task repo → nil.
	cb = NewCircuitBreakerForExecutor(nil, autonomy, nil, 3, time.Minute, nil, zerolog.Nop())
	assert.Nil(t, cb)

	// Well-formed config → returns a populated breaker.
	cb = NewCircuitBreakerForExecutor(repo, autonomy, nil, 3, time.Minute, []string{"cancelled"}, zerolog.Nop())
	assert.NotNil(t, cb)
}

func TestIsTaskCancelled(t *testing.T) {
	e, _, _, _, tr := setup()
	ctx := context.Background()

	// Task exists with cancelled status → true.
	tr.AddTask(&persistence.Task{ID: "t1", Status: persistence.TaskStatusCancelled})
	assert.True(t, e.isTaskCancelled(ctx, "t1"))

	// Task exists with non-cancelled status → false.
	tr.AddTask(&persistence.Task{ID: "t2", Status: persistence.TaskStatusRunning})
	assert.False(t, e.isTaskCancelled(ctx, "t2"))

	// Repo error → false (defensive default; don't mistake a transient
	// failure for a cancellation).
	tr.err = errors.New("db down")
	assert.False(t, e.isTaskCancelled(ctx, "t1"))
	tr.err = nil
}

func TestCleanExcludesFor(t *testing.T) {
	e, _, _, _, _ := setup()
	// No workflows resolver → nil.
	assert.Nil(t, e.cleanExcludesFor("p1"))

	// Empty project ID → nil even with a resolver.
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {Autonomy: registry.ProjectAutonomy{
				ContextFilePath:     "operator/notes.md",
				UserContextFilePath: ".autonomy/user.md",
			}},
		},
	}
	e.workflows = resolver
	assert.Nil(t, e.cleanExcludesFor(""))

	// Unknown project ID — resolver returns nil → method returns nil.
	assert.Nil(t, e.cleanExcludesFor("unknown"))

	// Known project: only the non-.autonomy/ context path contributes
	// an extra exclude. .autonomy/user.md is the default scope so it
	// doesn't add a fresh entry.
	got := e.cleanExcludesFor("p1")
	assert.Equal(t, []string{"operator"}, got)
}

// TestTaskLogs_RequiresID — entry-point validation. No repo / runtime
// touch when the ID is empty.
func TestTaskLogs_RequiresID(t *testing.T) {
	e, _, _, _, _ := setup()
	_, err := e.TaskLogs(context.Background(), "", 10)
	assert.Error(t, err)
}

// TestTaskLogs_NoActiveExecutionFallsBackToContainerLookup — when
// the task isn't in activeExecutions, TaskLogs queries the runtime
// for a matching container. Without one, it returns a not-found
// error so the operator sees the real reason.
func TestTaskLogs_NoActiveExecutionFallsBackToContainerLookup(t *testing.T) {
	e, rt, _, _, _ := setup()
	_ = rt // MockRuntime.GetContainerByTask returns nil by default.
	_, err := e.TaskLogs(context.Background(), "task-x", 10)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no container found")
}

// TestTaskLogs_ActiveExecutionUsesContainerID — when the task IS
// active and has a containerID, TaskLogs calls runtime.Logs.
func TestTaskLogs_ActiveExecutionUsesContainerID(t *testing.T) {
	e, rt, _, _, _ := setup()
	// Stuff in an active execution with a containerID.
	e.mu.Lock()
	e.activeExecutions["task-x"] = &executionHandle{taskID: "task-x", containerID: "c1"}
	e.mu.Unlock()
	_, err := e.TaskLogs(context.Background(), "task-x", 5)
	assert.NoError(t, err)
	_ = rt
}

// TestTaskLogs_RegisteredContainerLookupHit — no activeExecution,
// but the runtime returns a registered container for the task —
// runtime.Logs is called.
func TestTaskLogs_RegisteredContainerLookupHit(t *testing.T) {
	e, rt, _, _, _ := setup()
	rt.registerLiveContainer("task-y")
	out, err := e.TaskLogs(context.Background(), "task-y", 5)
	assert.NoError(t, err)
	// MockRuntime.Logs returns empty by default.
	assert.Equal(t, "", out)
}

// TestPruneAllWorktrees_EmptyConfigIsNoop — no ProjectWorkspacePath
// configured → early return.
func TestPruneAllWorktrees_EmptyConfigIsNoop(t *testing.T) {
	e, _, _, _, _ := setup()
	// Default config has no ProjectWorkspacePath in the test setup.
	e.config.ProjectWorkspacePath = ""
	assert.NotPanics(t, func() {
		e.pruneAllWorktrees(context.Background(), nil)
	})
}

func TestPruneAllWorktrees_ReadDirErrorIsSilentReturn(t *testing.T) {
	e, _, _, _, _ := setup()
	// Pointing at a non-existent path → os.ReadDir errors → silent return.
	e.config.ProjectWorkspacePath = "/var/no/such/directory/xyz123"
	assert.NotPanics(t, func() {
		e.pruneAllWorktrees(context.Background(), nil)
	})
}

func TestPruneAllWorktrees_IteratesProjectDirs(t *testing.T) {
	e, _, _, _, _ := setup()
	tmp := t.TempDir()
	e.config.ProjectWorkspacePath = tmp
	// Create a couple of project subdirectories plus a stray file.
	require := func(err error) {
		if err != nil {
			t.Fatalf("setup failed: %v", err)
		}
	}
	require(os.MkdirAll(filepath.Join(tmp, "proj-a"), 0o755))
	require(os.MkdirAll(filepath.Join(tmp, "proj-b"), 0o755))
	// Stray file at the top level — should be skipped.
	require(os.WriteFile(filepath.Join(tmp, "stray.txt"), []byte("x"), 0o600))

	// No worktrees inside → pruneWorktrees no-ops on each. The
	// outer loop's reachability is what we care about.
	assert.NotPanics(t, func() {
		e.pruneAllWorktrees(context.Background(), map[string]struct{}{"keep-id": {}})
	})
}

// TestShutdown_NoActiveExecutions — Shutdown delegates to Stop when
// there are no active executions to pause. Validates the early-return
// path without engaging the pause-and-resume machinery.
func TestShutdown_NoActiveExecutions(t *testing.T) {
	e, _, _, _, _ := setup()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := e.Shutdown(ctx)
	assert.NoError(t, err)

	// Nil receiver — safe no-op.
	var eNil *Executor
	assert.NoError(t, eNil.Shutdown(ctx))
}
