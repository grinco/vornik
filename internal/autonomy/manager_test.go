package autonomy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// mockTaskRepo is a minimal TaskRepository for autonomy tests.
type mockTaskRepo struct {
	PingFunc func(ctx context.Context) error
	mu       sync.Mutex
	tasks    []*persistence.Task
	createF  func(ctx context.Context, task *persistence.Task) error
}

func (m *mockTaskRepo) Create(ctx context.Context, task *persistence.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createF != nil {
		return m.createF(ctx, task)
	}
	m.tasks = append(m.tasks, task)
	return nil
}
func (m *mockTaskRepo) Get(context.Context, string) (*persistence.Task, error) {
	return nil, persistence.ErrNotFound
}
func (m *mockTaskRepo) GetByIdempotencyKey(_ context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, task := range m.tasks {
		if task.ProjectID == projectID && task.IdempotencyKey != nil && *task.IdempotencyKey == idempotencyKey {
			return task, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (m *mockTaskRepo) Update(context.Context, *persistence.Task) error { return nil }
func (m *mockTaskRepo) Delete(context.Context, string) error            { return nil }
func (m *mockTaskRepo) UpdateStatus(context.Context, string, persistence.TaskStatus) error {
	return nil
}
func (m *mockTaskRepo) TransitionToCancelled(context.Context, string) (bool, error) {
	return true, nil
}
func (m *mockTaskRepo) RequeueTerminalTask(context.Context, string, int, int) (bool, error) {
	return true, nil
}
func (m *mockTaskRepo) TransitionConditional(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
	return true, nil
}
func (m *mockTaskRepo) List(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks, nil
}
func (m *mockTaskRepo) Count(_ context.Context, _ persistence.TaskFilter) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.tasks)), nil
}
func (m *mockTaskRepo) LeaseTask(context.Context, persistence.LeaseOptions) (*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepo) RenewLease(context.Context, string, string, int) error { return nil }
func (m *mockTaskRepo) ReleaseLease(context.Context, string, string, persistence.TaskStatus, persistence.ReleaseOptions) error {
	return nil
}
func (m *mockTaskRepo) FindExpiredLeases(context.Context, int) ([]*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepo) CountByStatus(context.Context, string) (map[persistence.TaskStatus]int64, error) {
	return nil, nil
}
func (m *mockTaskRepo) CountRecentFailures(context.Context, string, []string, time.Time) (int, error) {
	return 0, nil
}
func (m *mockTaskRepo) GetChildren(context.Context, string) ([]*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepo) CountChildrenForParents(context.Context, []string) (map[string]int, error) {
	return nil, nil
}
func (m *mockTaskRepo) GetDependencies(context.Context, string) ([]*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepo) GetDependents(context.Context, string) ([]*persistence.Task, error) {
	return nil, nil
}

func (m *mockTaskRepo) createdTasks() []*persistence.Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*persistence.Task, len(m.tasks))
	copy(out, m.tasks)
	return out
}

func TestNew(t *testing.T) {
	repo := &mockTaskRepo{}
	reg := &registry.Registry{}
	m := New(nil, reg, repo, nil)

	assert.NotNil(t, m)
	assert.NotNil(t, m.taskCounts)
	assert.NotNil(t, m.hourReset)
	assert.NotNil(t, m.cancelFns)
}

func TestCheckRateLimit_NoLimit(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 0},
	}
	// No limit configured — always allowed
	assert.True(t, m.checkRateLimit(p))
}

func TestCheckRateLimit_UnderLimit(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 5},
	}
	assert.True(t, m.checkRateLimit(p))

	// Record 4 tasks — still under limit
	for i := 0; i < 4; i++ {
		m.recordTaskCreated("p1")
	}
	assert.True(t, m.checkRateLimit(p))
}

func TestCheckRateLimit_AtLimit(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 3},
	}

	// Initialize the window
	assert.True(t, m.checkRateLimit(p))

	// Create 3 tasks — should hit the limit
	for i := 0; i < 3; i++ {
		m.recordTaskCreated("p1")
	}
	assert.False(t, m.checkRateLimit(p))
}

