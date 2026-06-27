package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestLifecycle_FullHappyPath is the Tier-1 "if it breaks, everything
// breaks" backbone characterization test: a single submitted task is
// driven through the REAL executor (Execute → runExecution →
// executeWorkflowAttempt → executeAgentStep → handleSuccess) with the
// shared executor fakes, and we assert that EVERY persisted artefact of
// a successful run lands together:
//
//   - the task row ends COMPLETED (TaskRepo),
//   - the execution row ends COMPLETED with the agent's result bytes
//     recorded (ExecRepo.RecordCompletion → exec.Result),
//   - the agent's deliverable file becomes an OUTPUT artifact row
//     (ArtifactRepo.Create, via the real persistArtifacts harvest), and
//   - the project-opted-in LLM-as-judge runner fires for the terminal
//     task (judgeRunnerInterface, wired through the harness seam).
//
// Each of these has narrower unit/integration coverage elsewhere
// (execute_workflow_terminal_test pins terminals, step_coverage_
// deception_test pins the COMPLETED status, artifacts_test pins
// persistArtifacts in isolation, fire_judge_test pins the judge
// fire/skip ladder). What was missing — and what this test adds — is a
// single deterministic journey asserting the WHOLE backbone in one
// pass, so a refactor that drops any one of {result persistence,
// artifact harvest, judge fire} from the success path is caught even if
// the per-helper tests stay green.
//
// Deterministic: no sleeps. State transitions are async (Execute spawns
// runExecution in a goroutine), so we gate on the judge runner's `done`
// channel — the judge fires LAST in handleSuccess (after RecordCompletion,
// the task-status flip, and ingestOutputArtifacts), so once it has run
// every earlier persistence write is guaranteed to have happened.
func TestLifecycle_FullHappyPath(t *testing.T) {
	rt := NewMockRuntime()
	// The agent emits a structurally-trivial COMPLETED result.json with
	// a message we can later read back out of the persisted exec.Result,
	// and writes one deliverable into artifacts/out so the harvest path
	// produces an artifact row.
	rt.outputJSON = `{"status":"COMPLETED","message":"backbone ran clean"}`
	rt.artifactFiles = map[string]string{
		"deliverable.md": "# Result\n\nthe backbone produced this file\n",
	}

	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	// Recording artifact repo (in-package stub from artifacts_test.go) so
	// we can assert the OUTPUT row was created — the default
	// MockArtifactRepo.Create is a no-op and records nothing.
	ar := &stubArtifactRepo{}

	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	// Wire the judge through the harness seam: a project that opts in via
	// HallucinationJudge.Enabled, plus a stub runner whose done channel
	// lets us await the async fire deterministically.
	judgeDone := make(chan struct{})
	judge := &stubJudgeRunner{done: judgeDone}
	e.judgeRunner = judge
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID:                 "p1",
				SwarmID:            "s1",
				DefaultWorkflowID:  "wf1",
				HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true},
			},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "run",
				Steps: map[string]registry.WorkflowStep{
					"run": {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	})

	// Submit: a LEASED task, mirroring the scheduler hand-off.
	const taskID = "t-backbone"
	tr.AddTask(&persistence.Task{
		ID:          taskID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"context":{"prompt":"do the backbone work"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(taskID))

	// The judge fires last in handleSuccess. Awaiting it is therefore a
	// happens-after barrier for every earlier persistence write.
	select {
	case <-judgeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("judge runner never fired — backbone did not reach the success tail")
	}

	// 1. Task row: COMPLETED.
	task, err := tr.Get(context.Background(), taskID)
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status,
		"task must finalize COMPLETED on the happy path")

	// 2. Execution row: COMPLETED with the agent's result bytes recorded.
	exec, err := er.GetByTaskID(context.Background(), taskID)
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, persistence.ExecutionStatusCompleted, exec.Status,
		"execution must finalize COMPLETED")
	assert.Equal(t, []string{"run"}, exec.CompletedSteps,
		"the single agent step must be recorded as completed")
	require.NotEmpty(t, exec.Result, "RecordCompletion must persist the agent result bytes")
	var recorded struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(exec.Result, &recorded))
	assert.Equal(t, "backbone ran clean", recorded.Message,
		"the persisted result must be the agent's emitted output, not a stub")

	// 3. Artifacts: the deliverable became an OUTPUT artifact row,
	// attributed to this task + execution.
	require.Len(t, ar.rows, 1, "exactly one OUTPUT artifact must be persisted")
	row := ar.rows[0]
	assert.Equal(t, persistence.ArtifactClassOutput, row.ArtifactClass)
	assert.Equal(t, "p1", row.ProjectID)
	require.NotNil(t, row.TaskID)
	assert.Equal(t, taskID, *row.TaskID)
	require.NotNil(t, row.ExecutionID)
	assert.Equal(t, exec.ID, *row.ExecutionID)
	assert.Contains(t, row.Name, "deliverable",
		"the harvested artifact must carry the agent's deliverable name (disambiguated)")

	// 4. Judge: the runner fired exactly once for the terminal task.
	judge.mu.Lock()
	calls := judge.calls
	judge.mu.Unlock()
	assert.Equal(t, 1, calls, "the LLM-as-judge must fire exactly once for the COMPLETED task")
}
