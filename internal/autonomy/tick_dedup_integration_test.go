package autonomy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Tier-2 autonomy-tick integration coverage (https://docs.vornik.io).
//
// These tests exercise the FULL evaluate() tick entry point — the
// same path projectLoop drives on every poll — for the two
// deterministic, LLM-free modes (cron and backlog), and assert two
// invariants end-to-end:
//
//  1. one tick of an autonomy-enabled project creates exactly one
//     task for its mode, and
//  2. a rapid second tick does NOT double-create — the dedup guards
//     (completion-window dedup for cron; the active-task gate for
//     backlog) suppress it.
//
// The existing per-mode tests cover narrower slices:
//   - TestHV_Evaluate_CronModeBypassesLLM: a SINGLE cron tick (one
//     create), duplicateWindow=0s (dedup explicitly off).
//   - TestHV_TickBacklog_FiresItemAndMarksConsumed: one create via a
//     DIRECT tickBacklog call, bypassing evaluate's active-task gate.
//   - TestCreateAutonomousTask_SuppressesActiveDuplicate: dedup at the
//     createAutonomousTask level, not through a tick.
//
// None of them drive evaluate twice and assert the second tick is
// suppressed, which is the tick→create + dedup-prevents-duplicate
// angle pinned here. All deterministic: no LLM (nil client — the
// cron/backlog branches never reach it), no sleeps, time injected via
// the seeded task's UpdatedAt.

// stubExecRepo is a minimal ExecutionRepository for the tick tests.
// buildStateContext's completed-task branch calls GetByTaskIDs to
// enrich history; we return an empty map (the real repo's behaviour
// for tasks that have no execution rows). The interface is embedded
// so the unused methods exist without hand-stubbing all of them; only
// GetByTaskIDs is ever invoked on this path, and any accidental call
// to another method would nil-panic loudly rather than pass silently.
type stubExecRepo struct {
	persistence.ExecutionRepository
}

func (s *stubExecRepo) GetByTaskIDs(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
	return map[string]*persistence.Execution{}, nil
}

// TestTick_CronMode_SecondTickSuppressedByDuplicateWindow drives a
// cron-style project through evaluate() twice. With a NON-zero
// duplicateWindow the completion-dedup is active: tick one creates the
// task, and once that task has COMPLETED (recently), a rapid second
// tick fires the identical goal prompt and must be suppressed by
// findAutonomyDuplicate's completion-window branch — not create a
// second task.
//
// Why mark the first task COMPLETED before the second tick: a still-
// QUEUED task would trip evaluate's earlier hasActive gate (covered by
// the backlog case below), short-circuiting before the completion-
// window dedup we want to pin here.
func TestTick_CronMode_SecondTickSuppressedByDuplicateWindow(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	// execRepo is wired (as it always is in production) so the second
	// tick's buildStateContext can summarise the now-COMPLETED first
	// task without a nil-deref.
	m := New(nil, &registry.Registry{}, repo, &stubExecRepo{}, WithEvaluationRepository(evalRepo))

	p := &registry.Project{
		ID:      "cron-proj",
		SwarmID: "", // no swarm => workflow-role validation skipped
		Autonomy: registry.ProjectAutonomy{
			Enabled:          true,
			Mode:             registry.AutonomyModeCron,
			Goal:             "Run one trading tick",
			DuplicateWindow:  "1h", // non-zero => completion dedup active
			CronTaskType:     "trading",
			AllowedTaskTypes: []string{"trading"},
			MaxTasksPerHour:  100,
		},
	}

	// Tick 1: exactly one task created via the cron branch.
	require.NoError(t, m.evaluate(context.Background(), p),
		"first cron tick must bypass the nil LLM and create a task")
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1, "first cron tick creates exactly one task")
	assert.Equal(t, persistence.TaskStatusQueued, tasks[0].Status)

	// Move the first task to COMPLETED, just now — so the second tick
	// clears evaluate's hasActive gate and lands on the completion-
	// window dedup, with updatedAt comfortably inside the 1h window.
	repo.mu.Lock()
	repo.tasks[0].Status = persistence.TaskStatusCompleted
	repo.tasks[0].UpdatedAt = time.Now()
	repo.mu.Unlock()

	// Tick 2 (rapid): identical goal prompt, within the dedup window.
	require.NoError(t, m.evaluate(context.Background(), p),
		"second cron tick must skip cleanly, not error")
	assert.Len(t, repo.createdTasks(), 1,
		"second cron tick within the duplicate window must be suppressed — no second task")

	// The suppression must be recorded as a DUPLICATE outcome (the
	// completion-window branch), proving it was the dedup guard and not
	// some other early gate that stopped the create.
	var sawDuplicate bool
	for _, e := range evalRepo.snapshot() {
		if e.Outcome == persistence.AutonomyOutcomeDuplicate {
			sawDuplicate = true
		}
	}
	assert.True(t, sawDuplicate,
		"second tick must record a DUPLICATE evaluation outcome")
}

// TestTick_BacklogMode_SecondRapidTickSuppressedByActiveGate drives a
// backlog-style project through evaluate() twice. Tick one consumes
// the top BACKLOG.md item and creates exactly one QUEUED task. A rapid
// second tick — before the first task leaves the queue — must be
// suppressed by evaluate's active-task gate (a backlog project should
// never pile a second item on top of one still in flight), so no
// second task is created.
//
// This pins the tick→create + dedup-prevents-duplicate angle for
// backlog mode through the real evaluate() entry point, which the
// existing direct tickBacklog tests skip (they bypass the hasActive
// gate entirely).
func TestTick_BacklogMode_SecondRapidTickSuppressedByActiveGate(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "backlog-proj"), 0o755))
	backlog := filepath.Join(ws, "backlog-proj", "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog,
		[]byte("- [ ] Ship the first thing\n- [ ] Ship the second thing\n"), 0o644))

	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil,
		WithWorkspacePath(ws), WithEvaluationRepository(evalRepo))

	p := &registry.Project{
		ID: "backlog-proj",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Mode:            registry.AutonomyModeBacklog,
			MaxTasksPerHour: 100,
		},
	}

	// Tick 1: consume the first pending item, create exactly one task,
	// and mark that line consumed in the file.
	require.NoError(t, m.evaluate(context.Background(), p),
		"first backlog tick must create a task without touching the nil LLM")
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1, "first backlog tick creates exactly one task")
	assert.Equal(t, "Ship the first thing", extractPrompt(tasks[0].Payload))
	assert.Equal(t, persistence.TaskStatusQueued, tasks[0].Status)

	// The created task is still QUEUED (active). A rapid second tick
	// must be gated by evaluate's hasActive check before it ever reads
	// the next backlog item.
	require.NoError(t, m.evaluate(context.Background(), p),
		"second backlog tick must skip cleanly while a task is in flight")
	assert.Len(t, repo.createdTasks(), 1,
		"second rapid backlog tick must not create a task while one is still queued")

	// Confirm the suppression came from the active-task gate, and that
	// the second pending item was NOT consumed (the file still shows it
	// pending) — the operator's queued work is untouched.
	var sawActive bool
	for _, e := range evalRepo.snapshot() {
		if e.Outcome == persistence.AutonomyOutcomeActiveTasks {
			sawActive = true
		}
	}
	assert.True(t, sawActive,
		"second tick must record an ACTIVE_TASKS evaluation outcome")

	got, err := os.ReadFile(backlog)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [x] Ship the first thing",
		"first item consumed by tick one")
	assert.Contains(t, string(got), "- [ ] Ship the second thing",
		"second item must stay pending — the gated tick must not consume it")
}
