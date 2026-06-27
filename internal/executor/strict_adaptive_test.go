package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// newGitWorkspaceForProject creates <tmp>/<projectID> as a real git repo (with
// one commit on a `main` branch) and returns <tmp>. Pointing the executor's
// ProjectWorkspacePath at it makes execute() take the real worktree path
// (useWorktrees=true) — the production condition the mock workspace skips.
func newGitWorkspaceForProject(t *testing.T, projectID string) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, projectID)
	require.NoError(t, os.MkdirAll(repo, 0o755))
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644))
	run("add", "-A")
	run("commit", "-m", "seed")
	return root
}

// countingSystemHandler is a fake `system` step handler that records how many
// times it Executed — used to prove the publish step actually dispatches on the
// issue-fix resume path.
type countingSystemHandler struct {
	name  string
	calls atomic.Int32
}

func (h *countingSystemHandler) Name() string { return h.name }
func (h *countingSystemHandler) Execute(_ context.Context, _ SystemStepInput) (SystemStepResult, error) {
	h.calls.Add(1)
	return SystemStepResult{Result: json.RawMessage(`{"cr_url":"https://example/pr/1","state":"opened"}`)}, nil
}

// TestStrictAdaptive_RoutePausesParent — 2026-05-21 inverted post-
// fix. After the route step's lead emits selected_workflow and the
// executor spawns the child, the parent must transition to
// WAITING_FOR_CHILDREN so the dispatcher's "task done" signal
// fires only after the child actually produces output. Pre-fix the
// parent completed immediately on the spawn-then-fallthrough path,
// which left single-shot channels (email, voice) responding on the
// parent's "selected_workflow=research" outcome rather than the
// child's real artifacts.
//
// Unbounded-respawn loop protection (the 20+ czech-news incident
// observed on 2026-05-15) is now handled by the routeAlreadyHandled
// resume guard at the start of the route step — see
// TestStrictAdaptive_ResumeDoesNotRespawn.
//
// Asserts: parent WAITING_FOR_CHILDREN + exactly one child task
// created with the chosen workflow_id.
func TestStrictAdaptive_RoutePausesParent(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"selected_workflow": "research", "reason": "test"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID:                         "p1",
				SwarmID:                    "s1",
				DefaultWorkflowID:          "adaptive",
				AdaptiveCandidateWorkflows: []string{"research", "dev-pipeline"},
			},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{
				Name:    "lead",
				Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"},
			}}},
		},
		workflows: map[string]*registry.Workflow{
			"adaptive": {
				ID:         "adaptive",
				Entrypoint: "route",
				Steps: map[string]registry.WorkflowStep{
					"route": {Type: "agent", Role: "lead", OnSuccess: "delegated", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"delegated": {Status: "COMPLETED"},
					"failed":    {Status: "FAILED"},
				},
			},
			"research": {ID: "research", Entrypoint: "go", Steps: map[string]registry.WorkflowStep{"go": {Type: "agent", Role: "researcher", OnSuccess: "done"}}, Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}}},
		},
	})

	parentID := "t-parent"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"czech-news: refresh"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusWaitingForChildren
	}, 2*time.Second, 10*time.Millisecond,
		"parent must transition to WAITING_FOR_CHILDREN after route spawns child")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "exactly one child should be spawned per route emission")
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "research", *children[0].WorkflowID,
		"child must run the workflow the lead chose")
}

