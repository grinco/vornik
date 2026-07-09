package autonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/backlogfile"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Task 6 coverage: backlog-mode workflow_id routing, the
// injection-hardening framing prompt, consumed-item task annotation,
// and the FAILED-task reconcile pass. These drive tickBacklog (and the
// reconcile helper) through the real backlogfile.Store, asserting the
// dispatched args, the on-disk file lines, and the audit outcomes.

// seedBacklogTick wires a manager for the backlog-mode project "p1"
// whose BACKLOG.md holds content, returning the manager, the task repo,
// the captured eval repo, and the absolute backlog path.
func seedBacklogTick(t *testing.T, reg *registry.Registry, content string, opts ...Option) (*Manager, *mockTaskRepo, *captureEvalRepo, string) {
	t.Helper()
	const projectID = "p1"
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, projectID), 0o755))
	abs := filepath.Join(ws, projectID, "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))

	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	base := []Option{
		WithWorkspacePath(ws),
		WithEvaluationRepository(evalRepo),
		WithBacklogStore(backlogfile.NewStore()),
	}
	m := New(nil, reg, repo, nil, append(base, opts...)...)
	return m, repo, evalRepo, abs
}

// (a) workflow_id set + valid: the dispatched task carries it, and the
// consumed line is annotated with the created task ID (d) with the raw
// item text between the framing ITEM markers (f).
func TestTickBacklog_WorkflowIDValid_CarriedAndFramed(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  workflow_id: "w"
  pollInterval: "1h"
`)
	m, repo, _, abs := seedBacklogTick(t, reg, "- [ ] fix the parser bug\n")
	project := reg.GetProject("p1")
	require.NotNil(t, project)

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)

	// (a) workflow_id from autonomy config rides on the task.
	require.NotNil(t, tasks[0].WorkflowID, "valid workflow_id must be set on the task")
	assert.Equal(t, "w", *tasks[0].WorkflowID)

	// (f) the dispatched prompt is the framing template wrapping the raw item.
	prompt := extractPrompt(tasks[0].Payload)
	assert.Contains(t, prompt, "Work ONLY on the following backlog item")
	assert.Contains(t, prompt, "<<<ITEM\nfix the parser bug\nITEM",
		"raw item must appear verbatim between the ITEM markers")

	// (d) the file line gains the (task: <id>) annotation with the RAW
	// item text — NOT the framed prompt.
	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [x] fix the parser bug (task: "+tasks[0].ID+")")
	assert.NotContains(t, string(got), "Work ONLY on the following",
		"the framing wrapper must never leak into the file")
}

// (b) workflow_id set + invalid: no task is created, a ParseError
// evaluation is recorded, and an ERROR is logged.
func TestTickBacklog_WorkflowIDInvalid_SkipsTickAndLogsError(t *testing.T) {
	// registry.Load only WARNS on a bad workflow_id (the project still
	// loads — keeps-old-config hot-reload semantics), so this tick-time
	// check is the enforcement point. This in-memory project models it
	// directly: a workflow_id that doesn't resolve at tick time (config
	// typo, workflow removed after load, or a programmatic project).
	// An empty registry has no "ghost-workflow", so GetWorkflow → nil.
	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)
	m, repo, evalRepo, abs := seedBacklogTick(t, &registry.Registry{},
		"- [ ] do the thing\n", WithLogger(logger))
	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled:    true,
			Mode:       registry.AutonomyModeBacklog,
			WorkflowID: "ghost-workflow",
		},
	}

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	assert.Empty(t, repo.createdTasks(), "an unknown workflow_id must block dispatch")

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeParseError, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "workflow_id not found: ghost-workflow")

	assert.Contains(t, logBuf.String(), `"level":"error"`, "must log at ERROR level")
	assert.Contains(t, logBuf.String(), "ghost-workflow")

	// The item stays pending — the operator's work is not lost.
	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [ ] do the thing")
}

// (c) workflow_id empty: the dispatched args carry an empty workflow_id
// (task.WorkflowID stays nil), preserving the DefaultWorkflowID
// fallthrough that createAutonomousTask + the dispatcher apply.
func TestTickBacklog_WorkflowIDEmpty_FallsThrough(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	m, repo, _, _ := seedBacklogTick(t, reg, "- [ ] groom the backlog\n")
	project := reg.GetProject("p1")
	require.NotNil(t, project)
	require.Empty(t, project.Autonomy.WorkflowID, "test premise: no autonomy.workflow_id")

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1, "empty workflow_id must NOT block dispatch")
	assert.Nil(t, tasks[0].WorkflowID,
		"empty workflow_id leaves the task's field nil (DefaultWorkflowID resolved downstream)")
}

// (e) reconcile flips a FAILED task's consumed [x] line to [!] once,
// is idempotent, leaves a COMPLETED task's line at [x], and skips an
// item the operator manually reset to [ ] even if that task FAILED.
func TestReconcileFailedBacklogItems(t *testing.T) {
	content := "" +
		"- [x] failed one (task: task_fail)\n" +
		"- [ ] reset one (task: task_reset)\n" +
		"- [x] done one (task: task_done)\n" +
		"- [x] no-annotation item\n"

	repo := &mockTaskRepo{tasks: []*persistence.Task{
		{ID: "task_fail", Status: persistence.TaskStatusFailed},
		{ID: "task_reset", Status: persistence.TaskStatusFailed},
		{ID: "task_done", Status: persistence.TaskStatusCompleted},
	}}

	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	abs := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))

	m := New(nil, &registry.Registry{}, repo, nil,
		WithWorkspacePath(ws), WithBacklogStore(backlogfile.NewStore()))
	project := &registry.Project{ID: "p1"}

	m.reconcileFailedBacklogItems(context.Background(), abs, project)

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	want := "" +
		"- [!] failed one (task: task_fail, failed)\n" +
		"- [ ] reset one (task: task_reset)\n" + // operator reset: marker ' ' → skipped
		"- [x] done one (task: task_done)\n" + // COMPLETED → untouched
		"- [x] no-annotation item\n" // no (task:) suffix → untouched
	assert.Equal(t, want, string(got))

	// Idempotent: a second pass leaves the file byte-for-byte identical.
	m.reconcileFailedBacklogItems(context.Background(), abs, project)
	got2, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, want, string(got2), "reconcile must be idempotent")
}

// (e cont.) reconcile is best-effort: a task whose lookup fails
// (pruned by retention) is skipped, not flipped.
func TestReconcileFailedBacklogItems_SkipsUnknownTask(t *testing.T) {
	content := "- [x] orphaned (task: task_gone)\n"
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	abs := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))

	// Empty repo → every Get returns ErrNotFound.
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithWorkspacePath(ws), WithBacklogStore(backlogfile.NewStore()))
	m.reconcileFailedBacklogItems(context.Background(), abs, &registry.Project{ID: "p1"})

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, content, string(got), "a task we can't confirm FAILED must be left alone")
}

// tickBacklog runs the reconcile pass before dispatching: a FAILED
// consumed item is flipped to [!] and the next pending item still
// fires, in one tick.
func TestTickBacklog_ReconcilesThenDispatches(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	content := "" +
		"- [x] earlier work (task: task_fail)\n" +
		"- [ ] next work\n"
	m, repo, _, abs := seedBacklogTick(t, reg, content)
	// Seed the earlier task as FAILED so reconcile flips its line.
	repo.mu.Lock()
	repo.tasks = append(repo.tasks, &persistence.Task{ID: "task_fail", Status: persistence.TaskStatusFailed})
	repo.mu.Unlock()

	project := reg.GetProject("p1")
	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [!] earlier work (task: task_fail, failed)",
		"reconcile pass must flip the FAILED item")
	// The next pending item still dispatched + consumed in the same tick.
	created := repo.createdTasks()
	// createdTasks includes the seeded task_fail plus any newly created.
	var newTask *persistence.Task
	for _, task := range created {
		if task.ID != "task_fail" {
			newTask = task
		}
	}
	require.NotNil(t, newTask, "the next pending item must still be dispatched")
	assert.Contains(t, string(got), "- [x] next work (task: "+newTask.ID+")")
}

// (g) proposed `- [?]` items are never consumed by a backlog tick —
// only `- [ ]` is consumable. A file with only proposed items records
// NO_ACTION and creates no task.
func TestTickBacklog_ProposedItemsNeverConsumed(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	content := "" +
		"- [?] proposed by an agent, awaiting operator approval\n" +
		"- [x] already done\n"
	m, repo, evalRepo, abs := seedBacklogTick(t, reg, content)
	project := reg.GetProject("p1")

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	assert.Empty(t, repo.createdTasks(), "proposed [?] items must never be dispatched")
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeNoAction, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "no pending items")

	// The proposed item is left untouched.
	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [?] proposed by an agent, awaiting operator approval")
}

// tickBacklog with no backlog store wired records DB_ERROR and skips —
// backlog mode's file mutations are a hard dependency on the shared
// store.
func TestTickBacklog_NilStore_RecordsDBError(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "p1", "BACKLOG.md"),
		[]byte("- [ ] work\n"), 0o644))

	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	// No WithBacklogStore → m.backlog is nil.
	m := New(nil, &registry.Registry{}, repo, nil,
		WithWorkspacePath(ws), WithEvaluationRepository(evalRepo))
	err := m.tickBacklog(context.Background(), &registry.Project{ID: "p1"}, time.Now())
	require.NoError(t, err)
	assert.Empty(t, repo.createdTasks())
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeDBError, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "backlog store not configured")
}

// A real read error (here: the backlog path is a directory, so
// ReadFile fails with something other than "not exist") surfaces as a
// DB_ERROR outcome and a returned error — both the reconcile Items()
// read and the PeekNext read hit it.
func TestTickBacklog_ReadError_SurfacesDBError(t *testing.T) {
	ws := t.TempDir()
	// Make <ws>/p1/BACKLOG.md a DIRECTORY so os.ReadFile returns EISDIR.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1", "BACKLOG.md"), 0o755))

	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil,
		WithWorkspacePath(ws), WithEvaluationRepository(evalRepo),
		WithBacklogStore(backlogfile.NewStore()))

	err := m.tickBacklog(context.Background(), &registry.Project{ID: "p1"}, time.Now())
	require.Error(t, err, "a non-NotExist read error must propagate to the loop")
	assert.Empty(t, repo.createdTasks())
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeDBError, entries[0].Outcome)
}

// When createAutonomousTask returns an error (here: the task repo's
// Create fails), the item is NOT consumed — it stays pending so the
// next tick retries it.
func TestTickBacklog_CreateError_LeavesItemPending(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	abs := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte("- [ ] do the work\n"), 0o644))

	repo := &mockTaskRepo{createF: func(context.Context, *persistence.Task) error {
		return errors.New("simulated create failure")
	}}
	m := New(nil, reg, repo, nil,
		WithWorkspacePath(ws), WithBacklogStore(backlogfile.NewStore()))

	err := m.tickBacklog(context.Background(), reg.GetProject("p1"), time.Now())
	require.Error(t, err)

	got, rerr := os.ReadFile(abs)
	require.NoError(t, rerr)
	assert.Contains(t, string(got), "- [ ] do the work",
		"a failed create must leave the item pending")
	assert.NotContains(t, string(got), "[x]")
}

// reconcileFailedBacklogItems short-circuits when its dependencies
// (the shared store or the task repo) are absent.
func TestReconcileFailedBacklogItems_NilDeps(t *testing.T) {
	// nil backlog store.
	m1 := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithWorkspacePath(t.TempDir()))
	m1.reconcileFailedBacklogItems(context.Background(), "/nonexistent", &registry.Project{ID: "p1"})

	// nil task repo (store present).
	m2 := New(nil, &registry.Registry{}, nil, nil,
		WithWorkspacePath(t.TempDir()), WithBacklogStore(backlogfile.NewStore()))
	m2.reconcileFailedBacklogItems(context.Background(), "/nonexistent", &registry.Project{ID: "p1"})
	// No panic, no-op — reaching here is the assertion.
}

// A suppressed create (e.g. exact-duplicate within the dedup window)
// returns no task ID and leaves the item pending for a later tick,
// instead of stamping a bogus "(task: )" annotation.
func TestTickBacklog_SuppressedCreate_LeavesItemPending(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  mode: "backlog"
  pollInterval: "1h"
`)
	// Pre-seed an active (QUEUED) task whose framed prompt matches what
	// this item will dispatch, so createAutonomousTask suppresses it as
	// a duplicate (returns "", nil).
	framed := strings.Replace(backlogFramingPrompt, "%s", "repeat me", 1)
	// taskType must equal the tick's resolved cron task type ("task"
	// default) and the prompt must equal the framed dispatch so
	// findAutonomyDuplicate's active-task branch fires.
	payload, _ := json.Marshal(map[string]any{
		"taskType": "task",
		"context":  map[string]any{"prompt": framed},
	})
	repo := &mockTaskRepo{tasks: []*persistence.Task{{
		ID:             "existing",
		ProjectID:      "p1",
		CreationSource: persistence.TaskCreationSourceAutonomous,
		Status:         persistence.TaskStatusQueued,
		Payload:        payload,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}}}

	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	abs := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte("- [ ] repeat me\n"), 0o644))

	m := New(nil, reg, repo, nil,
		WithWorkspacePath(ws), WithBacklogStore(backlogfile.NewStore()))
	project := reg.GetProject("p1")

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [ ] repeat me",
		"a suppressed create must leave the item pending, not annotate it")
	assert.NotContains(t, string(got), "(task: )", "no bogus empty annotation")
}
