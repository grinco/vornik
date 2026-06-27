package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/verifier"
)

// --- classifyLeadParseError: cover every taxonomy branch ---

func TestPlanStepCov_ClassifyLeadParseError(t *testing.T) {
	cases := []struct {
		name      string
		errMsg    string
		isNil     bool
		wantClass string
	}{
		{name: "nil is ok", isNil: true, wantClass: ""},
		{name: "invalid json", errMsg: "invalid JSON from lead agent: x", wantClass: stepoutcomeClassInvalidJSON()},
		{name: "refused", errMsg: "lead agent refused to plan: out of scope", wantClass: stepoutcomeClassPlanRefused()},
		{name: "no steps", errMsg: "lead agent plan contains no steps", wantClass: stepoutcomeClassPlanNoSteps()},
		{name: "empty result", errMsg: "empty result from lead agent", wantClass: stepoutcomeClassPlanNoSteps()},
		{name: "unmatched falls back to invalid json", errMsg: "some other thing", wantClass: stepoutcomeClassInvalidJSON()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if !tc.isNil {
				err = assertErrf(tc.errMsg)
			}
			_, class := classifyLeadParseError(err)
			assert.Equal(t, tc.wantClass, class)
		})
	}
}

// stepoutcomeClass* indirections keep the test's expectations pinned to the
// production constants without re-importing stepoutcome in every assertion.
func stepoutcomeClassInvalidJSON() string {
	_, c := classifyLeadParseError(assertErrf("invalid JSON from lead agent: x"))
	return c
}
func stepoutcomeClassPlanRefused() string {
	_, c := classifyLeadParseError(assertErrf("lead agent refused to plan: y"))
	return c
}
func stepoutcomeClassPlanNoSteps() string {
	_, c := classifyLeadParseError(assertErrf("lead agent plan contains no steps"))
	return c
}

// --- extractPlanFromText: the no-JSON and embedded-no-steps branches ---

func TestPlanStepCov_ExtractPlanFromText(t *testing.T) {
	t.Run("no json object", func(t *testing.T) {
		_, _, err := extractPlanFromText("just prose, no braces")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no JSON object in text")
	})
	t.Run("end before start", func(t *testing.T) {
		// A "}" appearing before any "{" — LastIndex(}) <= Index({).
		_, _, err := extractPlanFromText("} then {")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no JSON object in text")
	})
	t.Run("embedded json with no steps", func(t *testing.T) {
		_, _, err := extractPlanFromText(`prefix {"plan":{"steps":[]}} suffix`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "embedded plan has no steps")
	})
	t.Run("embedded malformed json", func(t *testing.T) {
		_, _, err := extractPlanFromText(`pre {"plan": {oops}} post`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "embedded JSON parse failed")
	})
	t.Run("happy embedded", func(t *testing.T) {
		steps, msg, err := extractPlanFromText(`Here: {"plan":{"steps":["scout"]},"message":"go"}`)
		require.NoError(t, err)
		assert.Equal(t, []string{"scout"}, steps)
		assert.Equal(t, "go", msg)
	})
}

// --- missingDeclaredOutputs: guards + the missing-vs-present partition ---

