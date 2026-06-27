package autonomy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// stubBudgetNotifier satisfies budget.Notifier. The autonomy options
// tests don't drive evaluation, so no calls are expected — the
// implementation just needs to satisfy the interface so WithBudgetNotifier
// type-checks.
type stubBudgetNotifier struct{}

func (s *stubBudgetNotifier) NotifyBudgetBreach(_ context.Context, _, _, _ string, _ budget.Decision) {
}

// registryWithProject builds a registry with a single project + swarm
// loaded from a tempdir. Wraps the boilerplate of writing YAML files
// so each test reads as the scenario it cares about, not the I/O
// setup. Returns the loaded registry; the tempdir lives until the
// test exits.
func registryWithProject(t *testing.T, project string, autonomyYAML string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s.md"), []byte(`---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "vornik-agent:latest" }
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "w.md"), []byte(`---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", project+".yaml"),
		[]byte(fmt.Sprintf("projectId: %q\nswarmId: \"s\"\ndefaultWorkflowId: \"w\"\n%s", project, autonomyYAML)), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

// --------------------------------------------------------------------
// Option-style constructors (was 0% across the board).
// --------------------------------------------------------------------
// Each option mutates exactly one Manager field. Pin them as a single
// table so future option additions extend by a single row.

func TestWithOptions_AssignFields(t *testing.T) {
	logger := zerolog.New(nil).With().Logger()
	metrics := &Metrics{} // zero value is fine; we only check the pointer round-trips
	pTable := pricing.Empty()
	notifier := &stubBudgetNotifier{}

	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithLogger(logger),
		WithMetrics(metrics),
		WithToolAuditRepository(nil),
		WithBudgetNotifier(notifier),
		WithPricing(pTable),
		WithDefaultModel("kimi-k2.5"),
		WithWorkspacePath("/var/workspaces"),
		WithDefaultEvaluateTimeout(7*time.Minute),
	)

	assert.Same(t, metrics, m.metrics, "WithMetrics must assign the metrics pointer")
	assert.Same(t, notifier, m.budgetNotifier, "WithBudgetNotifier must assign the notifier")
	assert.Same(t, pTable, m.pricing, "WithPricing must assign the pricing table pointer")
	assert.Equal(t, "kimi-k2.5", m.defaultModel)
	assert.Equal(t, "/var/workspaces", m.workspacePath)
	assert.Equal(t, 7*time.Minute, m.defaultTimeout)
}

// TestNew_DefaultsWhenNoOptions pins the zero-option happy path: the
// constructor still allocates the per-project bookkeeping maps and
// applies a sensible default evaluation timeout. Regressions here
// (e.g. nil maps) would surface as panics later in projectLoop.
func TestNew_DefaultsWhenNoOptions(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	require.NotNil(t, m)
	assert.NotNil(t, m.taskCounts, "taskCounts map must be allocated")
	assert.NotNil(t, m.hourReset, "hourReset map must be allocated")
	assert.NotNil(t, m.cancelFns, "cancelFns map must be allocated")
	assert.Equal(t, 5*time.Minute, m.defaultTimeout, "defaultTimeout must fall back to 5m when no option is given")
}

// --------------------------------------------------------------------
// SetMetrics / SetWorkspacePath — runtime setters used by the service
// container after observability initialises.
// --------------------------------------------------------------------

func TestSetMetrics_UpdatesAfterConstruction(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	assert.Nil(t, m.metrics)
	metrics := NewMetrics(prometheus.NewRegistry())
	m.SetMetrics(metrics)
	assert.Same(t, metrics, m.metrics)
}

func TestSetWorkspacePath_UpdatesAfterConstruction(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	assert.Empty(t, m.workspacePath)
	m.SetWorkspacePath("/var/vornik/workspaces")
	assert.Equal(t, "/var/vornik/workspaces", m.workspacePath)
}

// --------------------------------------------------------------------
// ActiveLoops / Reload — start/stop topology.
// --------------------------------------------------------------------

func TestActiveLoops_EmptyManagerReportsZero(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	assert.Equal(t, 0, m.ActiveLoops())
}

func TestReload_NilRegistrySafe(t *testing.T) {
	// Reload should never panic on a nil-registry manager; with no
	// active loops it's effectively a no-op.
	m := &Manager{logger: zerolog.Nop(), cancelFns: map[string]context.CancelFunc{}, taskCounts: map[string]int{}, hourReset: map[string]time.Time{}}
	require.NotPanics(t, m.Reload)
	assert.Equal(t, 0, m.ActiveLoops())
}

func TestReload_RestartsActiveLoops(t *testing.T) {
	// Build a registry with one autonomy-enabled project, start the
	// manager, reload, confirm the loop count is preserved (the loop
	// is restarted, not lost).
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  goal: "do stuff"
  pollInterval: "1h"  # long enough that no tick fires during the test
`)
	m := New(nil, reg, &mockTaskRepo{}, nil)
	m.Start()
	t.Cleanup(m.Stop)

	// Give the goroutine a moment to register itself.
	require.Eventually(t, func() bool { return m.ActiveLoops() == 1 }, 2*time.Second, 10*time.Millisecond,
		"projectLoop must register its cancel before ActiveLoops() converges")

	before := m.ActiveLoops()
	m.Reload()
	require.Eventually(t, func() bool { return m.ActiveLoops() == before }, 2*time.Second, 10*time.Millisecond,
		"Reload must re-register the same loop count")
}

