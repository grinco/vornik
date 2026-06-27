package autonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// These high-value tests target the autonomous task-creation gating
// logic that drives the live-trading + job-hunt bots. They exercise
// the pure decision helpers directly (cheap, deterministic) and the
// createAutonomousTask / evaluate / tick* paths via the existing
// mockTaskRepo + captureEvalRepo fixtures from manager_test.go. The
// prefix is HV_ so they can be iterated in isolation:
//   go test ./internal/autonomy/ -run HV_

// ---------------------------------------------------------------------------
// Require-approval gating: AWAITING_APPROVAL vs QUEUED on creation.
// ---------------------------------------------------------------------------

// TestHV_CreateAutonomousTask_NoApproval_DefaultsQueued is the
// complement to the RequireApproval test: with approval OFF the task
// must land QUEUED so the scheduler can lease it immediately.
func TestHV_CreateAutonomousTask_NoApproval_DefaultsQueued(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "Build things",
			RequireApproval: false,
		},
	}
	require.NoError(t, m.createAutonomousTask(context.Background(), project, `{"prompt":"do work"}`, time.Now()))
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, persistence.TaskStatusQueued, tasks[0].Status,
		"approval OFF must produce a directly-leasable QUEUED task")
}

// TestHV_CreateAutonomousTask_ApprovalEmitsAuditOutcome confirms the
// approval path stamps the persisted audit row with CREATED (the task
// was created, merely parked) and the reason names AWAITING_APPROVAL.
func TestHV_CreateAutonomousTask_ApprovalEmitsAuditOutcome(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	project := &registry.Project{
		ID: "approve-proj",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "Build things",
			RequireApproval: true,
		},
	}
	require.NoError(t, m.createAutonomousTask(context.Background(), project, `{"prompt":"Do stuff","type":"feature"}`, time.Now()))

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeCreated, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, string(persistence.TaskStatusAwaitingApproval),
		"audit reason should name the AWAITING_APPROVAL parked status")
}

// ---------------------------------------------------------------------------
// CreationSource + identity stamping on the created task.
// ---------------------------------------------------------------------------

// TestHV_CreateAutonomousTask_StampsSourceAndRetries pins the
// invariant that every autonomy-created task is stamped AUTONOMOUS
// (so it counts toward the rate cap + circuit breaker) with the
// standard retry envelope (attempt 1, maxAttempts 3).
func TestHV_CreateAutonomousTask_StampsSourceAndRetries(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1", DefaultPriority: 70}

	require.NoError(t, m.createAutonomousTask(context.Background(), project, `{"prompt":"work","type":"feature"}`, time.Now()))
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, persistence.TaskCreationSourceAutonomous, tasks[0].CreationSource)
	assert.Equal(t, 1, tasks[0].Attempt)
	assert.Equal(t, 3, tasks[0].MaxAttempts)
	assert.Equal(t, 70, tasks[0].Priority, "priority inherits project DefaultPriority")
}

// TestHV_CreateAutonomousTask_BacklogMode_HasIdempotencyKey — the
// default (non-cron) duplicateWindow leaves the key SET so the
// hour-bucket safety net is active. Mirror of the cron-mode test
// which asserts the key is nil.
func TestHV_CreateAutonomousTask_BacklogMode_HasIdempotencyKey(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"} // no DuplicateWindow => 24h default

	require.NoError(t, m.createAutonomousTask(context.Background(), project, `{"prompt":"work","type":"feature"}`, time.Now()))
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	require.NotNil(t, tasks[0].IdempotencyKey, "non-cron tasks must carry an idempotency key")
	assert.Contains(t, *tasks[0].IdempotencyKey, "auto:")
}