func TestPlanStepCov_MissingDeclaredOutputs(t *testing.T) {
	e := &Executor{}
	t.Run("empty inputs short-circuit", func(t *testing.T) {
		assert.Nil(t, e.missingDeclaredOutputs(nil, "/tmp"))
		assert.Nil(t, e.missingDeclaredOutputs([]byte(`{}`), ""))
	})
	t.Run("non-json result", func(t *testing.T) {
		assert.Nil(t, e.missingDeclaredOutputs([]byte("not json"), "/tmp"))
	})
	t.Run("no outputArtifacts declared", func(t *testing.T) {
		assert.Nil(t, e.missingDeclaredOutputs([]byte(`{"status":"ok"}`), "/tmp"))
	})
	t.Run("blank path entry is skipped", func(t *testing.T) {
		got := e.missingDeclaredOutputs([]byte(`{"outputArtifacts":[{"name":"x","path":""}]}`), t.TempDir())
		assert.Nil(t, got)
	})
	t.Run("missing file is reported", func(t *testing.T) {
		dir := t.TempDir()
		got := e.missingDeclaredOutputs([]byte(`{"outputArtifacts":[{"name":"x","path":"absent.md"}]}`), dir)
		require.Len(t, got, 1)
		assert.Equal(t, "absent.md", got[0])
	})
	t.Run("present file is not reported", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, writeFileCov(dir, "present.md", "hi"))
		got := e.missingDeclaredOutputs([]byte(`{"outputArtifacts":[{"name":"x","path":"present.md"}]}`), dir)
		assert.Nil(t, got)
	})
	t.Run("unresolvable path (mount point) is reported missing", func(t *testing.T) {
		dir := t.TempDir()
		// resolveClaimedPath returns "" for the bare mount point, so the
		// declared output is flagged missing.
		got := e.missingDeclaredOutputs([]byte(`{"outputArtifacts":[{"name":"x","path":"/app/workspace"}]}`), dir)
		require.Len(t, got, 1)
		assert.Equal(t, "/app/workspace", got[0])
	})
}

func writeFileCov(dir, name, body string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

// --- recoveryContractCorrectiveHint + recoveryModeResponseSchema (pure) ---

func TestPlanStepCov_RecoveryContractCorrectiveHint(t *testing.T) {
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "verifier_block"}
	got := recoveryContractCorrectiveHint(rc, "continue")
	assert.Contains(t, got, "outcome=continue")
	assert.Contains(t, got, "research")
	assert.Contains(t, got, "verifier_block")
	assert.Contains(t, got, "checkpoint")
}

func TestPlanStepCov_RecoveryModeResponseSchema(t *testing.T) {
	schema := recoveryModeResponseSchema()
	require.Equal(t, "object", schema["type"])
	required, _ := schema["required"].([]string)
	assert.Equal(t, []string{"outcome"}, required)
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	outcome, ok := props["outcome"].(map[string]any)
	require.True(t, ok)
	enum, _ := outcome["enum"].([]string)
	// `continue` must be structurally absent from the enum.
	assert.NotContains(t, enum, string(LeadOutcomeContinue))
	assert.Contains(t, enum, string(LeadOutcomeCheckpoint))
	assert.Contains(t, enum, string(LeadOutcomeExternalWait))
	assert.Contains(t, enum, string(LeadOutcomeClosureRequest))
}

// --- learnedRemediationsBlock: directive + advisory tiers ---

func TestPlanStepCov_LearnedRemediationsBlock(t *testing.T) {
	assert.Equal(t, "", learnedRemediationsBlock(nil))

	rems := []playbook.LearnedRemediation{
		{Action: "retry resolved it", Confidence: 0.9, SupportCount: 9, ContradictCount: 1, AutoApplied: true},
		{Action: "switch model resolved it", Confidence: 0.6, SupportCount: 3, ContradictCount: 2},
	}
	got := learnedRemediationsBlock(rems)
	assert.Contains(t, got, "apply_these_proven_remediations")
	assert.Contains(t, got, "retry resolved it")
	assert.Contains(t, got, "similar_failures_previously_resolved_here")
	assert.Contains(t, got, "switch model resolved it")
}

// --- buildPlanningPromptWithContext: recovery banner + conversational ctx ---