// --------------------------------------------------------------------
// IsAutonomyEnabled — registry-passthrough getter.
// --------------------------------------------------------------------

func TestIsAutonomyEnabled_NilRegistry(t *testing.T) {
	m := &Manager{}
	assert.False(t, m.IsAutonomyEnabled("anything"), "nil registry must report disabled, not panic")
}

func TestIsAutonomyEnabled_UnknownProject(t *testing.T) {
	m := New(nil, registry.New(), &mockTaskRepo{}, nil)
	assert.False(t, m.IsAutonomyEnabled("does-not-exist"))
}

func TestIsAutonomyEnabled_ReadsFromRegistry(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s.md"), []byte(`---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "x" }
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "w.md"), []byte(`---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "p1.yaml"), []byte(`projectId: "p1"
swarmId: "s"
defaultWorkflowId: "w"
autonomy:
  enabled: true
  goal: "x"
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "p2.yaml"), []byte(`projectId: "p2"
swarmId: "s"
defaultWorkflowId: "w"
autonomy:
  enabled: false
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	m := New(nil, reg, &mockTaskRepo{}, nil)
	assert.True(t, m.IsAutonomyEnabled("p1"))
	assert.False(t, m.IsAutonomyEnabled("p2"))
}

// --------------------------------------------------------------------
// EnableProject / DisableProject — runtime toggles used by /autopilot.
// --------------------------------------------------------------------

func TestEnableProject_NilRegistryErrors(t *testing.T) {
	m := &Manager{}
	err := m.EnableProject("p1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry not configured")
}

func TestEnableProject_UnknownProject(t *testing.T) {
	m := New(nil, registry.New(), &mockTaskRepo{}, nil)
	err := m.EnableProject("ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestEnableProject_RequiresGoalForLLMMode(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: false
`)
	m := New(nil, reg, &mockTaskRepo{}, nil)
	err := m.EnableProject("p1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no autonomy.goal configured")
}

func TestEnableProject_StartsLoop(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: false  # disabled in YAML; enable via API
  goal: "do stuff"
  pollInterval: "1h"
`)
	m := New(nil, reg, &mockTaskRepo{}, nil)
	t.Cleanup(m.Stop)

	require.NoError(t, m.EnableProject("p1"))
	require.Eventually(t, func() bool { return m.ActiveLoops() == 1 }, 2*time.Second, 10*time.Millisecond)

	// Registry-side flag must flip too — otherwise IsAutonomyEnabled
	// would diverge from the live loop state.
	assert.True(t, m.IsAutonomyEnabled("p1"))
}

func TestEnableProject_BacklogModeWithoutGoal(t *testing.T) {
	// Backlog mode reads prompts from BACKLOG.md, so Goal is not
	// required for EnableProject to succeed.
	reg := registryWithProject(t, "p-backlog", `autonomy:
  mode: "backlog"
  pollInterval: "1h"
`)
	m := New(nil, reg, &mockTaskRepo{}, nil)
	t.Cleanup(m.Stop)

	require.NoError(t, m.EnableProject("p-backlog"))
	require.Eventually(t, func() bool { return m.ActiveLoops() == 1 }, 2*time.Second, 10*time.Millisecond)
}

func TestDisableProject_NilRegistryErrors(t *testing.T) {
	m := &Manager{}
	err := m.DisableProject("p1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry not configured")
}

func TestDisableProject_UnknownProject(t *testing.T) {
	m := New(nil, registry.New(), &mockTaskRepo{}, nil)
	err := m.DisableProject("ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDisableProject_StopsLoop(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  goal: "do stuff"
  pollInterval: "1h"
`)
	m := New(nil, reg, &mockTaskRepo{}, nil)
	m.Start()
	t.Cleanup(m.Stop)
	require.Eventually(t, func() bool { return m.ActiveLoops() == 1 }, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, m.DisableProject("p1"))
	require.Eventually(t, func() bool { return m.ActiveLoops() == 0 }, 2*time.Second, 10*time.Millisecond)
	assert.False(t, m.IsAutonomyEnabled("p1"))
}