// TestHV_CreateAutonomousTask_IdempotencyHit suppresses a second
// identical create within the same hour bucket when an existing task
// already carries that key. Pins the hour-bucketed safety net for
// backlog-style projects.
func TestHV_CreateAutonomousTask_IdempotencyHit(t *testing.T) {
	now := time.Now().UTC()
	key := buildAutonomyIdempotencyKey("p1", "feature", "", "work", now)
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{{
			ID:             "pre",
			ProjectID:      "p1",
			CreationSource: persistence.TaskCreationSourceAutonomous,
			Status:         persistence.TaskStatusCompleted,
			// Distinct prompt so findAutonomyDuplicate doesn't fire;
			// we want the idempotency-key path specifically.
			Payload:        []byte(`{"taskType":"feature","context":{"prompt":"unrelated old work"}}`),
			IdempotencyKey: &key,
			CreatedAt:      now.Add(-90 * time.Minute),
			UpdatedAt:      now.Add(-90 * time.Minute),
		}},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	require.NoError(t, m.createAutonomousTask(context.Background(), project, `{"prompt":"work","type":"feature"}`, now))
	// No new Create call — the pre-existing seeded task is the only one.
	assert.Len(t, repo.createdTasks(), 1, "matching idempotency key must suppress the duplicate create")
}

// ---------------------------------------------------------------------------
// Dedupe: never double-schedule a slug already QUEUED/LEASED/RUNNING etc.
// ---------------------------------------------------------------------------

// TestHV_FindAutonomyDuplicate_ActiveStatuses table-drives every
// status that findAutonomyDuplicate treats as "active" — a matching
// task in any of these must block a new schedule regardless of the
// completion window.
func TestHV_FindAutonomyDuplicate_ActiveStatuses(t *testing.T) {
	active := []persistence.TaskStatus{
		persistence.TaskStatusQueued,
		persistence.TaskStatusPending,
		persistence.TaskStatusAwaitingApproval,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
	}
	for _, st := range active {
		t.Run(string(st), func(t *testing.T) {
			tasks := []*persistence.Task{{
				ID:      "x",
				Status:  st,
				Payload: []byte(`{"taskType":"feature","context":{"prompt":"Build X"}}`),
			}}
			// completionWindow 0 (cron) — active check is window-independent.
			reason, dup := findAutonomyDuplicate(tasks, "feature", "", "Build X", 0)
			assert.True(t, dup, "active status %s must dedupe", st)
			assert.Contains(t, reason, "active task")
		})
	}
}

// TestHV_FindAutonomyDuplicate_TypeOrWorkflowMismatch — a same-prompt
// task with a different type OR workflow is NOT a duplicate; the
// triple (prompt,type,workflow) is the dedupe key.
func TestHV_FindAutonomyDuplicate_TypeOrWorkflowMismatch(t *testing.T) {
	wf := "wf-a"
	tasks := []*persistence.Task{{
		ID:         "x",
		Status:     persistence.TaskStatusRunning,
		WorkflowID: &wf,
		Payload:    []byte(`{"taskType":"feature","context":{"prompt":"Build X"}}`),
	}}
	// Different type.
	_, dup := findAutonomyDuplicate(tasks, "bugfix", "wf-a", "Build X", 0)
	assert.False(t, dup, "different task type is not a duplicate")
	// Different workflow.
	_, dup = findAutonomyDuplicate(tasks, "feature", "wf-b", "Build X", 0)
	assert.False(t, dup, "different workflow is not a duplicate")
	// Exact triple match.
	_, dup = findAutonomyDuplicate(tasks, "feature", "wf-a", "Build X", 0)
	assert.True(t, dup, "exact (prompt,type,workflow) match is a duplicate")
}

// TestHV_FindAutonomyDuplicate_CompletedWindowBoundary pins the
// completion-window edge: a completed task just INSIDE the window
// dedupes; just OUTSIDE it does not. Uses UpdatedAt (the field the
// helper measures from).
func TestHV_FindAutonomyDuplicate_CompletedWindowBoundary(t *testing.T) {
	window := time.Hour
	mk := func(age time.Duration) []*persistence.Task {
		return []*persistence.Task{{
			ID:        "c",
			Status:    persistence.TaskStatusCompleted,
			UpdatedAt: time.Now().Add(-age),
			Payload:   []byte(`{"taskType":"feature","context":{"prompt":"Build X"}}`),
		}}
	}
	_, dup := findAutonomyDuplicate(mk(30*time.Minute), "feature", "", "Build X", window)
	assert.True(t, dup, "completed 30m ago is within a 1h window — dedupe")

	_, dup = findAutonomyDuplicate(mk(2*time.Hour), "feature", "", "Build X", window)
	assert.False(t, dup, "completed 2h ago is outside a 1h window — allow")
}