// TestStrictAdaptive_ResumeDoesNotRespawn — pin the resume guard.
// When the parent is WAITING_FOR_CHILDREN and the child completes,
// checkParentUnblock requeues the parent and the scheduler starts
// a FRESH execution at the workflow entrypoint (route). Without
// the guard the lead's LLM would re-run and spawn another child —
// the 20+ czech-news unbounded-respawn loop. The guard detects
// existing children, skips the LLM call, and lets the post-step
// transition advance the parent to OnSuccess.
//
// Test shape: pre-create a child task linked to the parent, run
// the parent's execution (simulating the requeued resume), assert
// the runtime was NEVER invoked and the parent finalized
// COMPLETED.
func TestStrictAdaptive_ResumeDoesNotRespawn(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"selected_workflow":"research","reason":"should not run"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))

	parentID := "t-parent-resume"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"resume me"}}`),
		CreatedAt:   time.Now(),
	})
	// Pre-existing completed child — simulates the state where the
	// parent's prior execution already spawned a child and that
	// child already finished. checkParentUnblock has requeued the
	// parent; the executor is now running the resumed-parent's
	// fresh execution row.
	childWF := "research"
	tr.AddTask(&persistence.Task{
		ID:           "t-child-existing",
		ParentTaskID: &parentID,
		ProjectID:    "p1",
		WorkflowID:   &childWF,
		Status:       persistence.TaskStatusCompleted,
		CreatedAt:    time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond,
		"resumed parent must finalize COMPLETED via OnSuccess transition (no LLM, no spawn)")

	assert.Equal(t, 0, rt.startCalls,
		"runtime must NEVER be invoked on resume — the route step's LLM call must be skipped")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1,
		"no additional child should be spawned on resume (would be the unbounded-loop regression)")
}

// TestResumeAfterChildren_ParentReturnsChildOutcome pins the dispatcher-facing
// behaviour: when a single-child route parent resumes after its child
// completes, the parent's final result must surface the CHILD's outcome (what
// the dispatcher verbalizes), NOT the internal "route already executed; skipping
// LLM + spawn" note.
func TestResumeAfterChildren_ParentReturnsChildOutcome(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"selected_workflow":"research","reason":"should not run"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))

	parentID := "t-parent-outcome"
	tr.AddTask(&persistence.Task{
		ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"taskType":"research","context":{"prompt":"resume me"}}`),
		CreatedAt: time.Now(),
	})
	childWF := "research"
	childID := "t-child-outcome"
	tr.AddTask(&persistence.Task{
		ID: childID, ParentTaskID: &parentID, ProjectID: "p1",
		WorkflowID: &childWF, Status: persistence.TaskStatusCompleted, CreatedAt: time.Now(),
	})
	// The child's real work product — what the dispatcher should ultimately see.
	require.NoError(t, er.Create(context.Background(), &persistence.Execution{
		ID: "ex-child-outcome", TaskID: childID, Status: persistence.ExecutionStatusCompleted,
		Result: []byte(`{"message":"Found 3 kid-friendly venues near Brno."}`),
	}))

	require.NoError(t, e.Execute(parentID))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond, "resumed parent must finalize COMPLETED")

	// The parent has multiple executions (the paused-then-superseded first run
	// + the completed resume run); read the one that actually carries a result
	// rather than relying on map-order GetByTaskID.
	execs, err := er.List(context.Background(), persistence.ExecutionFilter{})
	require.NoError(t, err)
	var pe *persistence.Execution
	for _, ex := range execs {
		if ex.TaskID == parentID && len(ex.Result) > 0 {
			pe = ex
		}
	}
	require.NotNil(t, pe, "parent must have a completed execution carrying a result; execs=%d", len(execs))
	var got struct {
		Message  string `json:"message"`
		Children []struct {
			TaskID  string `json:"task_id"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"children"`
	}
	require.NoError(t, json.Unmarshal(pe.Result, &got))
	assert.Equal(t, "Found 3 kid-friendly venues near Brno.", got.Message,
		"parent must surface the child's outcome, not the synthetic resume note")
	assert.NotContains(t, got.Message, "route already executed")
	require.Len(t, got.Children, 1)
	assert.Equal(t, childID, got.Children[0].TaskID)
	assert.Equal(t, "Found 3 kid-friendly venues near Brno.", got.Children[0].Message)
}

// strictAdaptiveResolver builds a minimal MockWorkflowResolver
// wiring for the route-step end-to-end tests below. Single role
// `lead`, single-step adaptive workflow `adaptive` plus a worker
// `research`. Candidates = candidatesIn so each test can shape the
// list independently.
func strictAdaptiveResolver(candidates []string) *MockWorkflowResolver {
	return &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID:                         "p1",
				SwarmID:                    "s1",
				DefaultWorkflowID:          "adaptive",
				AdaptiveCandidateWorkflows: candidates,
			},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{
				Name:    "lead",
				Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"},
			}}},
		},
		workflows: map[string]*registry.Workflow{
			"adaptive": {
				ID:         "adaptive",
				Entrypoint: "route",
				Steps: map[string]registry.WorkflowStep{
					"route": {Type: "agent", Role: "lead", OnSuccess: "delegated", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"delegated": {Status: "COMPLETED"},
					"failed":    {Status: "FAILED"},
				},
			},
			"research": {ID: "research", Entrypoint: "go", Steps: map[string]registry.WorkflowStep{"go": {Type: "agent", Role: "researcher", OnSuccess: "done"}}, Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}}},
		},
	}
}