// --------------------------------------------------------------------
// Start — guard branches.
// --------------------------------------------------------------------

func TestStart_NilRegistryIsNoop(t *testing.T) {
	m := &Manager{logger: zerolog.Nop(), cancelFns: map[string]context.CancelFunc{}}
	require.NotPanics(t, m.Start)
	assert.Equal(t, 0, len(m.cancelFns))
}

func TestStart_SkipsProjectsWithoutGoalInLLMMode(t *testing.T) {
	// One LLM-mode project with no goal + one with a goal. Only the
	// project with a goal should spawn a loop.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s.md"), []byte(`---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "x" }
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "w.md"), []byte(`---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "no-goal.yaml"), []byte(`projectId: "no-goal"
swarmId: "s"
defaultWorkflowId: "w"
autonomy:
  enabled: true
  pollInterval: "1h"
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "has-goal.yaml"), []byte(`projectId: "has-goal"
swarmId: "s"
defaultWorkflowId: "w"
autonomy:
  enabled: true
  goal: "x"
  pollInterval: "1h"
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	m := New(nil, reg, &mockTaskRepo{}, nil)
	m.Start()
	t.Cleanup(m.Stop)
	require.Eventually(t, func() bool { return m.ActiveLoops() == 1 }, 2*time.Second, 10*time.Millisecond,
		"only the project with a goal should spawn a loop")
}

// --------------------------------------------------------------------
// extractPrompt / extractResultMessage — payload helpers (were 0%).
// --------------------------------------------------------------------

func TestExtractPrompt_HappyPath(t *testing.T) {
	payload := []byte(`{"context":{"prompt":"hello world"}}`)
	assert.Equal(t, "hello world", extractPrompt(payload))
}

func TestExtractPrompt_EmptyAndInvalid(t *testing.T) {
	assert.Empty(t, extractPrompt(nil))
	assert.Empty(t, extractPrompt([]byte{}))
	assert.Empty(t, extractPrompt([]byte("not json")))
	// Missing field returns "" (no error).
	assert.Empty(t, extractPrompt([]byte(`{"other":"value"}`)))
}

func TestExtractResultMessage_HappyPath(t *testing.T) {
	result := []byte(`{"message":"all done"}`)
	assert.Equal(t, "all done", extractResultMessage(result))
}

func TestExtractResultMessage_EmptyAndInvalid(t *testing.T) {
	assert.Empty(t, extractResultMessage(nil))
	assert.Empty(t, extractResultMessage([]byte{}))
	assert.Empty(t, extractResultMessage([]byte("not json")))
	assert.Empty(t, extractResultMessage([]byte(`{"other":"value"}`)))
}

// TestHashPrompt covers the SHA-256 prefix path + the empty-string
// short-circuit. The classifier in recordEvaluation depends on the
// empty path returning "" rather than the empty-string hash so
// "I forgot to set the prompt" doesn't collide with "the prompt is
// genuinely empty by design".
func TestHashPrompt_EmptyShortCircuits(t *testing.T) {
	assert.Empty(t, hashPrompt(""))
	assert.Empty(t, hashPrompt("   "), "all-whitespace also short-circuits")
}

func TestHashPrompt_StableAcrossCalls(t *testing.T) {
	h1 := hashPrompt("identical prompt")
	h2 := hashPrompt("identical prompt")
	assert.Equal(t, h1, h2, "hash must be deterministic")
	assert.Len(t, h1, 16, "16 hex chars = 8 bytes of SHA-256 prefix")

	different := hashPrompt("a different prompt")
	assert.NotEqual(t, h1, different)
}

// TestTruncate covers both branches of the small helper used by
// recordEvaluation when persisting reason strings to the audit table.
func TestTruncate_BothBranches(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 10), "no truncation when len ≤ max")
	assert.Equal(t, "abc...", truncate("abcdefgh", 3), "truncation appends ... separator")
	// Boundary: len == max returns the original.
	assert.Equal(t, "exact", truncate("exact", 5))
}

// --------------------------------------------------------------------
// recordEvaluation — nil-safety branches.
// --------------------------------------------------------------------

func TestRecordEvaluation_NilManagerSafe(t *testing.T) {
	// The nil-receiver guard exists so callers don't have to NPE-check
	// every recordEvaluation site after a defensive refactor.
	var m *Manager
	require.NotPanics(t, func() {
		m.recordEvaluation(context.Background(), evalRecord{projectID: "p1"})
	})
}

func TestRecordEvaluation_NilRepoSafe(t *testing.T) {
	// Repo not wired → recordEvaluation must not panic, must not log
	// an error, must just return. The autonomy loop runs without the
	// audit repo in legacy deployments.
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	require.Nil(t, m.evalRepo)
	require.NotPanics(t, func() {
		m.recordEvaluation(context.Background(), evalRecord{projectID: "p1", outcome: "skip"})
	})
}

// stubEvalRepo captures Record calls so tests can assert what the
// audit-trail rows look like at this layer.
type stubEvalRepo struct {
	recorded []*persistence.AutonomyEvaluation
	recordFn func(*persistence.AutonomyEvaluation) error
}

func (s *stubEvalRepo) Record(_ context.Context, e *persistence.AutonomyEvaluation) error {
	if s.recordFn != nil {
		if err := s.recordFn(e); err != nil {
			return err
		}
	}
	s.recorded = append(s.recorded, e)
	return nil
}
func (s *stubEvalRepo) List(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	return nil, nil
}
func (s *stubEvalRepo) CountByOutcome(_ context.Context, _ string, _, _ time.Time) (map[string]int64, error) {
	return nil, nil
}

// --------------------------------------------------------------------
// autonomyContextFilePath — path-safety / defaulting helper.
// --------------------------------------------------------------------

func TestAutonomyContextFilePath_DefaultAndOverrides(t *testing.T) {
	const def = ".autonomy/PROJECT_CONTEXT.md"

	// nil project → default.
	assert.Equal(t, def, autonomyContextFilePath(nil))

	// Empty / whitespace → default.
	assert.Equal(t, def, autonomyContextFilePath(&registry.Project{}))
	assert.Equal(t, def, autonomyContextFilePath(&registry.Project{
		Autonomy: registry.ProjectAutonomy{ContextFilePath: "   "},
	}))

	// Absolute path rejected — falls back to default to prevent
	// breakouts.
	assert.Equal(t, def, autonomyContextFilePath(&registry.Project{
		Autonomy: registry.ProjectAutonomy{ContextFilePath: "/etc/passwd"},
	}))

	// `..` traversal rejected.
	assert.Equal(t, def, autonomyContextFilePath(&registry.Project{
		Autonomy: registry.ProjectAutonomy{ContextFilePath: ".."},
	}))
	assert.Equal(t, def, autonomyContextFilePath(&registry.Project{
		Autonomy: registry.ProjectAutonomy{ContextFilePath: "../etc/passwd"},
	}))

	// Valid relative path passes through (cleaned).
	assert.Equal(t, "subdir/context.md",
		autonomyContextFilePath(&registry.Project{
			Autonomy: registry.ProjectAutonomy{ContextFilePath: "subdir//context.md"},
		}))
}

// --------------------------------------------------------------------
// tickCron — direct invocation
// --------------------------------------------------------------------

func TestTickCron_EmptyGoalRecordsParseError(t *testing.T) {
	repo := &stubEvalRepo{}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithEvaluationRepository(repo))

	project := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{}, // Goal empty
	}
	err := m.tickCron(context.Background(), project, time.Now())
	require.NoError(t, err, "empty goal is recoverable (skip cleanly), not a returned error")
	require.Len(t, repo.recorded, 1)
	assert.Equal(t, persistence.AutonomyOutcomeParseError, repo.recorded[0].Outcome)
	assert.Contains(t, repo.recorded[0].Reason, "autonomy.goal empty")
}

func TestTickCron_DispatchesTaskWithGoalAsPrompt(t *testing.T) {
	// cron-mode tick with a valid goal: must marshal the prompt args
	// and hand off to createAutonomousTask. We use a real registry so
	// the workflow + swarm lookup inside createAutonomousTask works.
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "cron"
  goal: "run the tick"
  pollInterval: "1h"
`)
	repo := &mockTaskRepo{}
	evalRepo := &stubEvalRepo{}
	m := New(nil, reg, repo, nil, WithEvaluationRepository(evalRepo))

	project := reg.GetProject("p1")
	require.NotNil(t, project)
	err := m.tickCron(context.Background(), project, time.Now())
	require.NoError(t, err)
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1, "valid goal must dispatch exactly one task")
	// Prompt rides on the task's Payload as part of the
	// createAutonomousTask args JSON.
	assert.Contains(t, string(tasks[0].Payload), "run the tick",
		"task payload must carry the goal verbatim")
}