// TestHV_FindAutonomyDuplicate_CronWindowZeroAllowsCompleted — with a
// zero/disabled completion window, a previously COMPLETED identical
// task does NOT block a new tick (the trading-bot cron contract).
func TestHV_FindAutonomyDuplicate_CronWindowZeroAllowsCompleted(t *testing.T) {
	tasks := []*persistence.Task{{
		ID:        "c",
		Status:    persistence.TaskStatusCompleted,
		UpdatedAt: time.Now(),
		Payload:   []byte(`{"taskType":"trading","context":{"prompt":"Run a tick"}}`),
	}}
	_, dup := findAutonomyDuplicate(tasks, "trading", "", "Run a tick", 0)
	assert.False(t, dup, "cron mode (window<=0) must let a completed identical task re-fire")
}

// TestHV_FindAutonomyDuplicate_NormalizesWhitespaceAndCase — prompt
// matching is normalized (lowercase + collapsed whitespace) so cosmetic
// reformatting still dedupes against an active task.
func TestHV_FindAutonomyDuplicate_NormalizesWhitespaceAndCase(t *testing.T) {
	tasks := []*persistence.Task{{
		ID:      "x",
		Status:  persistence.TaskStatusRunning,
		Payload: []byte(`{"taskType":"feature","context":{"prompt":"Build   The  Thing"}}`),
	}}
	_, dup := findAutonomyDuplicate(tasks, "feature", "", "build the thing", time.Hour)
	assert.True(t, dup, "normalized prompt match must dedupe across case/whitespace")
}

// ---------------------------------------------------------------------------
// Failure cooldown boundary (exactly 2 failures, 3h window, source/type).
// ---------------------------------------------------------------------------

// TestHV_AutonomyFailureCooldown_ExactlyTwoTrips confirms the >=2
// threshold: one matching failure does NOT cool down; two does.
func TestHV_AutonomyFailureCooldown_ExactlyTwoTrips(t *testing.T) {
	fail := func(id string, age time.Duration) *persistence.Task {
		return &persistence.Task{
			ID:        id,
			Status:    persistence.TaskStatusFailed,
			CreatedAt: time.Now().Add(-age),
			Payload:   []byte(`{"taskType":"feature","context":{"prompt":"Build X"}}`),
		}
	}
	one := []*persistence.Task{fail("f1", 10*time.Minute)}
	_, cd := autonomyFailureCooldown(one, "feature", "", "Build X")
	assert.False(t, cd, "one failure must not trip cooldown")

	two := []*persistence.Task{fail("f1", 10*time.Minute), fail("f2", 20*time.Minute)}
	_, cd = autonomyFailureCooldown(two, "feature", "", "Build X")
	assert.True(t, cd, "two matching failures must trip cooldown")
}

// TestHV_AutonomyFailureCooldown_IgnoresOldFailures — failures older
// than the 3h window don't count, so two stale failures don't cool down.
func TestHV_AutonomyFailureCooldown_IgnoresOldFailures(t *testing.T) {
	stale := func(id string) *persistence.Task {
		return &persistence.Task{
			ID:        id,
			Status:    persistence.TaskStatusFailed,
			CreatedAt: time.Now().Add(-4 * time.Hour),
			Payload:   []byte(`{"taskType":"feature","context":{"prompt":"Build X"}}`),
		}
	}
	tasks := []*persistence.Task{stale("f1"), stale("f2")}
	_, cd := autonomyFailureCooldown(tasks, "feature", "", "Build X")
	assert.False(t, cd, "failures older than 3h must not count toward cooldown")
}

// TestHV_AutonomyFailureCooldown_OnlyCountsFailedStatus — a COMPLETED
// or RUNNING task with a matching prompt is not a failure and must not
// count toward the cooldown.
func TestHV_AutonomyFailureCooldown_OnlyCountsFailedStatus(t *testing.T) {
	mk := func(id string, st persistence.TaskStatus) *persistence.Task {
		return &persistence.Task{
			ID:        id,
			Status:    st,
			CreatedAt: time.Now().Add(-10 * time.Minute),
			Payload:   []byte(`{"taskType":"feature","context":{"prompt":"Build X"}}`),
		}
	}
	tasks := []*persistence.Task{
		mk("c1", persistence.TaskStatusCompleted),
		mk("r1", persistence.TaskStatusRunning),
	}
	_, cd := autonomyFailureCooldown(tasks, "feature", "", "Build X")
	assert.False(t, cd, "only FAILED tasks count toward the cooldown")
}