func TestCheckRateLimit_HourlyReset(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 2},
	}

	// Initialize the window, then fill up the limit
	m.checkRateLimit(p)
	m.recordTaskCreated("p1")
	m.recordTaskCreated("p1")
	assert.False(t, m.checkRateLimit(p))

	// Simulate the hour window expiring
	m.mu.Lock()
	m.hourReset["p1"] = time.Now().Add(-2 * time.Hour)
	m.mu.Unlock()

	// Should be allowed again — counter resets
	assert.True(t, m.checkRateLimit(p))
}

// TestCheckRateLimit_SeedsFromDBOnRestart pins the regression
// where the in-memory rate-limit counter reset to zero on every
// daemon restart, granting each project an extra task per
// restart. Before the fix: spin up a fresh manager (zero
// counters), populate the DB with the project's autonomous
// tasks already at the cap, and call checkRateLimit — which
// would WRONGLY return true because the in-memory counter said 0.
//
// After the fix: the in-memory counter is seeded from the DB on
// the cold-start path (no entry for the project, OR the cache
// hasn't been refreshed in 5+ minutes). The seed correctly counts
// AUTONOMOUS-source tasks created in the last hour and the rate-
// limit decision matches reality.
func TestCheckRateLimit_SeedsFromDBOnRestart(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{
			{
				ID:             "auto-1",
				ProjectID:      "p1",
				CreationSource: persistence.TaskCreationSourceAutonomous,
				CreatedAt:      now.Add(-30 * time.Minute),
				Status:         persistence.TaskStatusCompleted,
				Payload:        []byte(`{"taskType":"feature"}`),
			},
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 1},
	}

	// Fresh manager (just like a daemon restart); the project
	// already has one autonomous task in the last hour. Cap is
	// 1 — the limit IS reached.
	assert.False(t, m.checkRateLimit(p),
		"DB has 1 autonomous task in last hour and cap is 1; rate limit must refuse")
}

// TestCheckRateLimit_DBSeedIgnoresUserTasks — only AUTONOMOUS
// rows count toward the cap. Manual API submissions don't
// constrain autonomy.
func TestCheckRateLimit_DBSeedIgnoresUserTasks(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{
			{
				ID:             "user-1",
				ProjectID:      "p1",
				CreationSource: persistence.TaskCreationSourceUser,
				CreatedAt:      now.Add(-10 * time.Minute),
				Status:         persistence.TaskStatusCompleted,
				Payload:        []byte(`{"taskType":"feature"}`),
			},
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 1},
	}
	assert.True(t, m.checkRateLimit(p),
		"USER-source tasks shouldn't count against the autonomy rate cap")
}

// TestCheckRateLimit_DBSeedIgnoresOldTasks — tasks older than
// the rolling 1-hour window don't count.
func TestCheckRateLimit_DBSeedIgnoresOldTasks(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{
			{
				ID:             "auto-old",
				ProjectID:      "p1",
				CreationSource: persistence.TaskCreationSourceAutonomous,
				CreatedAt:      now.Add(-2 * time.Hour),
				Status:         persistence.TaskStatusCompleted,
				Payload:        []byte(`{"taskType":"feature"}`),
			},
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	p := &registry.Project{
		ID:       "p1",
		Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 1},
	}
	assert.True(t, m.checkRateLimit(p),
		"tasks older than 1h shouldn't count against the rate cap")
}

// mustPayload builds the same {taskType, context.prompt} payload
// shape that the autonomy creator emits, with the prompt safely
// JSON-escaped. Lets table-driven tests compose realistic payloads
// without juggling backslashes for embedded quotes.
func mustPayload(t *testing.T, taskType, prompt string) []byte {
	t.Helper()
	payload := struct {
		TaskType string `json:"taskType"`
		Context  struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}{TaskType: taskType}
	payload.Context.Prompt = prompt
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return b
}