// resumeFlagResolver mirrors strictAdaptiveResolver but the intake workflow has
// a custom id ("gh-router") and an opt-in resume_after_children flag — used to
// verify C1 (the resume guard generalizes beyond the built-in "adaptive" id).
func resumeFlagResolver(resume bool, candidates []string) *MockWorkflowResolver {
	r := strictAdaptiveResolver(candidates)
	r.projects["p1"].DefaultWorkflowID = "gh-router"
	r.workflows["gh-router"] = &registry.Workflow{
		ID:                  "gh-router",
		Entrypoint:          "route",
		ResumeAfterChildren: resume,
		Steps: map[string]registry.WorkflowStep{
			"route": {Type: "agent", Role: "lead", OnSuccess: "delegated", OnFail: "failed"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"delegated": {Status: "COMPLETED"},
			"failed":    {Status: "FAILED"},
		},
	}
	return r
}

func addResumeParentWithChild(tr *MockTaskRepo, parentID string) {
	tr.AddTask(&persistence.Task{
		ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"taskType":"x","context":{"prompt":"resume me"}}`),
		CreatedAt: time.Now(),
	})
	childWF := "research"
	tr.AddTask(&persistence.Task{
		ID: "t-child-existing", ParentTaskID: &parentID, ProjectID: "p1",
		WorkflowID: &childWF, Status: persistence.TaskStatusCompleted, CreatedAt: time.Now(),
	})
}

// TestResumeAfterChildren_CustomWorkflowGuard — C1: a custom (non-"adaptive")
// workflow that sets resume_after_children gets the resume guard. On resume
// with an existing child it must skip the route LLM + spawn and advance to
// OnSuccess — no respawn.
func TestResumeAfterChildren_CustomWorkflowGuard(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"selected_workflow":"research","reason":"must NOT run on resume"}`
	er, ar, tr := NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(resumeFlagResolver(true, []string{"research", "summary"}))

	parentID := "t-parent-custom-resume"
	addResumeParentWithChild(tr, parentID)
	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond, "resumed custom-workflow parent must finalize COMPLETED via OnSuccess")
	assert.Equal(t, 0, rt.startCalls, "resume_after_children must skip the route LLM on resume")
	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "no additional child on resume (resume_after_children guard)")
}

