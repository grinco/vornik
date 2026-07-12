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

// (e) reconcile flips a consumed [x] line to blocked [!] when its task
// ended unsuccessfully — FAILED, CANCELLED, or CLOSED WITH a failure
// error — so the item is neither silently skipped as done nor auto-
// retried into a storm (operator flips [!]→[ ] to retry). A COMPLETED
// task's line stays [x] (it succeeded / raised its PR); a CLOSED task
// with NO failure error (operator closed a success) stays [x]; a non-[x]
// line and a line without a (task:) annotation are untouched.
//
// Regression for the 2026-07-11 headmatch incident: TASK-113 (CLOSED on
// a rework-loop give-up) and TASK-112 (FAILED) were left marked `[x]`
// done, silently skipped forever. The `task_closed_err` case pins the
// CLOSED-with-error → blocked behavior that fixes it.
func TestReconcileBacklogItems(t *testing.T) {
	boom := "step implement visited 4 times (max 3) — likely infinite rework loop"
	content := "" +
		"- [x] failed one (task: task_fail)\n" +
		"- [x] cancelled one (task: task_cancel)\n" +
		"- [x] gaveup one (task: task_closed_err)\n" +
		"- [x] clean-closed one (task: task_closed_ok)\n" +
		"- [ ] reset one (task: task_reset)\n" +
		"- [x] done one (task: task_done)\n" +
		"- [x] no-annotation item\n"

	repo := &mockTaskRepo{tasks: []*persistence.Task{
		{ID: "task_fail", Status: persistence.TaskStatusFailed},
		{ID: "task_cancel", Status: persistence.TaskStatusCancelled},
		// CLOSED + failure error (e.g. rework-loop give-up) → blocked.
		{ID: "task_closed_err", Status: persistence.TaskStatusClosed, LastError: &boom},
		// CLOSED with no error (operator closed a success) → stays done.
		{ID: "task_closed_ok", Status: persistence.TaskStatusClosed},
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

	m.reconcileBacklogItems(context.Background(), abs, project)

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	want := "" +
		"- [!] failed one (task: task_fail, failed)\n" + // FAILED → blocked
		"- [!] cancelled one (task: task_cancel, failed)\n" + // CANCELLED → blocked
		"- [!] gaveup one (task: task_closed_err, failed)\n" + // CLOSED+err → blocked
		"- [x] clean-closed one (task: task_closed_ok)\n" + // CLOSED, no err → untouched
		"- [ ] reset one (task: task_reset)\n" + // already [ ] → skipped by the marker gate
		"- [x] done one (task: task_done)\n" + // COMPLETED → untouched
		"- [x] no-annotation item\n" // no (task:) suffix → untouched
	assert.Equal(t, want, string(got))

	// Idempotent: a second pass leaves the file byte-for-byte identical
	// (the blocked items are now [!], no longer matched by the [x] gate).
	m.reconcileBacklogItems(context.Background(), abs, project)
	got2, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, want, string(got2), "reconcile must be idempotent")
}

// (e cont.) reconcile is best-effort: a task whose lookup fails
// (pruned by retention) is skipped, not touched.
func TestReconcileBacklogItems_SkipsUnknownTask(t *testing.T) {
	content := "- [x] orphaned (task: task_gone)\n"
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	abs := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))

	// Empty repo → every Get returns ErrNotFound.
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil,
		WithWorkspacePath(ws), WithBacklogStore(backlogfile.NewStore()))
	m.reconcileBacklogItems(context.Background(), abs, &registry.Project{ID: "p1"})

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, content, string(got), "a task we can't confirm FAILED must be left alone")
}

// tickBacklog runs the reconcile pass before dispatching: a FAILED
// consumed item is flipped to blocked [!] (NOT re-dispatched — the
// operator decides), so the tick dispatches the NEXT pending [ ] item
// instead, all in one pass.
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
	// Seed the earlier task as FAILED so reconcile blocks its line.
	repo.mu.Lock()
	repo.tasks = append(repo.tasks, &persistence.Task{ID: "task_fail", Status: persistence.TaskStatusFailed})
	repo.mu.Unlock()

	project := reg.GetProject("p1")
	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))

	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	// The failed item is blocked, not re-dispatched; "next work" is the
	// first pending [ ] item, so it's the one dispatched this tick.
	created := repo.createdTasks()
	var newTask *persistence.Task
	for _, task := range created {
		if task.ID != "task_fail" {
			newTask = task
		}
	}
	require.NotNil(t, newTask, "the next pending item must be dispatched")
	assert.Contains(t, string(got), "- [!] earlier work (task: task_fail, failed)",
		"reconcile blocks the FAILED item ([x]→[!]); it is NOT re-dispatched")
	assert.Contains(t, string(got), "- [x] next work (task: "+newTask.ID+")",
		"the next pending item is dispatched and consumed this tick")
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

// reconcileBacklogItems short-circuits when its dependencies
// (the shared store or the task repo) are absent.
func TestReconcileFailedBacklogItems_NilDeps(t *testing.T) {
	// nil backlog store.
	m1 := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, WithWorkspacePath(t.TempDir()))
	m1.reconcileBacklogItems(context.Background(), "/nonexistent", &registry.Project{ID: "p1"})

	// nil task repo (store present).
	m2 := New(nil, &registry.Registry{}, nil, nil,
		WithWorkspacePath(t.TempDir()), WithBacklogStore(backlogfile.NewStore()))
	m2.reconcileBacklogItems(context.Background(), "/nonexistent", &registry.Project{ID: "p1"})
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