// ---------------------------------------------------------------------------
// Circuit breaker boundary (>=8 terminal, >=6 fails, fails > 2*completed).
// ---------------------------------------------------------------------------

func mkTerminal(id string, st persistence.TaskStatus, age time.Duration) *persistence.Task {
	return &persistence.Task{
		ID:             id,
		CreationSource: persistence.TaskCreationSourceAutonomous,
		Status:         st,
		CreatedAt:      time.Now().Add(-age),
		Payload:        []byte(`{"taskType":"feature","context":{"prompt":"x"}}`),
	}
}

// TestHV_AutonomyCircuitOpen_BelowEightTerminal — fewer than 8 terminal
// autonomous tasks never opens the circuit, no matter the fail ratio.
func TestHV_AutonomyCircuitOpen_BelowEightTerminal(t *testing.T) {
	var tasks []*persistence.Task
	for i := 0; i < 7; i++ {
		tasks = append(tasks, mkTerminal(fmt.Sprintf("f%d", i), persistence.TaskStatusFailed, time.Duration(i)*time.Minute))
	}
	_, open := autonomyCircuitOpen(tasks)
	assert.False(t, open, "7 terminal tasks is below the 8-task floor — circuit stays closed")
}

// TestHV_AutonomyCircuitOpen_OpensAtThreshold — 8 terminal with 6 fails
// and 2 completed (6 > 2*2 is false... ensure 6 > 2*completed holds).
// Use 7 failed + 1 completed: 7 fails >= 6 AND 7 > 2*1 => open.
func TestHV_AutonomyCircuitOpen_OpensAtThreshold(t *testing.T) {
	var tasks []*persistence.Task
	for i := 0; i < 7; i++ {
		tasks = append(tasks, mkTerminal(fmt.Sprintf("f%d", i), persistence.TaskStatusFailed, time.Duration(i)*time.Minute))
	}
	tasks = append(tasks, mkTerminal("c0", persistence.TaskStatusCompleted, 30*time.Minute))
	reason, open := autonomyCircuitOpen(tasks)
	assert.True(t, open, "7 failed + 1 completed (8 terminal) must open the circuit")
	assert.Contains(t, reason, "completion ratio too low")
}

// TestHV_AutonomyCircuitOpen_HealthyRatioStaysClosed — 8 terminal but a
// healthy completion ratio (4 fail / 4 complete: 4 fails < 6 floor)
// keeps the circuit closed.
func TestHV_AutonomyCircuitOpen_HealthyRatioStaysClosed(t *testing.T) {
	var tasks []*persistence.Task
	for i := 0; i < 4; i++ {
		tasks = append(tasks, mkTerminal(fmt.Sprintf("f%d", i), persistence.TaskStatusFailed, time.Duration(i)*time.Minute))
		tasks = append(tasks, mkTerminal(fmt.Sprintf("c%d", i), persistence.TaskStatusCompleted, time.Duration(i)*time.Minute))
	}
	_, open := autonomyCircuitOpen(tasks)
	assert.False(t, open, "4 fail / 4 complete is below the 6-failure floor — stays closed")
}

// TestHV_AutonomyCircuitOpen_IgnoresNonAutonomousTerminal — USER-source
// failures don't count toward the circuit; only AUTONOMOUS terminal
// tasks do. Eight USER failures must NOT open the circuit.
func TestHV_AutonomyCircuitOpen_IgnoresNonAutonomousTerminal(t *testing.T) {
	var tasks []*persistence.Task
	for i := 0; i < 9; i++ {
		tk := mkTerminal(fmt.Sprintf("u%d", i), persistence.TaskStatusFailed, time.Duration(i)*time.Minute)
		tk.CreationSource = persistence.TaskCreationSourceUser
		tasks = append(tasks, tk)
	}
	_, open := autonomyCircuitOpen(tasks)
	assert.False(t, open, "non-autonomous failures must not open the autonomy circuit")
}

// ---------------------------------------------------------------------------
// Per-hour rate cap counting + window rollover (boundaries).
// ---------------------------------------------------------------------------

