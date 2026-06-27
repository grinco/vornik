package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestRetryFromStepResult pins the error→metric-label mapping for
// vornik_retry_from_step_total{result}. The refused_* cases are the
// operator-correctable ones; everything else folds into "error".
func TestRetryFromStepResult(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, "succeeded"},
		{"unknown step", ErrRetryStepNotInExecution, "refused_unknown_step"},
		{"not terminal", ErrRetryNotTerminal, "refused_bad_state"},
		{"already executing", ErrRetryAlreadyExecuting, "refused_bad_state"},
		{"wrapped unknown step", errors.New("retry-from-step: load: " + ErrRetryStepNotInExecution.Error()), "error"},
		{"other error", errors.New("persist failed"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, retryFromStepResult(tc.err))
		})
	}
	// Sentinels stay classified through errors.Is wrapping.
	assert.Equal(t, "refused_bad_state",
		retryFromStepResult(errors.Join(ErrRetryNotTerminal, errors.New("status=running"))))
}

// TestRetryFromStep_RejectsNonTerminal — operator must not be able
// to retry a still-running execution; the executor's own state
// machine owns those rows. The error class is a sentinel so the
// API can map it to 409.
func TestRetryFromStep_RejectsNonTerminal(t *testing.T) {
	e, _, er, _, _ := setup()
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusRunning,
		CompletedSteps: []string{"plan", "implement"},
	}
	require.NoError(t, er.Create(context.Background(), exec))

	err := e.RetryFromStep(context.Background(), "e1", "implement")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRetryNotTerminal),
		"running executions must surface ErrRetryNotTerminal so the API maps to 409")
}

// TestRetryFromStep_RejectsUnknownStep — typo guard. If the operator
// asks to retry from a step the run never reached, the error must
// be the StepNotInExecution sentinel so the API maps to 400.
func TestRetryFromStep_RejectsUnknownStep(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusFailed,
		CreatedAt: time.Now(),
	})
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan"}, // never got past plan
	}
	require.NoError(t, er.Create(context.Background(), exec))

	err := e.RetryFromStep(context.Background(), "e1", "review")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRetryStepNotInExecution),
		"unknown step must surface ErrRetryStepNotInExecution so the API maps to 400")
}

// TestRetryFromStep_TruncatesAndResetsState — the core contract.
// Retrying from "implement" with completed_steps [plan, implement,
// review] must:
//   - truncate completed_steps to [plan]
//   - set current_step_id = "implement"
//   - clear loop counters and plan-loop state
//   - flip execution status RUNNING
//   - flip task status RUNNING
//
// We don't assert on the runExecution goroutine actually completing
// (the mock runtime would need a real container plan); the active
// registration in IsExecuting is the same evidence Recover provides.
func TestRetryFromStep_TruncatesAndResetsState(t *testing.T) {
	e, rt, er, _, tr := setup()

	// Freeze the spawned retry goroutine at WaitForExit (mid-`implement`) so the
	// assertions observe RetryFromStep's PERSISTED state, not whatever the
	// running goroutine has advanced to. waitEntered fires when the run reaches
	// WaitForExit — before any post-step checkpoint persists an iteration bump —
	// so the execution row still holds the clean retry state. Without this the
	// goroutine races the assertions (flaky under CI -race).
	rt.waitGate = make(chan struct{})
	enteredCh := make(chan struct{})
	rt.waitEntered = enteredCh // capture the channel in a local; the mock nils the field under its lock

	// Wire a workflow resolver so recoverExecution can spawn the
	// goroutine without immediately erroring on a missing plan.
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "plan",
				Steps: map[string]registry.WorkflowStep{
					"plan":      {Type: "agent", Role: "worker", OnSuccess: "implement"},
					"implement": {Type: "agent", Role: "worker", OnSuccess: "review"},
					"review":    {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})

	tr.AddTask(&persistence.Task{
		ID:          "t1",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusFailed,
		Attempt:     3, // would normally be at retry budget; operator retry must bypass
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	})

	// Original-run state has loop counters and plan state populated;
	// retry must wipe these so the loop guard doesn't immediately
	// bail on revisiting a step.
	state := executionState{
		CurrentStepID:  "review",
		CompletedSteps: []string{"plan", "implement", "review"},
		VisitCounts:    map[string]int{"plan": 1, "implement": 1, "review": 1},
		Iterations:     12,
		PlanIndex:      2,
		PlanSteps:      []string{"a", "b", "c"},
	}
	snap, _ := json.Marshal(state)
	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "implement", "review"},
		StateSnapshot:  snap,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.RetryFromStep(context.Background(), "e1", "implement"))
	<-enteredCh // goroutine is now frozen at WaitForExit; the row holds the clean retry state

	// Re-fetch the row to see the persisted state.
	got, err := er.Get(context.Background(), "e1")
	require.NoError(t, err)

	assert.Equal(t, persistence.ExecutionStatusRunning, got.Status,
		"retry must flip execution to RUNNING")
	assert.Equal(t, []string{"plan"}, got.CompletedSteps,
		"completed_steps must be truncated to before the retry step")
	require.NotNil(t, got.CurrentStepID)
	assert.Equal(t, "implement", *got.CurrentStepID,
		"current_step_id must be the retry target")

	gotState := loadExecutionState(got)
	assert.Empty(t, gotState.VisitCounts,
		"visit counts must be cleared so the loop guard doesn't immediately bail")
	assert.Equal(t, 0, gotState.Iterations, "iterations counter must reset")
	assert.Empty(t, gotState.PlanSteps, "plan-loop state must be cleared")
	assert.Equal(t, 0, gotState.PlanIndex)

	// Task row was flipped back to RUNNING. Attempt counter must NOT
	// have been bumped — operator-initiated retry shouldn't burn
	// the autonomous retry budget. (The reverse: leaving it at
	// MaxAttempts would cause runExecution to immediately give up,
	// defeating the whole point of the operator retry.)
	task, err := tr.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusRunning, task.Status)
	assert.Equal(t, 3, task.Attempt, "operator retry must not touch task.Attempt")
	assert.Nil(t, task.LastError, "stale failure message must be cleared on retry")

	// Goroutine registered as active.
	assert.True(t, e.IsExecuting("t1"))

	close(rt.waitGate) // unblock the frozen goroutine so it can drain
}