// TestResumeAfterChildren_FlagIsLoadBearing — without the flag, the same custom
// workflow does NOT get the guard: on resume it re-runs the route LLM and
// respawns. Proves the flag is what extends the guard.
func TestResumeAfterChildren_FlagIsLoadBearing(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"selected_workflow":"research","reason":"reran because no guard"}`
	er, ar, tr := NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(resumeFlagResolver(false, []string{"research", "summary"}))

	parentID := "t-parent-noflag-resume"
	addResumeParentWithChild(tr, parentID)
	require.NoError(t, e.Execute(parentID))

	// Without the flag the guard doesn't fire, so the route step re-runs its
	// LLM on resume (the precise load-bearing signal; whether a respawn then
	// happens is governed by separate delegation caps).
	assert.Eventually(t, func() bool {
		return rt.StartCalls() > 0 // locked read — runs concurrently with the executor goroutine
	}, 2*time.Second, 10*time.Millisecond, "without resume_after_children the route LLM must run on resume (guard not applied)")
}

// issueFixResolver mirrors the live issue-fix workflow shape that broke
// task_20260613142534_c0ec8045970896cb: a resume_after_children workflow whose
// ENTRYPOINT delegates subtasks and whose SUBSEQUENT step is a real agent step
// (a reviewer) gated on its own output. The bug: the resume guard fired for
// every step in a resume_after_children workflow (bare ResumeAfterChildren
// check), so on resume the `review` agent step was skipped — synthesised no-op,
// reviewer LLM never ran, review.approved unset → FAILED even though every
// child merged. The fix confines the guard to the entrypoint via
// isStrictRouteStep. This resolver mirrors the live issue-fix review step
// exactly: inline gates with NO on_success (on_success would make the gates
// dead — the engine evaluates an agent step's inline gates only when on_success
// is empty), on_fail as the not-approved catch-all.
//
//	decompose (entrypoint, lead, delegates)  --on_success-->  review
//	review (reviewer, gate review.approved)  --gate true-->   done (COMPLETED)
//	                                         --on_fail-->      rejected (FAILED)
func issueFixResolver() *MockWorkflowResolver {
	return &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "issue-fix"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
				{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"issue-fix": {
				ID:                  "issue-fix",
				Entrypoint:          "decompose",
				ResumeAfterChildren: true,
				Steps: map[string]registry.WorkflowStep{
					"decompose": {Type: "agent", Role: "lead", OnSuccess: "review", OnFail: "rejected"},
					"review": {
						Type: "agent", Role: "reviewer",
						Gates: []registry.WorkflowGate{
							{Condition: "review.approved == true", Target: "done"},
							{Condition: "review.approved == false", Target: "rejected"},
						},
						OnFail: "rejected", // catch-all: reviewer emitted no parseable review.approved
					},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":     {Status: "COMPLETED"},
					"rejected": {Status: "FAILED", Message: "Issue fix incomplete or failed review — no PR opened."},
				},
			},
		},
	}
}

// issueFixWithPublishResolver mirrors the FULL live issue-fix shape including
// the terminal `publish` SYSTEM step: decompose (entrypoint, delegates) → review
// (gate approved → publish) → publish (system forge handler, on_success →
// complete) → complete (COMPLETED). Used to prove the system publish step
// actually dispatches on the resume path (the children merged, review approved
// — the PR must then be opened).
func issueFixWithPublishResolver() *MockWorkflowResolver {
	r := issueFixResolver()
	wf := r.workflows["issue-fix"]
	// review's approved gate now targets the publish system step (not a terminal).
	wf.Steps["review"] = registry.WorkflowStep{
		Type: "agent", Role: "reviewer",
		Gates: []registry.WorkflowGate{
			{Condition: "review.approved == true", Target: "publish"},
			{Condition: "review.approved == false", Target: "rejected"},
		},
		OnFail: "rejected",
	}
	wf.Steps["publish"] = registry.WorkflowStep{
		Type: "system", Handler: "test.open_change_request",
		OnSuccess: "done", OnFail: "rejected",
	}
	return r
}

// TestResumeAfterChildren_PublishSystemStepRuns — regression for
// task_20260613142534_c0ec8045970896cb (run 2): the resume guard + gate fixes
// got `review` to run and approve, the gate routed to `publish`, but the task
// COMPLETED without the `publish` SYSTEM step ever dispatching — so no PR was
// opened and the children's merged commits were stranded. This test mirrors the
// full review→publish(system)→complete path on a resumed execution (entrypoint
// already delegated, child COMPLETED) and asserts the publish handler actually
// runs exactly once.
func TestResumeAfterChildren_PublishSystemStepRuns(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"review":{"approved":true,"summary":"ok"}}`
	er, ar, tr := NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo()
	publish := &countingSystemHandler{name: "test.open_change_request"}
	reg := NewSystemHandlerRegistry()
	reg.Register(publish)
	e := NewWithOptions(rt, er, ar, tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0
	// Real git workspace so execute() takes the worktree path (useWorktrees=true)
	// — the production condition under which publish was skipped.
	e.config.ProjectWorkspacePath = newGitWorkspaceForProject(t, "p1")
	e.SetWorkflowResolver(issueFixWithPublishResolver())

	parentID := "t-parent-publish-resume"
	tr.AddTask(&persistence.Task{
		ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"taskType":"x","context":{"prompt":"fix issue #1"}}`),
		CreatedAt: time.Now(),
	})
	childWF := "issue-subtask"
	tr.AddTask(&persistence.Task{
		ID: "t-child-merged", ParentTaskID: &parentID, ProjectID: "p1",
		WorkflowID: &childWF, Status: persistence.TaskStatusCompleted, CreatedAt: time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond,
		"resumed parent must reach COMPLETED via publish→done")

	assert.Equal(t, int32(1), publish.calls.Load(),
		"the publish SYSTEM step must dispatch exactly once after review approves on resume — NOT be skipped (the run-2 bug: task COMPLETED with no PR opened)")
}

// TestResumeAfterChildren_NonEntrypointStepRuns — regression for
// task_20260613142534_c0ec8045970896cb. On resume (entrypoint already
// delegated, child COMPLETED) the resume guard must skip ONLY the entrypoint
// (decompose) and let the subsequent `review` agent step actually run its
// reviewer LLM. Pre-fix the guard fired for `review` too: synthesised no-op,
// reviewer skipped (startCalls==0), gate `approved==true` never matched →
// on_success `rejected` → parent FAILED despite a merged child. Post-fix the
// reviewer runs exactly once and, emitting approved==true, the parent reaches
// the COMPLETED terminal via the gate.
func TestResumeAfterChildren_NonEntrypointStepRuns(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"review":{"approved":true,"summary":"subtasks look good"}}`
	er, ar, tr := NewMockExecRepo(), NewMockArtifactRepo(), NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(issueFixResolver())

	parentID := "t-parent-issuefix-resume"
	tr.AddTask(&persistence.Task{
		ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"taskType":"x","context":{"prompt":"fix issue #1"}}`),
		CreatedAt: time.Now(),
	})
	// Pre-existing COMPLETED child — the decompose entrypoint already delegated
	// in a prior execution and the subtask merged. checkParentUnblock has
	// requeued the parent; this is the resumed-parent execution.
	childWF := "issue-subtask"
	tr.AddTask(&persistence.Task{
		ID: "t-child-merged", ParentTaskID: &parentID, ProjectID: "p1",
		WorkflowID: &childWF, Status: persistence.TaskStatusCompleted, CreatedAt: time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond,
		"resumed parent must reach COMPLETED via the review gate — review step must NOT be skipped by the resume guard")

	assert.Equal(t, 1, rt.StartCalls(),
		"reviewer LLM must run exactly once on resume: entrypoint (decompose) is skipped by the guard, but `review` (non-entrypoint) must execute")
}

// TestStrictAdaptive_CorrectiveRetryOnBadPick (Fix C, 2026-05-15
// incident) — when the lead's first attempt selects a workflow
// outside the candidate list, the executor must re-run the route
// step ONCE with a corrective hint, and on a valid second pick
// spawn exactly one child running that workflow. The two-attempt
// sequence is verified by checking the runtime's start-call count
// equals 2.
func TestStrictAdaptive_CorrectiveRetryOnBadPick(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"selected_workflow": "plan-and-write", "reason": "first attempt: bad pick"}`,
		`{"selected_workflow": "research", "reason": "corrective retry"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	// Two candidates so the route step actually invokes the LLM
	// (single-candidate lists short-circuit the LLM call — see
	// TestStrictAdaptive_SingleCandidateShortCircuits).
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))

	parentID := "t-parent"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"do the thing"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusWaitingForChildren
	}, 2*time.Second, 10*time.Millisecond,
		"parent must pause on WAITING_FOR_CHILDREN after corrective retry produces a valid pick")

	assert.Equal(t, 2, rt.startCalls,
		"runtime must be invoked exactly twice: first attempt (bad pick) + corrective retry")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "exactly one child should be spawned — the retry must not duplicate the spawn")
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "research", *children[0].WorkflowID,
		"child must run the workflow chosen on the corrective retry, not the original bad pick")
}

// TestStrictAdaptive_FailsAfterRetryStillBadPick (Fix C) — when
// both attempts pick a workflow outside the candidate list, the
// parent must FAIL rather than silently fall back to project
// default. This is the regression test for the original 2026-05-15
// incident (14-task runaway chain): the pre-fix behaviour was
// fall-back-then-recurse; the post-fix behaviour is fail-loud-after-
// one-retry.
func TestStrictAdaptive_FailsAfterRetryStillBadPick(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"selected_workflow": "plan-and-write", "reason": "first"}`,
		`{"selected_workflow": "plan-and-write", "reason": "still wrong"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	// Two candidates so the route step actually invokes the LLM
	// (single-candidate lists short-circuit the LLM call — see
	// TestStrictAdaptive_SingleCandidateShortCircuits).
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))

	parentID := "t-parent"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"do the thing"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusFailed
	}, 2*time.Second, 10*time.Millisecond,
		"parent must finalize FAILED when both lead attempts pick out-of-list workflows — NEVER silently fall back to project default")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	assert.Len(t, children, 0,
		"no child task should be spawned when both route attempts produce invalid picks")
}

// TestStrictAdaptive_SingleCandidateShortCircuits — when the project's
// adaptive candidate list contains exactly one entry, the route step
// must auto-route to that workflow WITHOUT invoking the LLM. This
// guards against the 2026-05-18 janka failure mode where a flash-tier
// model fabricated a "config missing — add adaptiveCandidateWorkflows.json"
// refusal when asked to pick from a list of one.
//
// Asserts: zero runtime invocations, parent COMPLETED, exactly one
// child spawned with the only candidate workflow.
func TestStrictAdaptive_SingleCandidateShortCircuits(t *testing.T) {
	rt := NewMockRuntime()
	// Deliberately set a refusal-style output so we can prove the
	// short-circuit fires BEFORE the LLM is reached. If the test
	// regresses to invoking the LLM, this prose will fail downstream
	// parsing and the test will surface a different failure mode.
	rt.outputJSON = `I cannot proceed: please add adaptiveCandidateWorkflows.json to .autonomy/`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research"}))

	parentID := "t-parent"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"do the thing"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusWaitingForChildren
	}, 2*time.Second, 10*time.Millisecond,
		"parent must pause on WAITING_FOR_CHILDREN after single-candidate auto-route")

	assert.Equal(t, 0, rt.startCalls,
		"single-candidate route must NOT invoke the LLM — startCalls should be zero")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "exactly one child should be spawned for the only candidate")
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "research", *children[0].WorkflowID,
		"child must run the only candidate workflow")
}

// TestStrictAdaptive_EmptyPickTriggersRetry (2026-05-18) — when the
// lead's first attempt emits no selected_workflow (typically a prose
// refusal — "the config is missing, please add the JSON file" —
// instead of valid JSON), the executor must run ONE corrective retry
// with an explicit "refusal not allowed" hint. Pre-fix this case fell
// through to legacy free-form planning and surfaced the refusal text
// to the operator as if it were a deliverable.
func TestStrictAdaptive_EmptyPickTriggersRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// First attempt: a refusal with no selected_workflow field.
		// JSON-shaped so the unmarshal succeeds but the field is absent.
		`{"message": "I cannot proceed without adaptiveCandidateWorkflows.json"}`,
		// Corrective retry produces a valid pick.
		`{"selected_workflow": "research", "reason": "second attempt"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	// Two candidates so the short-circuit doesn't fire — we want to
	// prove the LLM IS invoked, refuses, then retries.
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))

	parentID := "t-parent"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"do the thing"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusWaitingForChildren
	}, 2*time.Second, 10*time.Millisecond,
		"parent must pause on WAITING_FOR_CHILDREN after empty-pick retry produces a valid pick")

	assert.Equal(t, 2, rt.startCalls,
		"runtime must be invoked exactly twice: refusal + corrective retry")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "exactly one child should be spawned (retry must not duplicate)")
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "research", *children[0].WorkflowID,
		"child must run the workflow chosen on the corrective retry")
}

