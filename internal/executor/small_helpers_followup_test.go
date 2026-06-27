package executor

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// writableBuffer is a sync.Mutex-protected bytes.Buffer used as a
// zerolog destination from tests that race the logger across
// goroutines. Plain *bytes.Buffer panics under -race when one of
// the executor's background goroutines logs while the test reads.
type writableBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *writableBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *writableBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestLastStepOrUnknownY pins the public contract of the workflow's
// "what step did we finish on" helper used in operator-facing log
// lines. An empty slice must return the sentinel string rather than
// panicking on an out-of-range index.
func TestLastStepOrUnknownY(t *testing.T) {
	assert.Equal(t, "unknown", lastStepOrUnknown(nil))
	assert.Equal(t, "unknown", lastStepOrUnknown([]string{}))
	assert.Equal(t, "only", lastStepOrUnknown([]string{"only"}))
	assert.Equal(t, "last", lastStepOrUnknown([]string{"first", "middle", "last"}))
}

// TestRoleNamesY extracts the .Name field from a slice of SwarmRoles
// for error-message rendering. Output ordering must match input — the
// operator reads the list to compare against their swarm catalog so
// shuffling makes triage harder. Empty slice → empty (not nil) — the
// caller appends to the result so a nil here would still work but the
// type-stable empty slice is the established contract.
func TestRoleNamesY(t *testing.T) {
	got := roleNames([]registry.SwarmRole{
		{Name: "lead"},
		{Name: "researcher"},
		{Name: "writer"},
	})
	assert.Equal(t, []string{"lead", "researcher", "writer"}, got)
	assert.Equal(t, []string{}, roleNames(nil))
	assert.Equal(t, []string{}, roleNames([]registry.SwarmRole{}))
}

// TestGitHEAD_EmptyDir — the empty-directory guard short-circuits
// before invoking git, so no fork happens. Used at executor startup
// when a task has no worktreeDir wired yet.
func TestGitHEAD_EmptyDir(t *testing.T) {
	assert.Equal(t, "", gitHEAD(context.Background(), ""))
}

// TestGitHEAD_NotARepo — pointing at any non-git path returns
// empty (git rev-parse fails, the function tolerates).
func TestGitHEAD_NotARepo(t *testing.T) {
	// /tmp is reliably present and not a git repo on the test host.
	got := gitHEAD(context.Background(), t.TempDir())
	assert.Equal(t, "", got, "non-repo dir must yield empty string, not panic")
}

// TestSetMetrics_NilSafe — SetMetrics swaps the metrics sink on a
// running executor. The setter must accept nil (used by tests that
// strip observability after construction) and must be safe to call
// concurrently with a no-op execution.
func TestSetMetrics_NilSafe(t *testing.T) {
	e, _, _, _, _ := setup()
	require.NotNil(t, e)

	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)
	e.SetMetrics(m)
	assert.Same(t, m, e.metrics)

	// Replacing with nil clears the sink; subsequent code paths that
	// touch e.metrics use the package-wide nil-receiver guards.
	e.SetMetrics(nil)
	assert.Nil(t, e.metrics)
}

// TestSetHallucinationMetrics — the Phase 1 detector's metrics sink.
// Distinct setter from SetMetrics because hallucination metrics live
// in a separate type so the executor can be built without the
// hallucination package being instantiated.
func TestSetHallucinationMetrics_NilSafe(t *testing.T) {
	e, _, _, _, _ := setup()
	require.NotNil(t, e)

	registry := prometheus.NewRegistry()
	hm := hallucination.NewMetrics(registry)
	e.SetHallucinationMetrics(hm)
	assert.Same(t, hm, e.hallucinationMetrics)

	// nil executor — the setter must not panic. This path runs at
	// the boundary where service container wires observability into
	// an executor that may have failed construction.
	var nilExec *Executor
	nilExec.SetHallucinationMetrics(hm) // must not panic
}

// TestRecordToolCalls — metrics.RecordToolCalls feeds the
// tool_calls_total counter labeled by (project_id, tool). nil
// receiver is the configured-without-metrics path; non-nil
// receiver with empty map is a no-op.
func TestRecordToolCalls(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// Empty map — counters untouched.
	m.RecordToolCalls("proj", nil)
	m.RecordToolCalls("proj", map[string]int{})

	// Real increments emit one Add per tool. Reading them back via
	// testutil pins the (project_id, tool) label cardinality the
	// dashboards expect.
	m.RecordToolCalls("proj", map[string]int{"read_file": 3, "edit_file": 1})
	assert.Equal(t, 3.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("proj", "read_file")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ToolCallsTotal.WithLabelValues("proj", "edit_file")))

	// nil receiver — the package-wide no-op guard.
	var nilM *Metrics
	nilM.RecordToolCalls("proj", map[string]int{"x": 1})
}

