package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// seededMessageRepo is a fakeMessageRepo whose List returns a fixed,
// pre-seeded conversation — so the recover plan step's runLeadPlanning
// resolves the operator's structured-action answer from it on resume.
type seededMessageRepo struct {
	fakeMessageRepo
	convo []*persistence.TaskMessage
}

func (s *seededMessageRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return s.convo, nil
}

// failFirstRuntime fails the FIRST StartContainer (the `work` step) and
// then behaves like the embedded MockRuntime for the recover lead step
// (writing outputJSON as the lead's result.json). This expresses the
// "one step fails, the next succeeds" shape the plain MockRuntime can't.
type failFirstRuntime struct {
	*MockRuntime
	mu    sync.Mutex
	calls int
}

func (r *failFirstRuntime) StartContainer(ctx context.Context, c *runtime.ContainerConfig) (string, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first {
		return "", errors.New("work step failed")
	}
	return r.MockRuntime.StartContainer(ctx, c)
}

// TestRecoveryCheckpointRoundTrip_StructuredRerouteApplied is the Tier-1
// recovery-checkpoint round-trip E2E (https://docs.vornik.io) for LLD §9.
//
// Regression context: task_20260620123718_370df0973ac06941 — a 2-step
// research workflow's writer failed; the operator answered "re-run, then
// planner, then writer" but planner wasn't in that workflow and NOTHING
// structural happened (the answer was only a prose hint). This test pins
// the fix: the operator's chosen option carries a structured
// reroute_workflow action, and on resume the executor APPLIES it
// (delegates a child on the chosen candidate workflow) before re-planning.
//
// Flow: `work` step fails → on_fail routes to the `recover` plan step →
// the recovery plan step loads the conversation (a decision checkpoint +
// the operator's answer picking the reroute option) → resolveOperatorCheckpointAction
// finds the action → applyRecoveryCheckpointAction delegates a child task
// on the operator-chosen candidate workflow. Deterministic; no sleeps
// beyond the existing Eventually poll, no real podman.
func TestRecoveryCheckpointRoundTrip_StructuredRerouteApplied(t *testing.T) {
	base := NewMockRuntime()
	// The recover lead emits a valid recovery decision checkpoint (so the
	// recovery contract is satisfied and the lead handoff fires). The
	// structured action was already applied before this.
	base.outputJSON = `{"outcome":"checkpoint","checkpoint":{"kind":"decision","question":"re-run on planner?","options":[{"id":"reroute","label":"Re-run via planner","action":{"type":"reroute_workflow","workflow":"research-planner"}},{"id":"skip","label":"Skip","action":{"type":"skip"}}]}}`
	rt := &failFirstRuntime{MockRuntime: base}

	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	// Pre-seed the conversation the recover step will read on resume:
	// the original decision checkpoint (with the structured reroute
	// option) + the operator's answer selecting it.
	cp := checkpointMsg(t, []CheckpointOption{
		{ID: "reroute", Label: "Re-run via planner", Action: &CheckpointOptionAction{Type: CheckpointActionRerouteWorkflow, Workflow: "research-planner"}},
		{ID: "skip", Label: "Skip", Action: &CheckpointOptionAction{Type: CheckpointActionSkip}},
	})
	msgs := &seededMessageRepo{convo: []*persistence.TaskMessage{cp, answerMsg("cp1", "reroute")}}

	e := NewWithOptions(rt, er, ar, tr, nil,
		WithConversationalLifecycle(msgs, &fakeScratchpadRepo{}, newFakeTaskRepo()))
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID:                         "p1",
				SwarmID:                    "s1",
				DefaultWorkflowID:          "recovery-rt",
				AdaptiveCandidateWorkflows: []string{"research-planner"},
			},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", LeadRole: "lead", Roles: []registry.SwarmRole{
				{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "fake:latest"}},
				{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "fake:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"recovery-rt": {
				ID:         "recovery-rt",
				Entrypoint: "work",
				Steps: map[string]registry.WorkflowStep{
					"work":    {Type: "agent", Role: "researcher", OnSuccess: "done", OnFail: "recover"},
					"recover": {Type: "plan", Role: "lead", OnSuccess: "done", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	parentID := "t-roundtrip"
	parentWF := "recovery-rt"
	tr.AddTask(&persistence.Task{
		ID:          parentID,
		ProjectID:   "p1",
		WorkflowID:  &parentWF,
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"context":{"prompt":"do the thing"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(parentID))

	// The structured reroute action must have delegated exactly one child
	// task on the operator-chosen candidate workflow — proving the answer
	// was applied STRUCTURALLY, not just echoed as prose.
	assert.Eventually(t, func() bool {
		children, _ := tr.GetChildren(context.Background(), parentID)
		return len(children) == 1
	}, 2*time.Second, 10*time.Millisecond,
		"recover step must delegate one child on the chosen reroute candidate workflow")

	children, err := tr.GetChildren(context.Background(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "research-planner", *children[0].WorkflowID,
		"the delegated child must run the operator-chosen candidate workflow")
	assert.Equal(t, persistence.TaskCreationSourceRoute, children[0].CreationSource)
}
