package executor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// planCov_resolver is a small WorkflowResolver for the snapshot-pinning
// branches of resolveExecutionPlan.
func planCov_resolver() *MockWorkflowResolver {
	return &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker"}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "go",
				Steps: map[string]registry.WorkflowStep{
					"go": {Type: "agent", Role: "worker", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	}
}

// TestPlanCov_ResolveExecutionPlan_CapturesSnapshotWhenAbsent — first run with
// no snapshot: resolveExecutionPlan captures one from the live workflow.
func TestPlanCov_ResolveExecutionPlan_CapturesSnapshotWhenAbsent(t *testing.T) {
	e, _, er, _, _ := setup()
	e.SetWorkflowResolver(planCov_resolver())

	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "wf1", plan.workflow.ID)

	// A snapshot was captured for future resumes.
	snap, _ := er.GetWorkflowSnapshot(context.Background(), "e1")
	assert.NotEmpty(t, snap, "first resolve must capture a workflow snapshot")
}

// TestPlanCov_ResolveExecutionPlan_UsesExistingSnapshot — a snapshot already
// exists; resolveExecutionPlan deserializes and uses it (the pinned-replay
// branch). We pin a snapshot whose ID differs so we can prove it was used.
func TestPlanCov_ResolveExecutionPlan_UsesExistingSnapshot(t *testing.T) {
	e, _, er, _, _ := setup()
	e.SetWorkflowResolver(planCov_resolver())

	pinned := &registry.Workflow{
		ID:         "wf1",
		Entrypoint: "pinned_entry",
		Steps: map[string]registry.WorkflowStep{
			"pinned_entry": {Type: "agent", Role: "worker", OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	data, _ := json.Marshal(pinned)

	exec := &persistence.Execution{ID: "e2", TaskID: "t2", ProjectID: "p1"}
	require.NoError(t, er.Create(context.Background(), exec))
	require.NoError(t, er.SetWorkflowSnapshot(context.Background(), "e2", data))

	task := &persistence.Task{ID: "t2", ProjectID: "p1"}
	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	require.NoError(t, err)
	assert.Equal(t, "pinned_entry", plan.workflow.Entrypoint,
		"resolve must replay the pinned snapshot body, not the live workflow")
}

// TestPlanCov_ResolveExecutionPlan_BadSnapshotFallsBackToLive — a corrupt
// snapshot blob must not abort the resolve; it falls back to the live workflow.
func TestPlanCov_ResolveExecutionPlan_BadSnapshotFallsBackToLive(t *testing.T) {
	e, _, er, _, _ := setup()
	e.SetWorkflowResolver(planCov_resolver())

	exec := &persistence.Execution{ID: "e3", TaskID: "t3", ProjectID: "p1"}
	require.NoError(t, er.Create(context.Background(), exec))
	require.NoError(t, er.SetWorkflowSnapshot(context.Background(), "e3", []byte("{not json")))

	task := &persistence.Task{ID: "t3", ProjectID: "p1"}
	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	require.NoError(t, err)
	assert.Equal(t, "go", plan.workflow.Entrypoint, "corrupt snapshot must fall back to live workflow")
}

// --- buildAgentInput: branches not hit by the existing input-path tests ---

func TestPlanCov_BuildAgentInput_UnparseablePayloadWarns(t *testing.T) {
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(`{not valid json`)}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "worker", "", &agentInputOpts{})

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	ctxMap := parsed["context"].(map[string]any)
	prompt, _ := ctxMap["prompt"].(string)
	assert.Contains(t, prompt, "task payload could not be parsed",
		"an unparseable payload must surface a WARNING in the prompt")
}

func TestPlanCov_BuildAgentInput_TaskTypeFallbackPrompt(t *testing.T) {
	// No context.prompt, but a meaningful taskType (the API path stores the
	// prompt as taskType) — userPrompt falls back to taskType.
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(`{"taskType":"Build the widget"}`)}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "worker", "", &agentInputOpts{})
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	ctxMap := parsed["context"].(map[string]any)
	prompt, _ := ctxMap["prompt"].(string)
	assert.Contains(t, prompt, "Build the widget")
	assert.Equal(t, "Build the widget", ctxMap["taskType"])
}

func TestPlanCov_BuildAgentInput_WatchlistAndActivityBlocks(t *testing.T) {
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(`{"context":{"prompt":"strategize"}}`)}
	opts := &agentInputOpts{
		RecentActivityBlock:      "filled order AAPL",
		WatchlistQuotesBlock:     "AAPL 190.12",
		WatchlistIndicatorsBlock: "AAPL RSI 55",
	}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "strategist", "", opts)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	prompt := parsed["context"].(map[string]any)["prompt"].(string)
	assert.Contains(t, prompt, "## RECENT_ACTIVITY_24H")
	assert.Contains(t, prompt, "filled order AAPL")
	assert.Contains(t, prompt, "## WATCHLIST_QUOTES")
	assert.Contains(t, prompt, "AAPL 190.12")
	assert.Contains(t, prompt, "## WATCHLIST_INDICATORS")
	assert.Contains(t, prompt, "AAPL RSI 55")
}