// --------------------------------------------------------------------
// buildStateContext — assembles the LLM context blob; doesn't touch
// the LLM itself, just the workspace + tasks repo.
// --------------------------------------------------------------------

func TestBuildStateContext_NoTasksAndNoContextFile(t *testing.T) {
	// Workspace path unset → skip the context-file probe entirely;
	// task repo returns nothing → "no tasks yet" branch.
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	got, hasActive, err := m.buildStateContext(context.Background(), &registry.Project{ID: "p1"})
	require.NoError(t, err)
	assert.False(t, hasActive)
	assert.Contains(t, got, "No tasks have been created yet")
}

func TestBuildStateContext_ContextFilePresent(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1", ".autonomy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "p1", ".autonomy", "PROJECT_CONTEXT.md"),
		[]byte("strategy: trade momentum\n"), 0o644))

	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithWorkspacePath(ws))
	got, _, err := m.buildStateContext(context.Background(), &registry.Project{ID: "p1"})
	require.NoError(t, err)
	assert.Contains(t, got, "strategy: trade momentum")
	assert.Contains(t, got, ".autonomy/PROJECT_CONTEXT.md")
}

func TestBuildStateContext_ContextFileMissingReportsAbsence(t *testing.T) {
	ws := t.TempDir()
	// Workspace set but no file at the expected path → "NOT FOUND" line.
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithWorkspacePath(ws))
	got, _, err := m.buildStateContext(context.Background(), &registry.Project{ID: "p1"})
	require.NoError(t, err)
	assert.Contains(t, got, "NOT FOUND")
}