func TestPlanStepCov_BuildPlanningPrompt_RecoveryBanner(t *testing.T) {
	rc := &RecoveryContext{
		FailedStep:    "research",
		FailureClass:  "verifier_block",
		FailureReason: "reuters http_403",
		BlockedURLs:   []verifier.BlockedURL{{URL: "https://reuters.com", Reason: "auth_required"}},
		LearnedRemediations: []playbook.LearnedRemediation{
			{Action: "use cached source", Confidence: 0.7, SupportCount: 4, ContradictCount: 1},
		},
	}
	sw := &registry.Swarm{Roles: []registry.SwarmRole{{Name: "scout", Description: "explores"}, {Name: "writer"}}}
	got := buildPlanningPromptWithContext("PLAN THIS", sw, nil, nil, rc)
	assert.Contains(t, got, "RECOVERY MODE")
	assert.Contains(t, got, "research")
	assert.Contains(t, got, "verifier_block")
	assert.Contains(t, got, "reuters")
	assert.Contains(t, got, "use cached source")
	// Recovery menu prunes the continue outcome.
	assert.Contains(t, got, "continue\" outcome from the normal menu is NOT available")
	// Role catalog rendered; the writer role with no description gets the placeholder.
	assert.Contains(t, got, "scout: explores")
	assert.Contains(t, got, "writer: (no description)")
	assert.Contains(t, got, "PLAN THIS")
}

func TestPlanStepCov_BuildPlanningPrompt_ConversationalContext(t *testing.T) {
	phase := "phase-2"
	sp := &persistence.TaskScratchpad{
		Summary:       "running summary",
		CurrentPhase:  &phase,
		OpenQuestions: []byte(`["q1?"]`),
	}
	now := time.Now()
	msgs := []*persistence.TaskMessage{
		{ID: "m1", AuthorKind: persistence.TaskMessageAuthorOperator, MessageKind: persistence.TaskMessageKindDirective, Content: "do X", CreatedAt: now},
		// A note summarizing m1 — m1 should be hidden from the rendered thread.
		{ID: "m2", AuthorKind: persistence.TaskMessageAuthorLead, MessageKind: persistence.TaskMessageKindNote, Content: "summary note", CreatedAt: now, Metadata: []byte(`{"summarized_message_ids":["m1"]}`)},
	}
	got := buildPlanningPromptWithContext("", nil, sp, msgs, nil)
	assert.Contains(t, got, "running summary")
	assert.Contains(t, got, "phase-2")
	assert.Contains(t, got, "Conversation thread")
	// The summarized original (m1 "do X") is compressed out; the summary note stays.
	assert.Contains(t, got, "summary note")
	assert.Contains(t, got, "compressed by summarize_thread")
	// Normal (non-recovery) menu offers FOUR outcomes incl. continue.
	assert.Contains(t, got, "FOUR outcome shapes")
}

// --- runLeadPlanning via executePlanStep: recovery-contract violation +
// corrective-hint retry. The lead emits continue+plan in recovery mode
// (forbidden), and the corrective-hint retry ALSO emits continue, so the
// step fails terminally with the contract-violation error. This drives
// runLeadPlanning and retryRecoveryContractViolation (both 0% baseline)
// without any real container — the MockRuntime serves the scripted plan
// JSON. ---