// TestHV_CheckRateLimit_ExactlyAtCap — the cap is exclusive: count <
// cap is allowed, count == cap is refused. Pin the boundary precisely.
func TestHV_CheckRateLimit_ExactlyAtCap(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 2}}

	m.checkRateLimit(p) // seed window (DB empty => count 0)
	assert.True(t, m.checkRateLimit(p), "0 < 2 allowed")
	m.recordTaskCreated("p1")
	assert.True(t, m.checkRateLimit(p), "1 < 2 allowed")
	m.recordTaskCreated("p1")
	assert.False(t, m.checkRateLimit(p), "2 == cap of 2 must refuse")
}

// TestHV_CheckRateLimit_DBSeedExactlyAtCap — the DB-seed cold path
// counts an autonomous task created exactly at the window edge (just
// inside 1h) and refuses when that count meets the cap.
func TestHV_CheckRateLimit_DBSeedJustInsideWindow(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{{
			ID:             "a",
			ProjectID:      "p1",
			CreationSource: persistence.TaskCreationSourceAutonomous,
			CreatedAt:      now.Add(-59 * time.Minute), // inside the 1h window
			Status:         persistence.TaskStatusCompleted,
			Payload:        []byte(`{"taskType":"feature"}`),
		}},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 1}}
	assert.False(t, m.checkRateLimit(p), "task 59m old is inside the 1h window and meets the cap of 1")
}

// TestHV_CountAutonomousTasksLastHour_FiltersSourceAndAge mixes USER,
// old-autonomous, and in-window-autonomous rows and asserts only the
// in-window autonomous one is counted.
func TestHV_CountAutonomousTasksLastHour_FiltersSourceAndAge(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{
			{ID: "auto-recent", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceAutonomous, CreatedAt: now.Add(-5 * time.Minute)},
			{ID: "auto-old", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceAutonomous, CreatedAt: now.Add(-90 * time.Minute)},
			{ID: "user-recent", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceUser, CreatedAt: now.Add(-5 * time.Minute)},
			nil, // nil-row resilience
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	count, err := m.countAutonomousTasksLastHour("p1", now)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only the in-window autonomous task counts")
}

// TestHV_CountAutonomousTasksLastHour_NoRepo — defensive: a nil repo
// returns an error rather than panicking (the rate-limit caller falls
// back to the in-memory count on this error).
func TestHV_CountAutonomousTasksLastHour_NoRepo(t *testing.T) {
	m := New(nil, &registry.Registry{}, nil, nil)
	_, err := m.countAutonomousTasksLastHour("p1", time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task repository not configured")
}

// TestHV_CheckRateLimit_CacheTTLReseeds — once the 5-minute cache TTL
// lapses, the next check re-seeds from the DB, picking up out-of-band
// task creation (here: a new autonomous row appears post-seed).
func TestHV_CheckRateLimit_CacheTTLReseeds(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 1}}

	// First check: DB empty, seeds count=0 => allowed.
	assert.True(t, m.checkRateLimit(p))

	// Out-of-band: a task lands in the DB after the seed.
	repo.mu.Lock()
	repo.tasks = append(repo.tasks, &persistence.Task{
		ID: "ob", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceAutonomous,
		CreatedAt: now, Status: persistence.TaskStatusQueued,
	})
	repo.mu.Unlock()

	// Force the cache to look stale so the next check re-seeds.
	m.mu.Lock()
	m.hourReset["p1"] = now.Add(-6 * time.Minute)
	m.mu.Unlock()

	assert.False(t, m.checkRateLimit(p),
		"after TTL lapse the re-seed must observe the out-of-band task and refuse at cap")
}

// ---------------------------------------------------------------------------
// evaluate() early-exit decisions (skip cleanly, write the right outcome).
// ---------------------------------------------------------------------------

// TestHV_Evaluate_NoActiveGate confirms that when in-flight tasks
// exist, evaluate skips with ACTIVE_TASKS and never reaches the (nil)
// chat client. Seeds a RUNNING task so buildStateContext reports
// hasActive=true.
func TestHV_Evaluate_NoActiveGate(t *testing.T) {
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{{
			ID:        "running",
			ProjectID: "p1",
			Status:    persistence.TaskStatusRunning,
			Payload:   []byte(`{"taskType":"feature","context":{"prompt":"busy"}}`),
			CreatedAt: time.Now(),
		}},
	}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true, Goal: "test", MaxTasksPerHour: 10,
		},
	}
	require.NoError(t, m.evaluate(context.Background(), p),
		"active-tasks gate must skip cleanly without touching the nil chat client")
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeActiveTasks, entries[0].Outcome)
}

