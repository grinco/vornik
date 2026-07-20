package executor

import (
	"context"
	"os"
	"time"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestStageChildArtifacts_GenericSecondWorkflow proves G5 (design §2/§8):
// the stage_child_artifacts primitive is workflow-agnostic. A SYNTHETIC
// resume_after_children workflow that is NOT deep-research — a generic
// fanout→aggregate shape — declares stage_child_artifacts on its
// post-delegation consumer step and gets its delegated children's artifacts
// staged through the SAME executor gate (stageResumeChildArtifacts) + gather
// primitive (gatherChildArtifacts), with ZERO deep-research-specific code in
// the path.
//
// The honest fakes (gatherFakeExecRepo / gatherFakeArtifactRepo, from
// child_artifacts_test.go) honor the TaskID / ExecutionID filters that the
// shared MockExecRepo / MockArtifactRepo ignore, so per-child isolation is
// really exercised, not faked away.
func TestStageChildArtifacts_GenericSecondWorkflow(t *testing.T) {
	// --- 1. An arbitrary, non-deep-research resume_after_children workflow.
	// Nothing here references deep-research / research-subtask / synthesize;
	// it is an entirely generic fan-out/aggregate graph.
	wf := &registry.Workflow{
		ID:                  "generic-fanout-aggregate",
		Entrypoint:          "fanout",
		ResumeAfterChildren: true,
		Steps: map[string]registry.WorkflowStep{
			"fanout": {
				Type:              "agent",
				Role:              "planner",
				Prompt:            "split the work into subtasks",
				OnSuccess:         "aggregate",
				DelegatedWorkflow: "generic-subtask",
			},
			"aggregate": {
				Type:                "agent",
				Role:                "aggregator",
				Prompt:              "read every file in artifacts/in and combine",
				OnSuccess:           "done",
				StageChildArtifacts: true,
			},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
	// The synthetic workflow is structurally valid — proving the Part-B
	// placement guard ADMITS a legitimate generic consumer, not just
	// deep-research.
	require.NoError(t, wf.Validate("generic-fanout-aggregate.md"),
		"a generic resume_after_children consumer must pass the placement guard")

	consumer := wf.Steps["aggregate"]

	// --- 2. A parent whose two delegated children have store artifacts. The
	// children are seeded under the parent in a MockTaskRepo so the gate fetches
	// them through e.taskRepo.GetChildren (the production path), and the honest
	// exec/artifact fakes supply real per-child isolation.
	parent := &persistence.Task{ID: "task_genericparent"}
	childA := &persistence.Task{ID: "task_genericAxxxxx", ParentTaskID: strptr(parent.ID), DelegationMode: delegationMode(persistence.DelegationModeParallel), CreatedAt: t0}
	childB := &persistence.Task{ID: "task_genericBxxxxx", ParentTaskID: strptr(parent.ID), DelegationMode: delegationMode(persistence.DelegationModeParallel), CreatedAt: t0.Add(time.Minute)}
	tr := NewMockTaskRepo()
	tr.AddTask(parent)
	tr.AddTask(childA)
	tr.AddTask(childB)
	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_genA", childA.ID, t0),
		completedExec("exec_genB", childB.ID, t0.Add(time.Minute)),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_genA", childA.ID, "part.md", "/store/genA/part.md"),
		artifactFor("exec_genB", childB.ID, "part.md", "/store/genB/part.md"),
	}}
	e := newStageExecutor(er, ar, tr)

	// --- 3. Drive the SAME gate the executor runs before the consumer step.
	opts := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, consumer, opts, "")

	// Children staged into the consumer step's input, collision-free
	// (both emit part.md → distinct <childShortID>-part.md).
	require.Len(t, opts.InputArtifacts, 2, "both generic children's artifacts must stage")
	names := entryNames(opts.InputArtifacts)
	wantA := childShortID(childA.ID) + "-part.md"
	wantB := childShortID(childB.ID) + "-part.md"
	assert.Contains(t, names, wantA)
	assert.Contains(t, names, wantB)
	assert.NotEqual(t, wantA, wantB, "the childShortID prefix keeps identically-named artifacts distinct")
	require.NotNil(t, opts.InputArtifactsSummary, "the completeness summary must be injected on the opting step")
	assert.Equal(t, 2, opts.InputArtifactsSummary.Staged)
	assert.Equal(t, 2, opts.InputArtifactsSummary.Expected)

	// --- 3b. Gate is a NO-OP on the non-declaring step of the SAME workflow.
	// Proves staging is driven by the graph-keyed step flag, not by any
	// workflow-type/deep-research check.
	nonConsumer := wf.Steps["fanout"]
	optsOff := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, nonConsumer, optsOff, "")
	assert.Empty(t, optsOff.InputArtifacts, "a step that does not declare the flag must never stage")
	assert.Nil(t, optsOff.InputArtifactsSummary)

	// --- 4. Genericity audit (grep-style): the gather primitive names NO
	// deep-research-specific symbol. child_artifacts.go is the entire gather
	// primitive; assert none of the deep-research identifiers appear in it.
	// The gate (stageResumeChildArtifacts, workflow.go) references only
	// registry.WorkflowStep.StageChildArtifacts + gatherChildArtifacts and no
	// deep-research symbol — demonstrated operationally by this test driving
	// it with an arbitrary non-deep-research workflow above.
	src, err := os.ReadFile("child_artifacts.go")
	require.NoError(t, err)
	for _, forbidden := range []string{"deep-research", "deepresearch", "deep_research", "research-subtask", "researchSubtask", "synthesize"} {
		assert.NotContainsf(t, string(src), forbidden,
			"child_artifacts.go must contain no deep-research-specific symbol (%q) — the primitive is generic (G5)", forbidden)
	}
}