// TestNewCircuitBreakerForExecutor_HappyPath — the exported
// constructor used by the service container. Mirrors
// newCircuitBreaker but is the public surface, so it gets its
// own coverage to lock the wiring in.
func TestNewCircuitBreakerForExecutor_HappyPath(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountRecentFailuresFunc: func(_ context.Context, _ string, _ []string, _ time.Time) (int, error) {
			return 0, nil
		},
	}
	autonomy := &fakeAutonomyController{}
	cb := NewCircuitBreakerForExecutor(repo, autonomy, nil, 5, time.Hour, []string{"CANCELLED"}, zerolog.Nop())
	require.NotNil(t, cb, "constructor must return a non-nil breaker for valid args")
	assert.Equal(t, 5, cb.threshold)
	assert.Equal(t, time.Hour, cb.window)
	_, skipped := cb.skipClasses["CANCELLED"]
	assert.True(t, skipped, "skipClasses must include the supplied entries")
}

// TestNewCircuitBreakerForExecutor_NilOnInvalidInputs — the
// constructor returns nil rather than a misconfigured breaker. The
// container treats nil as "disabled" so this guard prevents a
// half-built breaker from silently no-op-ing Trip calls.
func TestNewCircuitBreakerForExecutor_NilOnInvalidInputs(t *testing.T) {
	autonomy := &fakeAutonomyController{}
	repo := &mocks.MockTaskRepository{}

	// nil taskRepo — disabled
	assert.Nil(t, NewCircuitBreakerForExecutor(nil, autonomy, nil, 5, time.Hour, nil, zerolog.Nop()))
	// nil autonomy — disabled
	assert.Nil(t, NewCircuitBreakerForExecutor(repo, nil, nil, 5, time.Hour, nil, zerolog.Nop()))
	// threshold ≤ 0 — disabled
	assert.Nil(t, NewCircuitBreakerForExecutor(repo, autonomy, nil, 0, time.Hour, nil, zerolog.Nop()))
	// window ≤ 0 — disabled
	assert.Nil(t, NewCircuitBreakerForExecutor(repo, autonomy, nil, 5, 0, nil, zerolog.Nop()))
}

// TestWithLogger — option setter stashes the logger on the
// executor. Pin the option ran by writing through the logger and
// asserting bytes landed in the destination io.Writer; comparing
// zerolog.Logger values directly is brittle (Context is opaque).
func TestWithLogger_Option(t *testing.T) {
	buf := &writableBuffer{}
	custom := zerolog.New(buf).Level(zerolog.InfoLevel)
	e := NewWithOptions(nil, nil, nil, nil, nil, WithLogger(custom))
	require.NotNil(t, e)
	e.logger.Info().Str("scope", "with_logger_test").Msg("hello")
	assert.Contains(t, buf.String(), "with_logger_test",
		"WithLogger must wire the supplied logger; emission would otherwise land in zerolog.Nop")
}

// TestSetCircuitBreaker — late-binding setter exists alongside the
// WithCircuitBreaker option because the service container builds the
// breaker only after both the notifier and registry are wired.
func TestSetCircuitBreaker_Setter(t *testing.T) {
	e, _, _, _, _ := setup()
	cb := newCircuitBreaker(
		&mocks.MockTaskRepository{},
		&fakeAutonomyController{},
		nil, 5, time.Hour, nil, zerolog.Nop(),
	)
	e.SetCircuitBreaker(cb)
	assert.Same(t, cb, e.circuitBreaker)

	// nil receiver — must not panic. The container uses this
	// setter inside best-effort wiring blocks where partial init
	// may leave the executor pointer nil.
	var nilExec *Executor
	nilExec.SetCircuitBreaker(cb) // must not panic
}

// TestSetPricing_NilSafe — pricing swap at runtime, nil-clears.
// Used by config-reload. nil-receiver branch covers the
// container's best-effort wiring path.
func TestSetPricing_NilSafe(t *testing.T) {
	e, _, _, _, _ := setup()
	tbl := &pricing.Table{}
	e.SetPricing(tbl)
	assert.Same(t, tbl, e.pricing)

	e.SetPricing(nil)
	assert.Nil(t, e.pricing)

	var nilExec *Executor
	nilExec.SetPricing(tbl) // must not panic
}