func TestPlanCov_BuildAgentInput_CanonicalContextSurfaced(t *testing.T) {
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(`{"context":{"prompt":"work"}}`)}
	opts := &agentInputOpts{
		SystemPrompt: "you are a coder",
		CanonicalContext: CanonicalContext{
			ProjectContext: "the spec",
			UserGuidance:   "be terse",
			Source:         "PROJECT_CONTEXT.md",
			Truncated:      []string{"USER_GUIDANCE.md"},
		},
	}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "coder", "", opts)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	ctxMap := parsed["context"].(map[string]any)
	assert.Equal(t, "the spec", ctxMap["projectContext"])
	assert.Equal(t, "be terse", ctxMap["userGuidance"])
	assert.Equal(t, "PROJECT_CONTEXT.md", ctxMap["projectContextSource"])
	require.Contains(t, ctxMap, "projectContextTruncated")
	// systemPrompt is composed with the canonical-context guidance.
	sp, _ := ctxMap["systemPrompt"].(string)
	assert.Contains(t, sp, "you are a coder")
}

func TestPlanCov_BuildAgentInput_AdaptiveRoleOrdering(t *testing.T) {
	// PreviousResult set → isAdaptiveRole true → step instructions first,
	// task framed as reference.
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(`{"context":{"prompt":"Build a Snake game"}}`)}
	opts := &agentInputOpts{
		PreviousResult:             "scout findings",
		StepPrompt:                 "ROLE: you are the writer",
		AdaptiveCandidateWorkflows: []string{"research", "build"},
		ResponseFormat:             "json_object",
	}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "writer", "ignored step prompt", opts)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	ctxMap := parsed["context"].(map[string]any)
	prompt := ctxMap["prompt"].(string)
	assert.Contains(t, prompt, "ROLE: you are the writer")
	assert.Contains(t, prompt, "Original task (for reference")
	assert.Equal(t, "scout findings", ctxMap["previousStepResult"])
	require.Contains(t, ctxMap, "adaptiveCandidateWorkflows")
	// responseFormat surfaces under config.
	cfg := parsed["config"].(map[string]any)
	assert.Equal(t, "json_object", cfg["responseFormat"])
}

func TestPlanCov_BuildAgentInput_PermissionsOverride(t *testing.T) {
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(`{"context":{"prompt":"x"}}`)}
	opts := &agentInputOpts{
		Permissions: &registry.SwarmRolePermissions{
			DelegationAllowed: true,
			AllowedTools:      []string{"memory_search"},
		},
	}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "lead", "", opts)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	perms := parsed["config"].(map[string]any)["permissions"].(map[string]any)
	assert.Equal(t, true, perms["delegationAllowed"])
	tools := perms["allowedTools"].([]any)
	assert.Equal(t, "memory_search", tools[0])
}

// TestPlanCov_BuildAgentInput_InputFilesAndExtractions exercises the
// payload.context.inputFiles + inputExtractions parsing, the host-path
// rewrite, the ATTACHED FILES block append, and the opts.InputArtifacts
// surfacing — closing the remaining buildAgentInput branches.
func TestPlanCov_BuildAgentInput_InputFilesAndExtractions(t *testing.T) {
	payload := `{"context":{"prompt":"read /tmp/cv.pdf and report","inputFiles":["/host/uploads/cv.pdf"],"inputExtractions":[{"title":"CV","artifact_id":"art_1","extracted_document_id":"doc_1","section_count":3,"chunks_ingested":7}]}}`
	task := &persistence.Task{ID: "tx", ProjectID: "p", Payload: []byte(payload)}
	opts := &agentInputOpts{
		InputArtifacts: []map[string]string{{"name": "prior.md", "path": "/in/prior.md"}},
	}
	raw := buildAgentInput(task, "e1", "wf", "s", "step", "researcher", "", opts)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	ctxMap := parsed["context"].(map[string]any)
	prompt := ctxMap["prompt"].(string)
	// The hallucinated /tmp/cv.pdf is rewritten to the container path.
	assert.Contains(t, prompt, "/app/workspace/artifacts/in/cv.pdf")
	// The extracted-document block routes the agent to memory tools.
	assert.Contains(t, prompt, "ATTACHED DOCUMENTS")
	assert.Contains(t, prompt, "doc_1")
	// opts.InputArtifacts surface under context.inputArtifacts.
	require.Contains(t, ctxMap, "inputArtifacts")
}

// --- findSwarmRole: the not-found + nil-swarm branches ---

func TestPlanCov_FindSwarmRole(t *testing.T) {
	sw := &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{{Name: "writer"}}}
	r, err := findSwarmRole(sw, "writer")
	require.NoError(t, err)
	assert.Equal(t, "writer", r.Name)

	_, err = findSwarmRole(sw, "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	_, err = findSwarmRole(nil, "writer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available")
}

// --- effectiveResponseFormat: each precedence branch ---

func TestPlanCov_EffectiveResponseFormat(t *testing.T) {
	assert.Equal(t, "", effectiveResponseFormat(nil))
	assert.Equal(t, "json_object",
		effectiveResponseFormat(&registry.SwarmRole{ResponseFormat: "json_object"}))
	assert.Equal(t, "json_object",
		effectiveResponseFormat(&registry.SwarmRole{RequiredOutputKeys: []string{"message"}}))
	// No schema, no required keys, no explicit format → "".
	assert.Equal(t, "", effectiveResponseFormat(&registry.SwarmRole{Name: "writer"}))
}

// --- parseDelegationMode: each branch ---

func TestPlanCov_ParseDelegationMode(t *testing.T) {
	assert.Equal(t, persistence.DelegationModeSequential, parseDelegationMode("sequential"))
	assert.Equal(t, persistence.DelegationModeFanOut, parseDelegationMode("FAN_OUT"))
	assert.Equal(t, persistence.DelegationModeParallel, parseDelegationMode(""))
	assert.Equal(t, persistence.DelegationModeParallel, parseDelegationMode("garbage"))
}
