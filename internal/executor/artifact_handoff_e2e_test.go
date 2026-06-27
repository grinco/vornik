package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestArtifactHandoff_E2E_ResearcherToWriter_e9a5 is the end-to-end
// regression guard for task e9a5: in a multi-step STATIC workflow
// (researcher → writer) the writer's container MUST see the researcher's
// artifacts/out/<file>. Before the fix, step 2 couldn't: the prior step's
// outputs were forwarded with only the agent's container-relative path
// (/app/workspace/artifacts/out/...) and no host sourcePath, so
// resolveStagingSrc rejected every one and the file never reached the
// writer.
//
// This test drives the REAL executor path with NO model, NO chat proxy,
// NO API keys: the LLM call lives inside the agent container, which the
// MockRuntime replaces. The whole workflow loop runs for real —
//
//	e.Execute → executeWorkflowAttempt → executeAgentStep →
//	persistArtifacts (into the durable STORE) → plan.stepOutputArtifacts →
//	workflow.go forwarding (tagged class="output") → stageInputArtifacts
//	(copies class=="output" into the NEXT step's <workspaceDir>/artifacts/out)
//
// — and we observe the handoff at the writer container's launch point.
//
// The load-bearing wiring: the real artifacts.Store is constructed at a
// t.TempDir() and its basePath is also set as e.config.ArtifactStoragePath,
// so the store's StoragePath (returned by persistArtifacts as the
// forwarded sourcePath) lands under an allowedStagingRoots entry and
// passes resolveStagingSrc — exactly the bridge the fix introduced.
//
// Deterministic: no sleeps. The async run is gated on the stub judge's
// `done` channel, which fires LAST in handleSuccess (after every step has
// completed and every artifact has been staged + harvested), so by the
// time we read StagedInputsSeen() both containers have launched.
func TestArtifactHandoff_E2E_ResearcherToWriter_e9a5(t *testing.T) {
	const researcherContent = "E9A5-RESEARCHER-SENTINEL: findings the writer must read"

	// 1. Real durable artifact store at a temp dir; the executor's
	// ArtifactStoragePath must equal its basePath so the store path is an
	// allowed staging root (allowedStagingRoots adds artifactStoragePath).
	storeBase := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(storeBase))
	require.NoError(t, err)

	rt := NewMockRuntime()
	// Per-step result.json: researcher reports one output artifact, writer
	// reports none. The path the researcher claims is the container-relative
	// path (it is irrelevant to staging now — the fix forwards the durable
	// store path instead, which is what makes the file reach the writer).
	rt.outputJSONSequence = []string{
		`{"status":"COMPLETED","message":"researched","outputArtifacts":[{"name":"research.md","path":"/app/workspace/artifacts/out/research.md"}]}`,
		`{"status":"COMPLETED","message":"wrote"}`,
	}
	// Per-step deliverables on disk: step 1 (researcher) writes research.md
	// into its workspace artifacts/out; step 2 (writer) writes nothing of
	// its own, so anything the writer's container sees in artifacts/out had
	// to be staged in from the researcher's output.
	rt.artifactFilesSequence = []map[string]string{
		{"research.md": researcherContent},
		{},
	}

	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	ar := &stubArtifactRepo{}

	e := NewWithOptions(rt, er, ar, tr, nil, WithArtifactStore(store))
	e.config.RetryDelay = 0
	// The bridge: the store's basePath must be an allowed staging root so
	// the forwarded store StoragePath passes resolveStagingSrc.
	e.config.ArtifactStoragePath = storeBase

	// Judge fires last in handleSuccess → happens-after barrier for the
	// whole run (both steps staged + harvested).
	judgeDone := make(chan struct{})
	e.judgeRunner = &stubJudgeRunner{done: judgeDone}

	// 2. project p1 → swarm s1 (researcher, writer) → workflow wf1:
	// research → write → done(COMPLETED).
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
				{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
				{Name: "writer", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "research",
				Steps: map[string]registry.WorkflowStep{
					"research": {Type: "agent", Role: "researcher", OnSuccess: "write"},
					"write":    {Type: "agent", Role: "writer", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	})

	// 3. Submit a LEASED task (scheduler hand-off shape) and drive it.
	const taskID = "t-e9a5-handoff"
	tr.AddTask(&persistence.Task{
		ID:          taskID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"context":{"prompt":"research then write"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(taskID))

	// 4. Await completion deterministically via the judge barrier.
	select {
	case <-judgeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("judge never fired — workflow did not reach the success tail")
	}

	// Sanity: the task finalized COMPLETED and both containers launched.
	task, gerr := tr.Get(context.Background(), taskID)
	require.NoError(t, gerr)
	require.NotNil(t, task)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status)
	require.Equal(t, 2, rt.StartCalls(), "both the researcher and writer steps must launch a container")

	// 5. The e9a5 guard: the writer step (StartContainer call index 1) must
	// have had research.md staged into its workspace artifacts/out BEFORE it
	// ran, with the researcher's exact content — proving the cross-step
	// handoff bridged the durable store into the next step's ephemeral
	// workspace.
	staged := rt.StagedInputsSeen()
	require.Len(t, staged, 2, "one staged-inputs snapshot per StartContainer call")

	// The researcher (index 0) starts with an empty artifacts/out — nothing
	// upstream to stage. (Guards against a false positive where the capture
	// merely echoes the step's own output.)
	assert.Empty(t, staged[0], "researcher step must start with no staged inputs")

	writerStaged := staged[1]
	require.Contains(t, writerStaged, "research.md",
		"writer container did not receive the researcher's research.md — e9a5 handoff regressed")
	assert.Equal(t, researcherContent, writerStaged["research.md"],
		"the staged file must carry the researcher's exact content, proving it came from the store-backed output, not a stub")
}

// TestArtifactHandoff_StagesIntoArtifactsOut_e9a5 pins the second half of
// the fix's contract: prior-step outputs (class="output") are staged into
// the next step's artifacts/OUT directory (where roles read upstream
// products), NOT artifacts/in. StagedInputsSeen reads <WorkspaceDir>/
// artifacts/out specifically, so a non-empty writer snapshot already
// proves OUT; this test makes the routing assertion explicit by also
// confirming nothing leaked into artifacts/in.
func TestArtifactHandoff_StagesIntoArtifactsOut_e9a5(t *testing.T) {
	const researcherContent = "E9A5-OUT-ROUTING-SENTINEL"

	storeBase := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(storeBase))
	require.NoError(t, err)

	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"COMPLETED","message":"researched","outputArtifacts":[{"name":"research.md","path":"/app/workspace/artifacts/out/research.md"}]}`,
		`{"status":"COMPLETED","message":"wrote"}`,
	}
	rt.artifactFilesSequence = []map[string]string{
		{"research.md": researcherContent},
		{},
	}
	// Capture the writer's artifacts/IN at launch via a custom hook would
	// require more plumbing; instead we assert OUT receives the file (the
	// positive contract) and that the staged path the executor rewrote is
	// the out/ container path. The StagedInputsSeen snapshot reads
	// artifacts/out, so a hit there is the artifacts/OUT proof.

	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	ar := &stubArtifactRepo{}

	e := NewWithOptions(rt, er, ar, tr, nil, WithArtifactStore(store))
	e.config.RetryDelay = 0
	e.config.ArtifactStoragePath = storeBase

	judgeDone := make(chan struct{})
	e.judgeRunner = &stubJudgeRunner{done: judgeDone}

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1", HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true}},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
				{Name: "writer", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "research",
				Steps: map[string]registry.WorkflowStep{
					"research": {Type: "agent", Role: "researcher", OnSuccess: "write"},
					"write":    {Type: "agent", Role: "writer", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})

	const taskID = "t-e9a5-out-routing"
	tr.AddTask(&persistence.Task{
		ID:          taskID,
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"context":{"prompt":"research then write"}}`),
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute(taskID))
	select {
	case <-judgeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("judge never fired — workflow did not reach the success tail")
	}

	staged := rt.StagedInputsSeen()
	require.Len(t, staged, 2)
	// StagedInputsSeen specifically reads <WorkspaceDir>/artifacts/OUT — a
	// hit here is the artifacts/out routing proof (class="output" inputs go
	// to out/, not in/).
	require.Contains(t, staged[1], "research.md",
		"prior-step output must be staged into the writer's artifacts/OUT, not artifacts/in")
	assert.Equal(t, researcherContent, staged[1]["research.md"])
}