// TestIsTaskCancelled_True — the cancellation check the executor
// runs between steps. Used by runExecution + run* loops to bail
// out without writing FAILED when the operator cancelled mid-run.
func TestIsTaskCancelled_True(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t-cancel",
		ProjectID: "p1",
		Status:    persistence.TaskStatusCancelled,
		CreatedAt: time.Now(),
	})
	assert.True(t, e.isTaskCancelled(context.Background(), "t-cancel"),
		"CANCELLED task must be recognized so the executor bails early")
}

// TestIsTaskCancelled_NonCancelled — every non-cancelled status
// must return false. Same task lookup, different statuses.
func TestIsTaskCancelled_NonCancelled(t *testing.T) {
	e, _, _, _, tr := setup()
	for _, s := range []persistence.TaskStatus{
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusAwaitingInput,
	} {
		id := "t-" + string(s)
		tr.AddTask(&persistence.Task{
			ID: id, ProjectID: "p1", Status: s, CreatedAt: time.Now(),
		})
		assert.False(t, e.isTaskCancelled(context.Background(), id),
			"status %s must NOT be classified as cancelled", s)
	}
}

// TestIsTaskCancelled_MissingTask — repo returns (nil, nil). The
// check must return false: no task means nothing to cancel
// against, and we'd rather false-negative here than false-positive
// (a false-positive bails the executor unnecessarily).
func TestIsTaskCancelled_MissingTask(t *testing.T) {
	e, _, _, _, _ := setup()
	assert.False(t, e.isTaskCancelled(context.Background(), "no-such-id"))
}

// TestCleanExcludesFor_NilWorkflowsAndEmptyID — defensive
// branches: when the registry is unwired or the projectID is
// empty, the helper returns nil so cleanProjectDir doesn't try
// to extend a nil-receiver excludes slice.
func TestCleanExcludesFor_EarlyReturns(t *testing.T) {
	e, _, _, _, _ := setup()
	// No workflows resolver wired — returns nil.
	assert.Nil(t, e.cleanExcludesFor("p1"))

	// Workflows wired but empty projectID.
	e.SetWorkflowResolver(&MockWorkflowResolver{projects: map[string]*registry.Project{}})
	assert.Nil(t, e.cleanExcludesFor(""))

	// Workflows wired, project unknown — returns nil.
	assert.Nil(t, e.cleanExcludesFor("missing"))
}

// TestCleanExcludesFor_ContextPathsOutsideDefault — operator
// pointed Autonomy.ContextFilePath outside .autonomy/, so the
// helper must surface that directory in the excludes list to
// keep cleanProjectDir from wiping the operator's untracked
// context file.
func TestCleanExcludesFor_ContextOutsideDefault(t *testing.T) {
	e, _, _, _, _ := setup()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID: "p1",
				Autonomy: registry.ProjectAutonomy{
					ContextFilePath:     "ops/notes.md",
					UserContextFilePath: "ops/user_notes.md",
				},
			},
		},
	})
	got := e.cleanExcludesFor("p1")
	// Both ops/ paths sit OUTSIDE the default .autonomy/, so the
	// helper must surface the "ops" parent so cleanProjectDir
	// adds it to its blanket-clean excludes. The same parent
	// appearing twice (once per ContextFilePath / UserContextFilePath)
	// is fine — cleanProjectDir de-dupes on the cleanup side.
	require.Contains(t, got, "ops", "operator-overridden ContextFilePath outside .autonomy/ must surface its dir")
	require.NotEmpty(t, got, "result must include at least one exclude")
}

// TestCleanExcludesFor_DefaultPaths — both paths sit under the
// default .autonomy/, which is already covered by the blanket
// excludes inside cleanProjectDir. The helper returns nil in
// this case so we don't double-cover.
func TestCleanExcludesFor_DefaultPaths(t *testing.T) {
	e, _, _, _, _ := setup()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID: "p1",
				Autonomy: registry.ProjectAutonomy{
					ContextFilePath:     ".autonomy/context.md",
					UserContextFilePath: ".autonomy/user_context.md",
				},
			},
		},
	})
	assert.Nil(t, e.cleanExcludesFor("p1"))
}