func TestPlanStepCov_RecoveryContractViolation_RetryAlsoFails(t *testing.T) {
	rt := NewMockRuntime()
	// Both the first lead attempt and the corrective-hint retry emit a
	// continue+plan envelope, which is forbidden in recovery mode.
	rt.outputJSON = `{"outcome":"continue","plan":{"steps":["researcher"]},"message":"replanning"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-rcv", ProjectID: "p", CreatedAt: time.Now(), Payload: []byte(`{"context":{"prompt":"go"}}`)}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-rcv", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{
		PendingRecovery: &RecoveryContext{FailedStep: "research", FailureClass: "verifier_block"},
	}

	_, _, _, _, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"recover", step, time.Minute, state, nil, nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recovery contract violation")
	// PendingRecovery was consumed (cleared) on the way into the lead.
	assert.Nil(t, state.PendingRecovery, "recovery context must be cleared before invoking the lead")
	// The lead ran twice: the first attempt + the corrective-hint retry.
	assert.GreaterOrEqual(t, rt.StartCalls(), 2, "lead must be re-run once with a corrective hint")
}

// TestPlanStepCov_RunLeadPlanning_HappyContinuePlan drives the non-recovery
// happy path through runLeadPlanning: the lead emits a continue+plan, the
// children run, and the plan completes. Exercises the complexity-tier
// capture branch too.
func TestPlanStepCov_RunLeadPlanning_HappyContinuePlan(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// Lead's planning output: a continue+plan with a complexity verdict.
		`{"outcome":"continue","plan":{"steps":["researcher"]},"message":"do research","complexity":"complex"}`,
		// The researcher child's result.
		`{"status":"COMPLETED","message":"found things"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-happy-lead", ProjectID: "p", CreatedAt: time.Now(), Payload: []byte(`{"context":{"prompt":"go"}}`)}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-happy-lead", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "final"}
	state := &executionState{}

	_, _, nextStep, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan", step, time.Minute, state, nil, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "final", nextStep)
	// The lead's plan was persisted and the researcher ran.
	assert.Equal(t, []string{"researcher"}, state.PlanSteps)
	require.Len(t, completedSteps, 1)
	assert.Contains(t, completedSteps[0], "researcher")
	// Complexity tier captured from the lead's verdict.
	assert.Equal(t, "complex", state.ComplexityTier)
}

// TestPlanStepCov_RunLeadPlanning_UnknownRolesFail covers the
// "lead plan references only unknown roles" terminal branch: the lead's
// plan names roles that are neither canonical nor aliased, so resolvePlanRoles
// leaves valid empty and executePlanStep fails.
func TestPlanStepCov_RunLeadPlanning_UnknownRolesFail(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"outcome":"continue","plan":{"steps":["nonexistent_role"]},"message":"plan"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-unknown", ProjectID: "p", CreatedAt: time.Now(), Payload: []byte(`{"context":{"prompt":"go"}}`)}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-unknown", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			{Name: "writer", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{}

	_, _, _, _, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan", step, time.Minute, state, nil, nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only unknown roles")
}

// isPlanRefusal already has shape coverage elsewhere; pin the negative
// branch here for completeness without duplicating the positive one.
func TestPlanStepCov_IsPlanRefusal_NilAndNonMatch(t *testing.T) {
	assert.False(t, isPlanRefusal(nil))
	assert.False(t, isPlanRefusal(assertErrf("some other error")))
	assert.True(t, isPlanRefusal(assertErrf("lead agent refused to plan: x")))
}

// roleNames is a trivial helper but the error-path that calls it is rarely
// hit; pin it directly.
func TestPlanStepCov_RoleNames(t *testing.T) {
	got := roleNames([]registry.SwarmRole{{Name: "a"}, {Name: "b"}})
	assert.Equal(t, []string{"a", "b"}, got)
	assert.Empty(t, roleNames(nil))
}

// planStepCov_conversationalExecutor builds an Executor with the
// conversational lifecycle wired (fake message/scratchpad/task repos) so the
// lead-handoff path in runLeadPlanning activates. Reuses the handoff-test
// fakes (fakeMessageRepo etc.) defined in lead_handoff_test.go.
func planStepCov_conversationalExecutor(rt *MockRuntime, er *MockExecRepo, ar *MockArtifactRepo, tr *MockTaskRepo) *Executor {
	msgs := &fakeMessageRepo{}
	sp := &fakeScratchpadRepo{}
	// persistTaskRepo needs the broad TaskRepository; the MockTaskRepo
	// satisfies it. Use a fresh fakeTaskRepo so transitions succeed.
	pt := newFakeTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil, WithConversationalLifecycle(msgs, sp, pt))
	e.config.RetryDelay = 0
	return e
}