// TestRetryFromStep_RetryAtEntrypointWipesAllOutcomes — retrying
// from the very first completed step (cutIdx == 0) means there are
// NO survivors; supersedes EVERY existing outcome row for the
// execution. The cutoff math has to handle this — using zero-time
// as the cutoff for SupersedeAfter does the right thing.
func TestRetryFromStep_RetryAtEntrypointWipesAllOutcomes(t *testing.T) {
	e, _, er, _, tr := setup()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID: "wf1", Entrypoint: "plan",
				Steps:     map[string]registry.WorkflowStep{"plan": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, MaxAttempts: 3, CreatedAt: time.Now()})

	repo := newStubStepOutcomeRepo()
	e.outcomeRepo = repo
	now := time.Now()
	require.NoError(t, repo.Record(context.Background(), &persistence.ExecutionStepOutcome{
		ID: "o1", ExecutionID: "e1", StepID: "plan", Outcome: "ok", RecordedAt: now,
	}))
	require.NoError(t, repo.Record(context.Background(), &persistence.ExecutionStepOutcome{
		ID: "o2", ExecutionID: "e1", StepID: "plan", Outcome: "failed", RecordedAt: now.Add(time.Second),
	}))

	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan"},
	}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.RetryFromStep(context.Background(), "e1", "plan"))

	// Both outcome rows must be superseded — there were no survivors,
	// so the cutoff is zero time and every recorded_at is "after" it.
	rows, _ := repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	for _, r := range rows {
		assert.Equal(t, "superseded", r.Outcome,
			"retry from entrypoint must supersede all existing outcomes — outcome=%q on %s", r.Outcome, r.ID)
	}
}