func TestBuildStateContext_TruncatesLargeContextFile(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1", ".autonomy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "p1", ".autonomy", "PROJECT_CONTEXT.md"),
		[]byte(strings.Repeat("x", 7000)), 0o644))

	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithWorkspacePath(ws))
	got, _, err := m.buildStateContext(context.Background(), &registry.Project{ID: "p1"})
	require.NoError(t, err)
	assert.Contains(t, got, "[...truncated]", "files past the 6000-byte cap must be truncated")
}

func TestBuildStateContext_FlagsActiveTask(t *testing.T) {
	now := time.Now()
	// Only an active task — completed tasks would trigger an
	// execRepo.GetByTaskIDs call which we haven't wired here.
	tasks := []*persistence.Task{
		{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: now,
			Payload: []byte(`{"context":{"prompt":"in-flight tick"}}`)},
	}
	repo := &mockTaskRepo{tasks: tasks}
	m := New(nil, &registry.Registry{}, repo, nil)
	got, hasActive, err := m.buildStateContext(context.Background(), &registry.Project{ID: "p1"})
	require.NoError(t, err)
	assert.True(t, hasActive, "RUNNING task must flip hasActive")
	assert.Contains(t, got, "Active/queued tasks")
	assert.Contains(t, got, "in-flight tick")
}

// --------------------------------------------------------------------
// autonomyPromptTitle — title-extraction helper covered by similarity
// + UI surfaces, but the direct unit test pins both the colon-split
// and length-truncate branches.
// --------------------------------------------------------------------