// TestHV_Evaluate_CronModeBypassesLLM — a cron-mode project with no
// active tasks fires its goal verbatim through createAutonomousTask
// without ever calling the (nil) chat client. The created task carries
// the resolved cron task type and no idempotency key.
func TestHV_Evaluate_CronModeBypassesLLM(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	p := &registry.Project{
		ID:      "ibkr-trader",
		SwarmID: "", // no swarm => workflow-role validation skipped
		Autonomy: registry.ProjectAutonomy{
			Enabled:          true,
			Mode:             registry.AutonomyModeCron,
			Goal:             "Run one trading tick",
			DuplicateWindow:  "0s",
			CronTaskType:     "trading",
			AllowedTaskTypes: []string{"trading"},
			MaxTasksPerHour:  100,
		},
	}
	require.NoError(t, m.evaluate(context.Background(), p),
		"cron mode must bypass the LLM and not panic on the nil client")
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, persistence.TaskStatusQueued, tasks[0].Status)
	assert.Nil(t, tasks[0].IdempotencyKey, "cron-mode task carries no idempotency key")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(tasks[0].Payload, &payload))
	assert.Equal(t, "trading", payload["taskType"])
}

// ---------------------------------------------------------------------------
// tickCron / tickBacklog deterministic engines.
// ---------------------------------------------------------------------------

// TestHV_TickCron_EmptyGoalSkips — defence-in-depth: a cron tick with
// an empty goal records a PARSE_ERROR and creates nothing rather than
// firing a blank prompt.
func TestHV_TickCron_EmptyGoalSkips(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{Mode: registry.AutonomyModeCron, Goal: "   "}}

	require.NoError(t, m.tickCron(context.Background(), p, time.Now()))
	assert.Empty(t, repo.createdTasks())
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeParseError, entries[0].Outcome)
}

// TestHV_TickCron_ResolvesTaskTypeFromAllowed — when CronTaskType is
// unset, the cron tick falls back to AllowedTaskTypes[0] so the
// allowedTaskTypes gate inside createAutonomousTask passes.
func TestHV_TickCron_ResolvesTaskTypeFromAllowed(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Mode:             registry.AutonomyModeCron,
			Goal:             "Heartbeat",
			DuplicateWindow:  "0s",
			AllowedTaskTypes: []string{"heartbeat"},
		},
	}
	require.NoError(t, m.tickCron(context.Background(), p, time.Now()))
	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(tasks[0].Payload, &payload))
	assert.Equal(t, "heartbeat", payload["taskType"], "cron type falls back to AllowedTaskTypes[0]")
}

// TestHV_TickBacklog_NoWorkspaceSkips — backlog mode without a
// configured workspace path skips cleanly (DB_ERROR audit) instead of
// dereferencing an empty path.
func TestHV_TickBacklog_NoWorkspaceSkips(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{Mode: registry.AutonomyModeBacklog}}

	require.NoError(t, m.tickBacklog(context.Background(), p, time.Now()))
	assert.Empty(t, repo.createdTasks())
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeDBError, entries[0].Outcome)
}

// TestHV_TickBacklog_FiresItemAndMarksConsumed — the happy path: read
// the first pending item, create a task with its text, and rewrite the
// file marking that line `- [x]` only after the create succeeds.
func TestHV_TickBacklog_FiresItemAndMarksConsumed(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	backlog := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [ ] Ship the thing\n- [ ] Later\n"), 0o644))

	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithWorkspacePath(ws))
	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Mode:            registry.AutonomyModeBacklog,
			DuplicateWindow: "0s", // avoid 24h dedupe interfering
		},
	}
	require.NoError(t, m.tickBacklog(context.Background(), p, time.Now()))

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, "Ship the thing", extractPrompt(tasks[0].Payload))

	got, err := os.ReadFile(backlog)
	require.NoError(t, err)
	assert.Contains(t, string(got), "- [x] Ship the thing")
	assert.Contains(t, string(got), "- [ ] Later", "subsequent items stay pending")
}