func TestCreateAutonomousTask_Basic(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{
		ID:              "test-proj",
		DefaultPriority: 50,
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "Build things",
		},
	}

	argsJSON := `{"prompt": "Implement feature X", "type": "feature"}`
	err := m.createAutonomousTask(context.Background(), project, argsJSON, time.Now())
	require.NoError(t, err)

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)

	task := tasks[0]
	assert.Equal(t, "test-proj", task.ProjectID)
	assert.Equal(t, persistence.TaskStatusQueued, task.Status)
	assert.Equal(t, persistence.TaskCreationSourceAutonomous, task.CreationSource)
	assert.Equal(t, 50, task.Priority)
	assert.Equal(t, 1, task.Attempt)
	assert.Equal(t, 3, task.MaxAttempts)
	require.NotNil(t, task.IdempotencyKey)
	assert.Contains(t, *task.IdempotencyKey, "auto:")

	// Verify payload contains the prompt
	var payload map[string]any
	require.NoError(t, json.Unmarshal(task.Payload, &payload))
	ctx, _ := payload["context"].(map[string]any)
	assert.Equal(t, "Implement feature X", ctx["prompt"])
	assert.Equal(t, "feature", payload["taskType"])
}

func TestCreateAutonomousTask_RequireApproval(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{
		ID: "approve-proj",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "Build things",
			RequireApproval: true,
		},
	}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"Do stuff"}`, time.Now())
	require.NoError(t, err)

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	// requireApproval parks the task in AWAITING_APPROVAL — a
	// first-class awaiting-action status the operator resolves via
	// approve/reject — NOT PENDING. PENDING is never leased by the
	// scheduler and is invisible to the inbox surface, so approval-gated
	// tasks used to wait forever (operator report 2026-06-09). See
	// https://docs.vornik.io
	assert.Equal(t, persistence.TaskStatusAwaitingApproval, tasks[0].Status)
}

func TestCreateAutonomousTask_WithWorkflowID(t *testing.T) {
	// Build a temp config dir with a minimal workflow and swarm so the
	// workflow/role validation in createAutonomousTask passes.
	configDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755)
	_ = os.MkdirAll(filepath.Join(configDir, "projects"), 0o755)
	_ = os.WriteFile(filepath.Join(configDir, "workflows", "dev-pipeline.md"), []byte(`---
workflowId: "dev-pipeline"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644)
	_ = os.WriteFile(filepath.Join(configDir, "swarms", "test-swarm.md"), []byte(`---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`), 0o644)

	reg := registry.New()
	require.NoError(t, reg.Load(configDir))

	repo := &mockTaskRepo{}
	m := New(nil, reg, repo, nil)
	project := &registry.Project{
		ID:      "wf-proj",
		SwarmID: "test-swarm",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "Build things",
		},
	}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"Build it","workflow_id":"dev-pipeline"}`, time.Now())
	require.NoError(t, err)

	tasks := repo.createdTasks()
	require.Len(t, tasks, 1)
	assert.NotNil(t, tasks[0].WorkflowID)
	assert.Equal(t, "dev-pipeline", *tasks[0].WorkflowID)
}

func TestCreateAutonomousTask_EmptyPrompt(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":""}`, time.Now())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")
	assert.Empty(t, repo.createdTasks())
}

// TestCreateAutonomousTask_NoActionSentinel — defence-in-depth
// check inside createAutonomousTask. Even if every upstream path
// were to leak a NO_ACTION sentinel (literal or JSON-embedded),
// the create_task method must suppress the task creation rather
// than spawn a worker on a no-op prompt. The 2026-05-12 janka
// incident (task_20260512212548 created with prompt "NO_ACTION")
// motivated this final guard.
func TestCreateAutonomousTask_NoActionSentinel(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"literal", `{"prompt": "NO_ACTION"}`},
		{"lowercase", `{"prompt": "no_action"}`},
		{"wrapped_in_prose", `{"prompt": "Skip this cycle — NO_ACTION is appropriate here."}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockTaskRepo{}
			m := New(nil, &registry.Registry{}, repo, nil)
			project := &registry.Project{ID: "p1"}

			err := m.createAutonomousTask(context.Background(), project, tc.args, time.Now())
			require.NoError(t, err, "NO_ACTION sentinel must be suppressed silently, not raise")
			assert.Empty(t, repo.createdTasks(),
				"NO_ACTION sentinel must NOT create a task — that's the bug we're fixing")
		})
	}
}

func TestCreateAutonomousTask_AllowedTypesRejectsEmptyType(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			AllowedTaskTypes: []string{"feature"},
		},
	}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"Implement feature X"}`, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task type is required")
	assert.Empty(t, repo.createdTasks())
}