func TestAutonomyPromptTitle(t *testing.T) {
	// Colon within bound → take the prefix before the colon.
	assert.Equal(t, "Trade tick", autonomyPromptTitle("Trade tick: scan watchlist and propose entries"))

	// No colon, short → whole string.
	assert.Equal(t, "Short prompt", autonomyPromptTitle("Short prompt"))

	// No colon, long → truncated to 120.
	long := strings.Repeat("a", 200)
	got := autonomyPromptTitle(long)
	assert.Len(t, got, 120, "no-colon long prompts truncate at 120 chars")

	// Colon past the 120-char window → length truncation wins.
	mixed := strings.Repeat("a", 130) + ":suffix"
	got = autonomyPromptTitle(mixed)
	assert.Len(t, got, 120)

	// Leading whitespace trimmed before slicing.
	assert.Equal(t, "title", autonomyPromptTitle("   title:rest"))
}

// --------------------------------------------------------------------
// tickBacklog — direct invocation across the file-state matrix
// --------------------------------------------------------------------

func TestTickBacklog_NoWorkspacePath(t *testing.T) {
	repo := &stubEvalRepo{}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithEvaluationRepository(repo))

	project := &registry.Project{ID: "p1"}
	err := m.tickBacklog(context.Background(), project, time.Now())
	require.NoError(t, err)
	require.Len(t, repo.recorded, 1)
	assert.Equal(t, persistence.AutonomyOutcomeDBError, repo.recorded[0].Outcome)
	assert.Contains(t, repo.recorded[0].Reason, "workspace path not configured")
}

func TestTickBacklog_InvalidBacklogPath(t *testing.T) {
	repo := &stubEvalRepo{}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithEvaluationRepository(repo),
		WithWorkspacePath(t.TempDir()),
	)

	// Absolute path → ResolveBacklogFilePath returns empty.
	project := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{BacklogFilePath: "/etc/passwd"},
	}
	err := m.tickBacklog(context.Background(), project, time.Now())
	require.NoError(t, err)
	require.Len(t, repo.recorded, 1)
	assert.Equal(t, persistence.AutonomyOutcomeParseError, repo.recorded[0].Outcome)
}

// TestTickBacklog_SymlinkTraversalRejected asserts the path
// safety check refuses a backlog path whose deepest existing
// component is a symlink pointing outside the workspace.
//
// Original bug: tickBacklog used a bare filepath.Join, which
// cleans `..` but doesn't follow symlinks at write time. An
// in-container agent (or a compromised workspace dir) that
// planted `<workspace>/<projectID>/escape -> /tmp/outside`
// could then point backlogFilePath at "escape/file.md" and
// either read /tmp/outside/file.md or have the daemon write
// the consumed-marker outside the workspace.
//
// Fix: safepath.JoinUnder resolves symlinks in the deepest
// existing prefix and rejects anything that escapes the root.
func TestTickBacklog_SymlinkTraversalRejected(t *testing.T) {
	// Skip on Windows where the test relies on POSIX symlinks.
	if testing.Short() {
		t.Skip("symlink test skipped in -short mode")
	}
	ws := t.TempDir()
	outside := t.TempDir() // a dir OUTSIDE the workspace root
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	// Plant a symlink at <ws>/p1/escape pointing at /tmp/outside.
	// If safepath misses this, ReadFile/WriteFile against
	// <ws>/p1/escape/anything resolves to /tmp/outside/anything.
	require.NoError(t, os.Symlink(outside, filepath.Join(ws, "p1", "escape")))

	repo := &stubEvalRepo{}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithEvaluationRepository(repo),
		WithWorkspacePath(ws),
	)

	// Point backlogFilePath at a leaf under the escape symlink.
	project := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{BacklogFilePath: "escape/BACKLOG.md"},
	}
	err := m.tickBacklog(context.Background(), project, time.Now())
	require.NoError(t, err, "tick should swallow the rejection, not propagate")
	require.Len(t, repo.recorded, 1)
	assert.Equal(t, persistence.AutonomyOutcomeParseError, repo.recorded[0].Outcome,
		"escape attempt must be recorded as ParseError, not silently allowed")

	// The outside directory must remain empty — no consumed-
	// marker file landed there.
	entries, _ := os.ReadDir(outside)
	assert.Empty(t, entries, "no file should have been written outside the workspace")
}

func TestTickBacklog_MissingFileNoAction(t *testing.T) {
	repo := &stubEvalRepo{}
	ws := t.TempDir()
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithEvaluationRepository(repo),
		WithWorkspacePath(ws),
	)

	project := &registry.Project{ID: "p-no-file"}
	err := m.tickBacklog(context.Background(), project, time.Now())
	require.NoError(t, err)
	require.Len(t, repo.recorded, 1)
	assert.Equal(t, persistence.AutonomyOutcomeNoAction, repo.recorded[0].Outcome)
	assert.Contains(t, repo.recorded[0].Reason, "BACKLOG.md absent")
}