// TestHV_TickBacklog_NoPendingItemsNoOp — a backlog with only completed
// items records NO_ACTION and writes nothing back.
func TestHV_TickBacklog_NoPendingItemsNoOp(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))
	backlog := filepath.Join(ws, "p1", "BACKLOG.md")
	require.NoError(t, os.WriteFile(backlog, []byte("- [x] done\n"), 0o644))

	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithWorkspacePath(ws), WithEvaluationRepository(evalRepo))
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{Mode: registry.AutonomyModeBacklog}}

	require.NoError(t, m.tickBacklog(context.Background(), p, time.Now()))
	assert.Empty(t, repo.createdTasks())
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeNoAction, entries[0].Outcome)
}

// TestHV_TickBacklog_MissingFileNoOp — absent BACKLOG.md is treated as
// an empty backlog (NO_ACTION), not a loop-failing error.
func TestHV_TickBacklog_MissingFileNoOp(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "p1"), 0o755))

	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithWorkspacePath(ws), WithEvaluationRepository(evalRepo))
	p := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{Mode: registry.AutonomyModeBacklog}}

	require.NoError(t, m.tickBacklog(context.Background(), p, time.Now()))
	assert.Empty(t, repo.createdTasks())
	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeNoAction, entries[0].Outcome)
}

// ---------------------------------------------------------------------------
// Idempotency-key + duplicate-window helpers (pure).
// ---------------------------------------------------------------------------

// TestHV_BuildAutonomyIdempotencyKey_HourBucketAndInputs — the key is
// stable within an hour bucket and varies with the bucket, prompt, type
// and workflow. This is the safety-net that catches an exact double-fire
// inside one hour for backlog-style projects.
func TestHV_BuildAutonomyIdempotencyKey_HourBucketAndInputs(t *testing.T) {
	t0 := time.Date(2026, 6, 17, 14, 5, 0, 0, time.UTC)
	t0b := time.Date(2026, 6, 17, 14, 55, 0, 0, time.UTC) // same hour bucket
	t1 := time.Date(2026, 6, 17, 15, 5, 0, 0, time.UTC)   // next bucket

	base := buildAutonomyIdempotencyKey("p1", "feature", "wf", "Build X", t0)
	assert.Equal(t, base, buildAutonomyIdempotencyKey("p1", "feature", "wf", "Build X", t0b),
		"same hour bucket + same inputs => identical key")
	assert.NotEqual(t, base, buildAutonomyIdempotencyKey("p1", "feature", "wf", "Build X", t1),
		"next hour bucket => different key")
	assert.NotEqual(t, base, buildAutonomyIdempotencyKey("p1", "bugfix", "wf", "Build X", t0),
		"different type => different key")
	assert.NotEqual(t, base, buildAutonomyIdempotencyKey("p1", "feature", "wf", "Build Y", t0),
		"different prompt => different key")
	// Normalization: case/whitespace variants collapse to the same key.
	assert.Equal(t, base, buildAutonomyIdempotencyKey("p1", "feature", "wf", "build   x", t0),
		"prompt normalization must yield a stable key")
}

// TestHV_AutonomyDuplicateWindow_Resolution pins the window resolution
// contract: unset => 24h; "0"/"0s" => 0 (cron, dedup disabled);
// negative/garbage => 24h default; valid => parsed.
func TestHV_AutonomyDuplicateWindow_Resolution(t *testing.T) {
	mk := func(v string) *registry.Project {
		return &registry.Project{Autonomy: registry.ProjectAutonomy{DuplicateWindow: v}}
	}
	assert.Equal(t, 24*time.Hour, autonomyDuplicateWindow(mk("")), "unset => 24h")
	assert.Equal(t, time.Duration(0), autonomyDuplicateWindow(mk("0s")), "0s => cron disabled")
	assert.Equal(t, time.Duration(0), autonomyDuplicateWindow(mk("0")), "0 => cron disabled")
	assert.Equal(t, 24*time.Hour, autonomyDuplicateWindow(mk("-5m")), "negative => 24h default")
	assert.Equal(t, 24*time.Hour, autonomyDuplicateWindow(mk("nonsense")), "garbage => 24h default")
	assert.Equal(t, 6*time.Hour, autonomyDuplicateWindow(mk("6h")), "valid => parsed")
	assert.Equal(t, 24*time.Hour, autonomyDuplicateWindow(nil), "nil project => 24h default")
}