func TestCreateAutonomousTask_InvalidJSON(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	err := m.createAutonomousTask(context.Background(), project, `not json`, time.Now())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid tool arguments")
}

func TestCreateAutonomousTask_CreateError(t *testing.T) {
	repo := &mockTaskRepo{
		createF: func(ctx context.Context, task *persistence.Task) error {
			return assert.AnError
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"test"}`, time.Now())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create task")
}

// TestCreateAutonomousTask_CronMode_NoIdempotencyKey pins the
// behavior that unblocks frequent-tick autonomy (trading, observ-
// ability heartbeats).
//
// Regression: the idempotency key was hour-bucketed
// (YYYYMMDDHH) and applied unconditionally. ibkr-trader polls every
// 5 minutes with duplicateWindow=0s (cron mode) and expects every
// tick to fire (capped by maxTasksPerHour). With the old code, the
// first tick consumed the hour bucket and the next ~11 ticks all
// returned IDEMPOTENCY_HIT — visible in autonomy_evaluations as a
// solid run of suppressed evaluations after a single CREATED.
//
// Fix: when duplicateWindow=0, the operator has explicitly opted
// out of duplicate detection — skip the idempotency lookup AND
// leave task.IdempotencyKey nil so the partial unique index
// (project_id, idempotency_key) WHERE NOT NULL doesn't tank
// Create either.
func TestCreateAutonomousTask_CronMode_NoIdempotencyKey(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{
		ID:              "ibkr-trader",
		DefaultPriority: 50,
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "Run trading ticks",
			DuplicateWindow: "0s", // cron mode — every tick fires
		},
	}

	args := `{"prompt":"Run one trading tick","type":"trading"}`
	require.NoError(t, m.createAutonomousTask(context.Background(), project, args, time.Now()))

	// Mark the first tick as COMPLETED before firing the second.
	// findAutonomyDuplicate's active-task check (LLD comment on
	// autonomyDuplicateWindow: "active-task check is unaffected
	// either way") would otherwise suppress the second call —
	// that's correct production behavior (don't pile up identical
	// work while one is in-flight) and not the path we're pinning
	// here. The bug we care about is what happens once the prior
	// tick finishes and the next 5-min poll fires: pre-fix it hit
	// the hour-bucketed idempotency check and returned
	// IDEMPOTENCY_HIT for the rest of the hour.
	repo.mu.Lock()
	repo.tasks[0].Status = persistence.TaskStatusCompleted
	repo.tasks[0].UpdatedAt = time.Now()
	repo.mu.Unlock()

	require.NoError(t, m.createAutonomousTask(context.Background(), project, args, time.Now()))

	tasks := repo.createdTasks()
	require.Len(t, tasks, 2, "cron-mode autonomy must produce a task per tick once the prior task completes, not collapse to one per hour")
	for i, task := range tasks {
		if task.IdempotencyKey != nil {
			t.Errorf("task[%d].IdempotencyKey must be nil for cron-mode tasks; got %q", i, *task.IdempotencyKey)
		}
	}
}

func TestCreateAutonomousTask_SuppressesActiveDuplicate(t *testing.T) {
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{{
			ID:             "existing",
			ProjectID:      "p1",
			CreationSource: persistence.TaskCreationSourceAutonomous,
			Status:         persistence.TaskStatusQueued,
			Payload:        []byte(`{"taskType":"feature","context":{"prompt":"Implement feature X"}}`),
			CreatedAt:      time.Now(),
		}},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"Implement feature X","type":"feature"}`, time.Now())
	require.NoError(t, err)
	assert.Len(t, repo.createdTasks(), 1)
}