// TestStrictAdaptive_EmptyPickFailsAfterRetry — both attempts emit
// empty/refusal results: the parent must FAIL rather than silently
// fall through to legacy free-form planning.
func TestStrictAdaptive_EmptyPickFailsAfterRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"message": "I cannot proceed — config missing"}`,
		`{"message": "Still cannot proceed"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))

	parentID := "t-parent"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"do the thing"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), parentID)
		return task != nil && task.Status == persistence.TaskStatusFailed
	}, 2*time.Second, 10*time.Millisecond,
		"parent must finalize FAILED when both attempts refuse to pick")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	assert.Len(t, children, 0,
		"no child task should be spawned when both route attempts refuse")
}

// TestDelegateSelectedWorkflow_HappyPath — the lead's choice is in
// the candidate list; a child task is created with that workflow_id,
// inheriting the parent's payload + project.
func TestDelegateSelectedWorkflow_HappyPath(t *testing.T) {
	e, _, _, _, tr := setup()
	parent := &persistence.Task{
		ID:          "t-parent",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Priority:    25,
		Payload:     []byte(`{"context":{"prompt":"do the thing"}}`),
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(parent)

	project := &registry.Project{
		ID:                         "p1",
		DefaultWorkflowID:          "fallback-wf",
		AdaptiveCandidateWorkflows: []string{"dev-pipeline", "research", "plan-and-write"},
	}

	got, err := e.delegateSelectedWorkflow(context.Background(), parent, project, "research")
	require.NoError(t, err)
	assert.Equal(t, "research", got, "valid candidate must flow through unchanged")

	// One child task created with the chosen workflow.
	children, err := tr.GetChildren(context.Background(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1, "exactly one child must be spawned for the chosen workflow")
	child := children[0]
	require.NotNil(t, child.WorkflowID)
	assert.Equal(t, "research", *child.WorkflowID)
	assert.Equal(t, parent.ProjectID, child.ProjectID)
	assert.Equal(t, parent.Priority, child.Priority, "child inherits parent priority")
	assert.Equal(t, persistence.TaskStatusQueued, child.Status)
	require.NotNil(t, child.ParentTaskID)
	assert.Equal(t, parent.ID, *child.ParentTaskID)
	assert.Equal(t, persistence.TaskCreationSourceRoute, child.CreationSource)
	assert.Equal(t, parent.Payload, child.Payload, "child must receive the parent's full payload (the original task context)")
}

// TestDelegateSelectedWorkflow_StrictRejectsNonCandidate — lead
// picks something not in the candidate list. Post-2026-05-15
// incident the executor refuses rather than silently falling back
// to project.DefaultWorkflowID. The fallback was the mechanism by
// which the assistant project (defaultWorkflowId=adaptive, candidates=
// [research]) spawned an unbounded chain: lead picked plan-and-write,
// fallback resolved to adaptive, child re-ran the router, picked
// plan-and-write again, ad infinitum. Corrective retry is the
// caller's responsibility (workflow.go); delegateSelectedWorkflow
// itself must reject loudly.
func TestDelegateSelectedWorkflow_StrictRejectsNonCandidate(t *testing.T) {
	e, _, _, _, tr := setup()
	parent := &persistence.Task{
		ID:          "t-parent",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(parent)

	project := &registry.Project{
		ID:                         "p1",
		DefaultWorkflowID:          "fallback-wf",
		AdaptiveCandidateWorkflows: []string{"dev-pipeline"},
	}

	_, err := e.delegateSelectedWorkflow(context.Background(), parent, project, "yolo-pipeline")
	require.Error(t, err, "out-of-list pick must fail rather than silently fall back to project default")
	assert.Contains(t, err.Error(), "not in candidate list", "error must clearly identify the validation failure")

	children, err := tr.GetChildren(context.Background(), parent.ID)
	require.NoError(t, err)
	assert.Len(t, children, 0, "no child should be spawned for a rejected pick")
}

// TestDelegateSelectedWorkflow_SameWorkflowLoopGuard — even when
// the lead's pick is in the candidate list, refuse to spawn a child
// running the parent's own workflow. This catches the misconfiguration
// where the routing workflow itself appears in candidates (intentional
// or accidental), which would cause the child to re-run the router
// and recurse.
func TestDelegateSelectedWorkflow_SameWorkflowLoopGuard(t *testing.T) {
	e, _, _, _, tr := setup()
	wfID := "adaptive"
	parent := &persistence.Task{
		ID:          "t-parent",
		ProjectID:   "p1",
		WorkflowID:  &wfID,
		Status:      persistence.TaskStatusRunning,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(parent)

	project := &registry.Project{
		ID:                         "p1",
		DefaultWorkflowID:          "adaptive",
		AdaptiveCandidateWorkflows: []string{"adaptive", "research"},
	}

	_, err := e.delegateSelectedWorkflow(context.Background(), parent, project, "adaptive")
	require.Error(t, err, "spawning a child on the parent's own workflow must be refused")
	assert.Contains(t, err.Error(), "routing loop")

	children, err := tr.GetChildren(context.Background(), parent.ID)
	require.NoError(t, err)
	assert.Len(t, children, 0)
}

// TestDelegateSelectedWorkflow_DepthCapStopsRunaway — even if Fix A
// (same-workflow guard) and Fix C (corrective retry) are both bypassed
// somehow (e.g. via a chain of distinct workflows that each route back
// to the previous), the depth cap stops the chain at maxRouteDepth.
// Mirrors TestScheduleCheckpointFollowUp_DepthCapStopsRunaway.
func TestDelegateSelectedWorkflow_DepthCapStopsRunaway(t *testing.T) {
	e, _, _, _, tr := setup()

	// Build chain: original USER task → maxRouteDepth ROUTE children
	// already in the repo. The next call (this one) must refuse.
	original := &persistence.Task{ID: "t_root", ProjectID: "p1", CreationSource: persistence.TaskCreationSourceUser}
	tr.AddTask(original)
	previous := original.ID
	for i := 0; i < maxRouteDepth; i++ {
		child := &persistence.Task{
			ID:             "t_route_" + string(rune('a'+i)),
			ProjectID:      "p1",
			CreationSource: persistence.TaskCreationSourceRoute,
			ParentTaskID:   &previous,
		}
		tr.AddTask(child)
		previous = child.ID
	}
	leaf, err := tr.Get(context.Background(), previous)
	require.NoError(t, err)

	project := &registry.Project{
		ID:                         "p1",
		DefaultWorkflowID:          "adaptive",
		AdaptiveCandidateWorkflows: []string{"research"},
	}
	_, err = e.delegateSelectedWorkflow(context.Background(), leaf, project, "research")
	require.Error(t, err, "delegation past the route-depth cap must be refused")
	assert.Contains(t, err.Error(), "route depth")
}

// TestCountRouteDepth_StopsAtNonRouteAncestor — sibling of the
// checkpoint depth test: a USER→DELEGATION→ROUTE chain has route
// depth 0 (the immediate parent is delegation, not route).
func TestCountRouteDepth_StopsAtNonRouteAncestor(t *testing.T) {
	e, _, _, _, tr := setup()
	user := &persistence.Task{ID: "u", CreationSource: persistence.TaskCreationSourceUser}
	tr.AddTask(user)
	delegated := &persistence.Task{ID: "d", CreationSource: persistence.TaskCreationSourceDelegation, ParentTaskID: &user.ID}
	tr.AddTask(delegated)

	depth, err := e.countRouteDepth(context.Background(), delegated)
	require.NoError(t, err)
	assert.Equal(t, 0, depth, "delegation parent does not count toward the route-depth chain")
}

// TestCountRouteDepth_CountsConsecutiveRouteAncestors — a USER → ROUTE
// → ROUTE → ROUTE chain has route depth 2 at the leaf (counts
// consecutive ROUTE *ancestors*, not the leaf itself; matches
// countCheckpointDepth semantics). Guards against off-by-one
// regressions in countRouteDepth.
func TestCountRouteDepth_CountsConsecutiveRouteAncestors(t *testing.T) {
	e, _, _, _, tr := setup()
	user := &persistence.Task{ID: "u", CreationSource: persistence.TaskCreationSourceUser}
	tr.AddTask(user)
	r1 := &persistence.Task{ID: "r1", CreationSource: persistence.TaskCreationSourceRoute, ParentTaskID: &user.ID}
	tr.AddTask(r1)
	r2 := &persistence.Task{ID: "r2", CreationSource: persistence.TaskCreationSourceRoute, ParentTaskID: &r1.ID}
	tr.AddTask(r2)
	r3 := &persistence.Task{ID: "r3", CreationSource: persistence.TaskCreationSourceRoute, ParentTaskID: &r2.ID}
	tr.AddTask(r3)

	depth, err := e.countRouteDepth(context.Background(), r3)
	require.NoError(t, err)
	assert.Equal(t, 2, depth, "leaf r3 has two consecutive ROUTE ancestors r2 and r1 (the USER root stops the walk)")
}

// TestCountRouteDepth_RepoErrorPropagates — a DB error on the
// parent-walk surfaces to the caller. delegateSelectedWorkflow's
// best-effort branch (which logs and continues) depends on this
// exact return contract.
func TestCountRouteDepth_RepoErrorPropagates(t *testing.T) {
	e, _, _, _, tr := setup()
	parentID := "u"
	tr.AddTask(&persistence.Task{ID: parentID, CreationSource: persistence.TaskCreationSourceUser})
	tr.err = errors.New("db down")
	child := &persistence.Task{ID: "c", CreationSource: persistence.TaskCreationSourceRoute, ParentTaskID: &parentID}

	_, err := e.countRouteDepth(context.Background(), child)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}

// TestDelegateSelectedWorkflow_ProceedsOnDepthWalkError — the
// best-effort branch: when the depth walk fails (DB error reading
// ancestor), delegation must STILL succeed for legitimate first-level
// routes. The next failure's check still bounds the chain. Without
// this fallback, a transient DB hiccup on the ancestor read would
// block routing entirely.
func TestDelegateSelectedWorkflow_ProceedsOnDepthWalkError(t *testing.T) {
	e, _, _, _, tr := setup()
	parentID := "u"
	tr.AddTask(&persistence.Task{ID: parentID, CreationSource: persistence.TaskCreationSourceUser})
	parent := &persistence.Task{
		ID:           "t-parent",
		ProjectID:    "p1",
		ParentTaskID: &parentID, // triggers a Get during depth walk
		MaxAttempts:  3,
		CreatedAt:    time.Now(),
	}
	tr.AddTask(parent)
	project := &registry.Project{
		ID:                         "p1",
		DefaultWorkflowID:          "research",
		AdaptiveCandidateWorkflows: []string{"research"},
	}
	tr.err = errors.New("db hiccup") // Get fails, Create still works

	got, err := e.delegateSelectedWorkflow(context.Background(), parent, project, "research")
	require.NoError(t, err, "transient depth-walk failure must not block first-level routing")
	assert.Equal(t, "research", got)

	tr.err = nil // re-enable reads for the children assertion
	children, err := tr.GetChildren(context.Background(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1, "child must still be created when depth walk degrades to best-effort")
}

// TestBuildRouteCorrectiveHint_MentionsCandidatesAndBadPick pins
// the corrective hint's shape so a future renaming doesn't silently
// drop the candidate list or the bad value from the retry prompt —
// either omission turns the retry into a coin flip.
func TestBuildRouteCorrectiveHint_MentionsCandidatesAndBadPick(t *testing.T) {
	hint := buildRouteCorrectiveHint("plan-and-write", []string{"research", "summary"})
	assert.Contains(t, hint, "plan-and-write", "hint must echo the bad pick so the lead understands what was rejected")
	assert.Contains(t, hint, "research")
	assert.Contains(t, hint, "summary")
	assert.Contains(t, hint, "selected_workflow", "hint must restate the expected output shape")
}

// TestBuildRouteCorrectiveHint_EmptyPickAddressesRefusal — when the
// first attempt produced no selected_workflow (refusal), the hint
// must explicitly tell the model that refusal is not allowed and
// that the candidate list IS the configuration. Without this, the
// retry frequently produces the same prose refusal in a JSON
// wrapper.
func TestBuildRouteCorrectiveHint_EmptyPickAddressesRefusal(t *testing.T) {
	hint := buildRouteCorrectiveHint("", []string{"research", "summary"})
	assert.Contains(t, hint, "Refusal is not allowed", "hint must forbid refusal explicitly")
	assert.Contains(t, hint, "candidate list IS the configuration", "hint must rebut the 'config missing' refusal class")
	assert.Contains(t, hint, "research")
	assert.Contains(t, hint, "summary")
	assert.Contains(t, hint, "selected_workflow", "hint must restate the expected output shape")
}

// TestDelegateSelectedWorkflow_ChildInheritsRetryBudget — the
// child's MaxAttempts mirrors the parent so a retried adaptive
// task doesn't reset its budget on each routing pass.
func TestDelegateSelectedWorkflow_ChildInheritsRetryBudget(t *testing.T) {
	e, _, _, _, tr := setup()
	parent := &persistence.Task{
		ID:          "t-parent",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		MaxAttempts: 5,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(parent)

	project := &registry.Project{
		ID:                         "p1",
		DefaultWorkflowID:          "fb",
		AdaptiveCandidateWorkflows: []string{"dev-pipeline"},
	}

	_, err := e.delegateSelectedWorkflow(context.Background(), parent, project, "dev-pipeline")
	require.NoError(t, err)

	children, err := tr.GetChildren(context.Background(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, parent.MaxAttempts, children[0].MaxAttempts,
		"child must inherit parent's MaxAttempts so retry budget tracks the original task, not reset per route")
}