func TestTickBacklog_EmptyBacklogFile(t *testing.T) {
	repo := &stubEvalRepo{}
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "p1", "BACKLOG.md"),
		[]byte("# Backlog\n\nNothing pending here.\n- [x] already done\n"), 0o644))

	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithEvaluationRepository(repo),
		WithWorkspacePath(ws),
	)
	project := &registry.Project{ID: "p1"}
	err := m.tickBacklog(context.Background(), project, time.Now())
	require.NoError(t, err)
	require.Len(t, repo.recorded, 1)
	assert.Equal(t, persistence.AutonomyOutcomeNoAction, repo.recorded[0].Outcome)
	assert.Contains(t, repo.recorded[0].Reason, "no pending items")
}

func TestTickBacklog_ConsumesPendingItemAndDispatches(t *testing.T) {
	// Happy path: pending item exists, task fires, file is rewritten.
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	backlogPath := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlogPath,
		[]byte("# Backlog\n\n- [ ] first pending item\n- [ ] second pending item\n"), 0o644))

	repo := &mockTaskRepo{}
	m := New(nil, reg, repo, nil, WithWorkspacePath(ws))

	project := reg.GetProject("p1")
	require.NotNil(t, project)
	err := m.tickBacklog(context.Background(), project, time.Now())
	require.NoError(t, err)

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	assert.Contains(t, string(tasks[0].Payload), "first pending item")

	// File must be rewritten with the consumed item marked done.
	updated, err := os.ReadFile(backlogPath)
	require.NoError(t, err)
	assert.Contains(t, string(updated), "- [x] first pending item")
	assert.Contains(t, string(updated), "- [ ] second pending item",
		"only the consumed item changes; subsequent items remain pending")
}

// --------------------------------------------------------------------
// consumeNextBacklogItem — the pure parser the tick wraps.
// --------------------------------------------------------------------

func TestConsumeNextBacklogItem_PicksFirstPending(t *testing.T) {
	content := `# Backlog

- [x] already done
- [ ] do the first thing
- [ ] do the second thing
`
	prompt, newContent, ok := consumeNextBacklogItem(content)
	require.True(t, ok)
	assert.Equal(t, "do the first thing", prompt)
	assert.Contains(t, newContent, "- [x] do the first thing", "consumed item must be marked done")
	assert.Contains(t, newContent, "- [ ] do the second thing", "subsequent items must remain pending")
}

func TestConsumeNextBacklogItem_NoPendingItems(t *testing.T) {
	content := `# Backlog
- [x] done one
- [x] done two
`
	prompt, newContent, ok := consumeNextBacklogItem(content)
	assert.False(t, ok)
	assert.Empty(t, prompt)
	assert.Empty(t, newContent, "ok=false must return empty newContent so callers don't overwrite the file")
}

func TestConsumeNextBacklogItem_BulletVariants(t *testing.T) {
	// Both `-` and `*` bullets count as pending items.
	content := "* [ ] star bullet\n- [ ] dash bullet\n"
	prompt, _, ok := consumeNextBacklogItem(content)
	require.True(t, ok)
	assert.Equal(t, "star bullet", prompt, "first match wins")
}

func TestRecordEvaluation_PersistsRowShape(t *testing.T) {
	repo := &stubEvalRepo{}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithEvaluationRepository(repo))

	taskID := "task-123"
	start := time.Now().Add(-50 * time.Millisecond)
	m.recordEvaluation(context.Background(), evalRecord{
		projectID:  "p1",
		outcome:    "executed",
		reason:     "fired tick",
		taskID:     taskID,
		taskType:   "research",
		workflowID: "wf-1",
		prompt:     "do the thing",
		start:      start,
	})
	require.Len(t, repo.recorded, 1)
	got := repo.recorded[0]
	assert.Equal(t, "p1", got.ProjectID)
	assert.Equal(t, "executed", got.Outcome)
	assert.Equal(t, "fired tick", got.Reason)
	require.NotNil(t, got.TaskID)
	assert.Equal(t, taskID, *got.TaskID)
	assert.NotEmpty(t, got.PromptHash, "non-empty prompt must produce a non-empty hash")
	assert.NotZero(t, got.DurationMs, "non-zero start time must produce a non-zero duration")
}