// TestPlanStepCov_RunLeadPlanning_HandoffCheckpoint — the lead emits a
// checkpoint outcome (non-continue). With the conversational lifecycle wired,
// runLeadPlanning writes the task_message, flips the task, and returns
// errLeadHandoff. executePlanStep propagates the sentinel.
func TestPlanStepCov_RunLeadPlanning_HandoffCheckpoint(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"which way?","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]},"message":"need a decision"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := planStepCov_conversationalExecutor(rt, er, ar, tr)

	task := &persistence.Task{ID: "t-ckpt", ProjectID: "p", Status: persistence.TaskStatusRunning, CreatedAt: time.Now(), Payload: []byte(`{"context":{"prompt":"go"}}`)}
	tr.AddTask(task)
	// The persistTaskRepo must know the task so its TransitionConditional hits.
	exec := &persistence.Execution{ID: "x-ckpt", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{}

	_, _, _, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan", step, time.Minute, state, []string{"prior"}, nil,
	)
	require.Error(t, err)
	assert.True(t, IsLeadHandoff(err), "lead checkpoint must surface as the handoff sentinel")
	// The lead's synthetic step row is appended as the terminating step.
	require.NotEmpty(t, completedSteps)
	assert.Contains(t, completedSteps[len(completedSteps)-1], "lead")
}

// TestPlanStepCov_RecoveryContractRetry_SucceedsAsHandoff — first lead
// attempt emits continue+plan in recovery mode (violation); the corrective-
// hint retry emits a valid checkpoint, which the wired handoff path commits
// and surfaces as errLeadHandoff. Drives retryRecoveryContractViolation's
// success branch.
func TestPlanStepCov_RecoveryContractRetry_SucceedsAsHandoff(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// Attempt 1: forbidden continue+plan in recovery mode.
		`{"outcome":"continue","plan":{"steps":["researcher"]},"message":"replan"}`,
		// Retry: a valid checkpoint — the contract-satisfying move.
		`{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"alt approach?","options":[{"id":"a","label":"try cached"},{"id":"abort","label":"abort"}]},"message":"proposing alternatives"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := planStepCov_conversationalExecutor(rt, er, ar, tr)

	task := &persistence.Task{ID: "t-rcv2", ProjectID: "p", Status: persistence.TaskStatusRunning, CreatedAt: time.Now(), Payload: []byte(`{"context":{"prompt":"go"}}`)}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-rcv2", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{
		PendingRecovery: &RecoveryContext{FailedStep: "research", FailureClass: "verifier_block"},
	}

	_, _, _, _, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"recover", step, time.Minute, state, nil, nil,
	)
	require.Error(t, err)
	assert.True(t, IsLeadHandoff(err),
		"a corrective-hint retry that emits a valid checkpoint must surface as the handoff sentinel")
	assert.Equal(t, 2, rt.StartCalls(), "lead runs once + one corrective retry")
}

// TestPlanStepCov_RunLeadPlanning_RefusalCorrectiveRetry — the lead's first
// output is a refusal (empty steps + message), triggering the refusal
// corrective-hint retry which then emits a valid plan. Drives the
// isPlanRefusal retry branch in runLeadPlanning.
func TestPlanStepCov_RunLeadPlanning_RefusalCorrectiveRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// Attempt 1: refusal — empty steps, non-empty message.
		`{"plan":{"steps":[]},"message":"I cannot plan this; out of scope"}`,
		// Refusal retry: a valid research-only plan.
		`{"plan":{"steps":["researcher"]},"message":"scoping the request"}`,
		// The researcher child's result.
		`{"status":"COMPLETED","message":"scoped"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-refuse", ProjectID: "p", CreatedAt: time.Now(), Payload: []byte(`{"context":{"prompt":"do the ambiguous thing"}}`)}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-refuse", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "final"}
	state := &executionState{}

	_, _, nextStep, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan", step, time.Minute, state, nil, nil,
	)
	require.NoError(t, err, "refusal-retry that recovers a plan must succeed")
	assert.Equal(t, "final", nextStep)
	assert.Equal(t, []string{"researcher"}, state.PlanSteps)
	require.Len(t, completedSteps, 1)
}