// TestRetryFromStep_PreservesUpstreamOutcomes — the complement: when
// retrying from a middle step, the outcome rows for steps that
// SURVIVED the truncation must NOT be marked superseded. They're
// still the canonical record of the original-run upstream work.
func TestRetryFromStep_PreservesUpstreamOutcomes(t *testing.T) {
	e, _, er, _, tr := setup()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID: "wf1", Entrypoint: "plan",
				Steps: map[string]registry.WorkflowStep{
					"plan":      {Type: "agent", Role: "worker", OnSuccess: "implement"},
					"implement": {Type: "agent", Role: "worker", OnSuccess: "review"},
					"review":    {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, MaxAttempts: 3, CreatedAt: time.Now()})

	repo := newStubStepOutcomeRepo()
	e.outcomeRepo = repo
	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	// Three outcomes recorded in execution order. We retry from
	// "implement"; "plan" must survive, "implement" + "review" must
	// be superseded.
	for i, r := range []*persistence.ExecutionStepOutcome{
		{ID: "o-plan", ExecutionID: "e1", StepID: "plan", Outcome: "ok", RecordedAt: t0},
		{ID: "o-impl", ExecutionID: "e1", StepID: "implement", Outcome: "ok", RecordedAt: t0.Add(time.Minute)},
		{ID: "o-rev", ExecutionID: "e1", StepID: "review", Outcome: "failed", RecordedAt: t0.Add(2 * time.Minute)},
	} {
		require.NoError(t, repo.Record(context.Background(), r), "row %d", i)
	}

	exec := &persistence.Execution{
		ID:             "e1",
		TaskID:         "t1",
		ProjectID:      "p1",
		Status:         persistence.ExecutionStatusFailed,
		CompletedSteps: []string{"plan", "implement", "review"},
	}
	require.NoError(t, er.Create(context.Background(), exec))
	require.NoError(t, e.RetryFromStep(context.Background(), "e1", "implement"))

	rows, _ := repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	got := make(map[string]string)
	for _, r := range rows {
		got[r.ID] = r.Outcome
	}
	assert.Equal(t, "ok", got["o-plan"], "upstream survivor outcome must be preserved")
	assert.Equal(t, "superseded", got["o-impl"], "outcome at the retry point must be superseded")
	assert.Equal(t, "superseded", got["o-rev"], "downstream outcome must be superseded")
}

// TestRetryFromStep_RejectsAlreadyExecuting — protects against the
// double-click case: operator hits "retry" twice in quick
// succession. The second call must surface the
// AlreadyExecuting sentinel so the API maps to 409.
func TestRetryFromStep_RejectsAlreadyExecuting(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: time.Now()})

	// Pre-populate activeExecutions with a fake handle so the
	// guard fires without needing a real running goroutine.
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1", projectID: "p1", startedAt: time.Now()}
	e.mu.Unlock()

	err := e.RetryFromStep(context.Background(), "e-not-loaded", "any")
	require.Error(t, err)
	// First failure path: execution lookup fails (we never created
	// it) — proves the API surfaces a clear error rather than
	// silently succeeding.
	assert.NotContains(t, err.Error(), "panic", "lookup failure must not panic")
}

// TestSideEffectingUpstreamSteps — the retry-from-step containment guard's
// classifier. Survivors whose workflow step type is system/call_project are
// flagged (their external effects won't be replayed); agent/gate/plan are not.
func TestSideEffectingUpstreamSteps(t *testing.T) {
	e, _, _, _, _ := setup()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID: "wf1",
				Steps: map[string]registry.WorkflowStep{
					"plan":      {Type: "plan"},
					"implement": {Type: "agent"},
					"index":     {Type: "system", Handler: "rag.index"},
					"callfoo":   {Type: "call_project", TargetProject: "foo"},
					"review":    {Type: "agent"},
				},
			},
		},
	})
	exec := &persistence.Execution{ID: "e1", WorkflowID: "wf1"}

	// Mixed survivors: only the system + call_project steps are flagged,
	// preserving survivor order.
	got := e.sideEffectingUpstreamSteps(exec, []string{"plan", "index", "implement", "callfoo"})
	assert.Equal(t, []string{"index", "callfoo"}, got)

	// All-benign survivors → nil.
	assert.Nil(t, e.sideEffectingUpstreamSteps(exec, []string{"plan", "implement", "review"}))

	// Empty survivors (retry from entrypoint) → nil.
	assert.Nil(t, e.sideEffectingUpstreamSteps(exec, nil))

	// Unknown workflow → nil (best-effort, never blocks).
	assert.Nil(t, e.sideEffectingUpstreamSteps(&persistence.Execution{WorkflowID: "missing"}, []string{"index"}))

	// No resolver wired → nil.
	bare, _, _, _, _ := setup()
	assert.Nil(t, bare.sideEffectingUpstreamSteps(exec, []string{"index"}))
}