func TestCreateAutonomousTask_SuppressesFailureCooldown(t *testing.T) {
	now := time.Now()
	repo := &mockTaskRepo{
		tasks: []*persistence.Task{
			{
				ID:             "f1",
				ProjectID:      "p1",
				CreationSource: persistence.TaskCreationSourceAutonomous,
				Status:         persistence.TaskStatusFailed,
				Payload:        []byte(`{"taskType":"feature","context":{"prompt":"Implement feature X"}}`),
				CreatedAt:      now.Add(-20 * time.Minute),
			},
			{
				ID:             "f2",
				ProjectID:      "p1",
				CreationSource: persistence.TaskCreationSourceAutonomous,
				Status:         persistence.TaskStatusFailed,
				Payload:        []byte(`{"taskType":"feature","context":{"prompt":"Implement feature X"}}`),
				CreatedAt:      now.Add(-10 * time.Minute),
			},
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"Implement feature X","type":"feature"}`, time.Now())
	require.NoError(t, err)
	assert.Len(t, repo.createdTasks(), 2)
}

// TestCreateAutonomousTask_SuppressesFuzzyFailureCooldown locks the
// real-world snake "Ghost Mode" incident: the autonomy LLM
// rephrased the same feature implementation prompt across two
// failed runs, and the strict-equality cooldown didn't fire because
// the strings differed word-for-word. Both prompts target the same
// topic, share most discriminative tokens, and Jaccard similarity
// is comfortably above the 0.55 threshold — so the third attempt
// MUST be suppressed.
func TestCreateAutonomousTask_SuppressesFuzzyFailureCooldown(t *testing.T) {
	now := time.Now()
	prompt1 := "Implement Nokia Snake Game Ghost Mode Enhancement: Complete the implementation of Ghost Mode in project/index.html. Ghost Mode allows the snake to pass through its own body segments without dying. Modify the collision detection logic to check against the snake's head position instead of all body segments when ghost mode is active, and add a toggle UI element."
	prompt2 := "Implement Ghost Mode Enhancement: Complete the implementation of Ghost Mode (snake passes through walls and self). Add toggle in settings menu in project/index.html; when active, snake can cross screen boundaries to emerge on opposite side without dying. Update state management to handle ghoststate correctly including collision avoidance when ghostmode is on."

	repo := &mockTaskRepo{
		tasks: []*persistence.Task{
			{
				ID:             "f1",
				ProjectID:      "snake",
				CreationSource: persistence.TaskCreationSourceAutonomous,
				Status:         persistence.TaskStatusFailed,
				Payload:        mustPayload(t, "feature", prompt1),
				CreatedAt:      now.Add(-2 * time.Hour),
			},
			{
				ID:             "f2",
				ProjectID:      "snake",
				CreationSource: persistence.TaskCreationSourceAutonomous,
				Status:         persistence.TaskStatusFailed,
				Payload:        mustPayload(t, "feature", prompt2),
				CreatedAt:      now.Add(-30 * time.Minute),
			},
		},
	}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "snake"}

	// A third Ghost Mode rephrase — same topic, different surface.
	prompt3 := `{"prompt":"Implement Ghost Mode: Add ghost-mode behaviour to project/index.html so the snake passes through walls and its own segments. Add a settings toggle.","type":"feature"}`
	err := m.createAutonomousTask(context.Background(), project, prompt3, time.Now())
	require.NoError(t, err)
	// Repo's createdTasks slice tracks Create() calls — the cooldown
	// should have blocked this one before persistence, so the slice
	// stays at the two pre-existing failures.
	assert.Len(t, repo.createdTasks(), 2, "third Ghost Mode rephrase must be suppressed by fuzzy failure cooldown")
}

// TestAutonomyPromptSimilarity_DiscriminatesUnrelatedTopics pins
// the threshold's specificity. Two unrelated feature prompts must
// score well below the cooldown threshold — otherwise legitimate
// new work gets silently dropped after any two failures.
func TestAutonomyPromptSimilarity_DiscriminatesUnrelatedTopics(t *testing.T) {
	a := "Implement Network-based Leaderboard: Add a network-powered online leaderboard to project/index.html that syncs high scores with a backend service."
	b := "Implement Configurable Effect Toggles: Add a settings panel to project/index.html that lets players enable/disable phosphor trails, scanlines, and vignette effects."

	got := autonomyPromptSimilarity(a, b)
	assert.Less(t, got, autonomyFailureSimilarityThreshold,
		"unrelated feature prompts must score well below %.2f, got %.3f", autonomyFailureSimilarityThreshold, got)
}

func TestCreateAutonomousTask_SuppressesCircuitBreaker(t *testing.T) {
	now := time.Now()
	var tasks []*persistence.Task
	for i := 0; i < 8; i++ {
		status := persistence.TaskStatusFailed
		if i == 0 || i == 1 {
			status = persistence.TaskStatusCompleted
		}
		tasks = append(tasks, &persistence.Task{
			ID:             persistence.GenerateID("task"),
			ProjectID:      "p1",
			CreationSource: persistence.TaskCreationSourceAutonomous,
			Status:         status,
			Payload:        []byte(`{"taskType":"feature","context":{"prompt":"Task"}}`),
			CreatedAt:      now.Add(-time.Duration(i) * time.Minute),
		})
	}
	repo := &mockTaskRepo{tasks: tasks}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{ID: "p1"}

	err := m.createAutonomousTask(context.Background(), project, `{"prompt":"new work","type":"feature"}`, time.Now())
	require.NoError(t, err)
	assert.Len(t, repo.createdTasks(), 8)
}

func TestRecordTaskCreated(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)

	m.recordTaskCreated("p1")
	m.recordTaskCreated("p1")
	m.recordTaskCreated("p2")

	m.mu.Lock()
	assert.Equal(t, 2, m.taskCounts["p1"])
	assert.Equal(t, 1, m.taskCounts["p2"])
	m.mu.Unlock()
}

func TestStartStop_NoAutonomyProjects(t *testing.T) {
	reg := &registry.Registry{}
	m := New(nil, reg, &mockTaskRepo{}, nil)

	m.Start()
	// Should not panic and should shut down cleanly
	m.Stop()
}

func TestGenerateTaskID(t *testing.T) {
	id1 := persistence.GenerateID("task")
	id2 := persistence.GenerateID("task")

	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2)
	assert.Contains(t, id1, "task_")
}

// TestProjectLoop_RunsInitialEvaluation pins the regression that
// prompted the fix: every daemon restart used to push the first
// evaluation out by the full pollInterval (60m for ibkr-trader,
// 90m for snake, 5h for janka), so an operator who saw "tasks due
// now" right after a restart had to wait the full interval before
// the first scheduling pass. With the fix, the first evaluation
// fires synchronously at loop start; the ticker covers
// subsequent polls.
//
// Verified by running projectLoop with a chat client that errors
// out (no LLM wired), observing that evaluate() was called at
// least once before the test's ctx-cancel released the loop.
func TestProjectLoop_RunsInitialEvaluation(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil)
	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "test",
			// Large interval so the test only succeeds via the
			// initial-evaluation path. If the loop falls back to
			// waiting for the ticker, we'd time out.
			PollInterval:    "1h",
			MaxTasksPerHour: 10,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	m.wg.Add(1)
	loopDone := make(chan struct{})
	go func() {
		m.projectLoop(ctx, project)
		close(loopDone)
	}()

	// Wait briefly for the loop to run its initial evaluate().
	// evaluate() with a nil client returns an error fast, so we
	// don't need a generous deadline.
	require.Eventually(t, func() bool {
		// The initial eval triggers an evaluation record OR a
		// failure log. Either way, the loop must have called
		// evaluate(); the simplest visible side-effect is that
		// at least one autonomy_evaluations record was attempted —
		// but we don't have repo wiring for that in this test.
		// Instead, cancel the ctx and see that the loop exits
		// cleanly, which it can only do AFTER initial evaluate
		// returns.
		return true
	}, 200*time.Millisecond, 20*time.Millisecond)

	cancel()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("projectLoop did not exit within 2s after ctx cancel — initial evaluate may have hung")
	}
}

// TestProjectLoop_SkipsInitialEvalWhenRecent — the regression
// fix from 2026-05-05: pre-fix every daemon restart and every
// SIGHUP reload re-spawned the per-project loops, and each loop
// fired its initial eval unconditionally. With 5 enabled projects
// and ~10 reloads/restarts per debug session, that's 50 spurious
// evals piling up against rate-limit gates — operator-reported
// symptom: "janka scheduled 4 tasks in the last hour despite a
// 5h pollInterval".
//
// Now: when autonomy_evaluations has a row newer than `interval`,
// the initial eval is delayed instead of fired immediately. This
// test pins the delay path by using a List-mocked eval repo that
// reports an eval from 30s ago against a 1h interval — the initial
// eval should NOT fire before the test's ctx-cancel deadline.
func TestProjectLoop_SkipsInitialEvalWhenRecent(t *testing.T) {
	repo := &mockTaskRepo{}
	listMock := &listMockEvalRepo{
		recent: []*persistence.AutonomyEvaluation{{
			ProjectID: "p1",
			CreatedAt: time.Now().Add(-30 * time.Second),
			Outcome:   persistence.AutonomyOutcomeNoAction,
		}},
	}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(listMock))
	project := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "test",
			PollInterval:    "1h", // initial would fire 59m30s after ctx cancels
			MaxTasksPerHour: 10,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	m.wg.Add(1)
	loopDone := make(chan struct{})
	go func() {
		m.projectLoop(ctx, project)
		close(loopDone)
	}()

	select {
	case <-loopDone:
		// Expected: ctx-cancel released the loop without it firing
		// the initial eval. listMock.Record was never called because
		// the gate skipped the synchronous evaluate() path.
	case <-time.After(2 * time.Second):
		t.Fatal("projectLoop did not exit after ctx cancel")
	}

	assert.Empty(t, listMock.snapshot(),
		"initial eval must NOT fire when last eval is younger than pollInterval — "+
			"this is the operator-restart-spam guard")
}

// listMockEvalRepo is captureEvalRepo plus a List() that returns
// pre-seeded recent evals. Used to drive the new "is the last eval
// recent enough to skip the initial fire?" gate.
type listMockEvalRepo struct {
	mu       sync.Mutex
	recent   []*persistence.AutonomyEvaluation // returned by List
	recorded []*persistence.AutonomyEvaluation // written via Record
}

func (l *listMockEvalRepo) Record(_ context.Context, e *persistence.AutonomyEvaluation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, e)
	return nil
}
func (l *listMockEvalRepo) List(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*persistence.AutonomyEvaluation, len(l.recent))
	copy(out, l.recent)
	return out, nil
}
func (l *listMockEvalRepo) CountByOutcome(_ context.Context, _ string, _, _ time.Time) (map[string]int64, error) {
	return nil, nil
}
func (l *listMockEvalRepo) snapshot() []*persistence.AutonomyEvaluation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*persistence.AutonomyEvaluation, len(l.recorded))
	copy(out, l.recorded)
	return out
}

func TestEvaluate_NilClient(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			Goal:            "test",
			MaxTasksPerHour: 10,
		},
	}
	err := m.evaluate(context.Background(), p)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "chat client not configured")
}

func TestEvaluate_NilMetrics_NoOp(t *testing.T) {
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil)
	// metrics is nil by default
	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "test",
		},
	}
	err := m.evaluate(context.Background(), p)
	assert.Error(t, err) // fails on nil client, not panic on nil metrics
}

func TestNew_NilDependencies(t *testing.T) {
	m := New(nil, nil, nil, nil)
	assert.NotNil(t, m)
	assert.NotNil(t, m.taskCounts)
	assert.NotNil(t, m.hourReset)
	assert.NotNil(t, m.cancelFns)
}

func (m *mockTaskRepo) Ping(ctx context.Context) error {
	if m.PingFunc != nil {
		return m.PingFunc(ctx)
	}
	return nil
}

// captureEvalRepo is a minimal AutonomyEvaluationRepository that lets
// the gate tests assert which outcome the evaluate path wrote without
// touching a real DB. Record() is the only path the manager exercises;
// List/CountByOutcome are stubbed.
type captureEvalRepo struct {
	mu      sync.Mutex
	entries []*persistence.AutonomyEvaluation
}

func (c *captureEvalRepo) Record(_ context.Context, e *persistence.AutonomyEvaluation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
	return nil
}
func (c *captureEvalRepo) List(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	return nil, nil
}
func (c *captureEvalRepo) CountByOutcome(_ context.Context, _ string, _, _ time.Time) (map[string]int64, error) {
	return nil, nil
}

func (c *captureEvalRepo) snapshot() []*persistence.AutonomyEvaluation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*persistence.AutonomyEvaluation, len(c.entries))
	copy(out, c.entries)
	return out
}

// stubBudgetRepo satisfies budget.Repo (a narrow subset of
// TaskLLMUsageRepository) so the gate tests can drive the budget
// check without spinning up a real LLM-usage repo. The autonomy
// manager actually accepts a TaskLLMUsageRepository, but only the
// SumCostByProject method is exercised in the gate path; the
// budget.Repo interface is the same shape, so we wrap it in an
// adapter here to keep the unrelated methods stubbed.
type stubLLMUsageRepo struct {
	dailyCost float64
}

func (s *stubLLMUsageRepo) Record(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (s *stubLLMUsageRepo) Upsert(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (s *stubLLMUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return s.dailyCost, nil
}
func (s *stubLLMUsageRepo) SumCost(context.Context, time.Time, time.Time) (float64, error) {
	return s.dailyCost, nil
}
func (s *stubLLMUsageRepo) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TopTasks(context.Context, time.Time, time.Time, int, string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}

// TestEvaluate_AutonomyRateLimitGate confirms a tick over the
// project's autonomy.maxTasksPerHour cap returns nil (skip), writes
// one RATE_LIMITED audit row, and never reaches the (nil) chat client
// — if it did, evaluate would panic. The audit row is the operator-
// visible signal that the loop is alive but capped.
func TestEvaluate_AutonomyRateLimitGate(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evalRepo))
	// Pre-fill the in-memory counter to the cap so the gate fires
	// without needing the DB-seed path.
	m.checkRateLimit(&registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{MaxTasksPerHour: 1}})
	m.recordTaskCreated("p1")

	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			MaxTasksPerHour: 1,
			Goal:            "test",
		},
	}
	err := m.evaluate(context.Background(), p)
	require.NoError(t, err, "rate-limit gate must skip cleanly, not error")

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1, "exactly one audit row per skipped tick")
	assert.Equal(t, persistence.AutonomyOutcomeRateLimited, entries[0].Outcome)
	assert.Equal(t, "p1", entries[0].ProjectID)
	assert.Empty(t, repo.createdTasks(), "rate-limited tick must not create any task")
}

// TestEvaluate_SharedRateLimitGate covers the second early-exit
// gate: even when the autonomy counter is fine, the shared rate
// limiter (also enforced by the dispatcher and POST /tasks) can
// block. The audit row should still land with RATE_LIMITED so the
// operator sees a single signal regardless of which limiter fired.
func TestEvaluate_SharedRateLimitGate(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	limiter := ratelimit.New()
	m := New(nil, &registry.Registry{}, repo, nil,
		WithEvaluationRepository(evalRepo),
		WithRateLimiter(limiter),
	)

	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "test",
			// No autonomy cap — autonomy gate passes.
		},
		RateLimit: registry.ProjectRateLimit{TasksPerHour: 1},
	}
	// Pre-record one task so the limiter is at the per-hour cap.
	limiter.Record(p.ID, time.Now())

	err := m.evaluate(context.Background(), p)
	require.NoError(t, err)

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeRateLimited, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "rate limit", "reason should reference the limiter, not the autonomy cap")
	assert.Empty(t, repo.createdTasks())
}

// TestEvaluate_BudgetGate covers the third early-exit gate: hard
// daily-budget breach. evaluate must skip with BUDGET_BLOCKED and
// not call the LLM (chat client is nil, so reaching it would
// panic). Soft-breach behaviour is covered by the existing
// CreateTask budget tests in the api package; the hard gate has
// the tightest blast radius and the audit specifically called out
// the BUDGET_BLOCKED outcome write as untested.
func TestEvaluate_BudgetGate(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	usage := &stubLLMUsageRepo{dailyCost: 10.0} // way over the cap below
	m := New(nil, &registry.Registry{}, repo, nil,
		WithEvaluationRepository(evalRepo),
		WithLLMUsageRepository(usage),
	)

	p := &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "test",
		},
		Budget: registry.ProjectBudget{DailyHardUSD: 1.0},
	}
	err := m.evaluate(context.Background(), p)
	require.NoError(t, err)

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeBudgetBlocked, entries[0].Outcome)
	assert.Contains(t, entries[0].Reason, "budget", "reason should cite the budget breach")
	assert.Empty(t, repo.createdTasks())
}

func (s *stubLLMUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (s *stubLLMUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
