package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// These tests exercise the F1 graph-keyed staging gate (design §3.2) in
// isolation: stageResumeChildArtifacts is the single seam the workflow loop
// calls before running a consumer step. It fetches the parent's children via
// e.taskRepo.GetChildren and stages the delegation children's artifacts onto
// the step's opts. Keeping the assertion at the helper (over opts / summary)
// rather than a full DB-backed workflow run avoids the
// MockExecRepo/MockArtifactRepo caveat (they ignore TaskID / return nil); the
// honest gather fakes from child_artifacts_test.go supply real per-child
// isolation, and MockTaskRepo (executor_test.go) supplies real GetChildren
// filtering by parent_task_id.

// newStageExecutor wires all three repos onto the Executor: the honest gather
// fakes for exec/artifact (they honor TaskID/ExecutionID) and MockTaskRepo for
// the parent→children lookup the new signature drives through. metrics stays
// nil (RecordChildArtifactStaging is nil-safe).
func newStageExecutor(er *gatherFakeExecRepo, ar *gatherFakeArtifactRepo, tr *MockTaskRepo) *Executor {
	return &Executor{
		execRepo:     er,
		artifactRepo: ar,
		taskRepo:     tr,
		logger:       zerolog.Nop(),
		config:       DefaultConfig(),
	}
}

// baselineArtifacts mimics the task-input artifacts already on opts before the
// gate runs, so we can prove APPEND (not replace).
func baselineArtifacts() []map[string]string {
	return []map[string]string{{"name": "upload.pdf", "sourcePath": "/store/upload.pdf"}}
}

// parentWithOneDelegationChild seeds a MockTaskRepo with a single delegation
// child of parent (DelegationMode != nil), one COMPLETED execution and one
// findings artifact for it, and returns the parent task plus wired executor.
func parentWithOneDelegationChild() (*persistence.Task, *Executor) {
	parent := &persistence.Task{ID: "task_parent0001"}
	child := &persistence.Task{
		ID:             "task_stagechild01",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	tr := NewMockTaskRepo()
	tr.AddTask(parent)
	tr.AddTask(child)

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_sc", child.ID, t0),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_sc", child.ID, "findings.md", "/store/findings.md"),
	}}
	return parent, newStageExecutor(er, ar, tr)
}

// Gate OFF: a step that does NOT declare stage_child_artifacts must be
// byte-identical to today even when the parent has delegation children (I2).
func TestStageResumeChildArtifacts_FlagOff(t *testing.T) {
	parent, e := parentWithOneDelegationChild()

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	step := registry.WorkflowStep{Type: "agent", StageChildArtifacts: false}

	e.stageResumeChildArtifacts(context.Background(), parent, step, opts, "")

	assert.Len(t, opts.InputArtifacts, 1, "flag off → opts unchanged")
	assert.Equal(t, "upload.pdf", opts.InputArtifacts[0]["name"])
	assert.Nil(t, opts.InputArtifactsSummary, "flag off → no inputArtifactsSummary")
}

// A DIFFERENT (non-declaring) step on the same workflow, with the same parent
// children present, must NOT stage — the gate is keyed on the step that
// declares the flag (graph property), not on the workflow.
func TestStageResumeChildArtifacts_NonDeclaringStepNotStaged(t *testing.T) {
	parent, e := parentWithOneDelegationChild()

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	publish := registry.WorkflowStep{Type: "agent", StageChildArtifacts: false} // sibling consumer

	e.stageResumeChildArtifacts(context.Background(), parent, publish, opts, "")

	assert.Len(t, opts.InputArtifacts, 1, "non-declaring step never stages")
	assert.Nil(t, opts.InputArtifactsSummary)
}

// Gate ON but the parent has no children yet (GetChildren returns empty) → no
// staging (the gate never fires before the children exist).
func TestStageResumeChildArtifacts_ZeroChildren(t *testing.T) {
	parent := &persistence.Task{ID: "task_lonelyparent"}
	tr := NewMockTaskRepo()
	tr.AddTask(parent) // no children seeded
	e := newStageExecutor(&gatherFakeExecRepo{}, &gatherFakeArtifactRepo{}, tr)

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	step := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), parent, step, opts, "")

	assert.Len(t, opts.InputArtifacts, 1, "zero children → no staging")
	assert.Nil(t, opts.InputArtifactsSummary)
}

// TestStageResumeChildArtifacts_ConsumerStepStagesWithoutRouteGuard is the
// regression for the T-06b5 gate bug — the flag is on the CONSUMER step
// (synthesize), never the entrypoint; staging MUST fire from the parent's
// delegation children regardless of any resume-guard signal. There is NO
// routeAlreadyHandled param anymore — that's the point. A checkpoint/callee
// child (DelegationMode == nil) mixed into GetChildren must NOT be staged
// (Expected counts only delegation children).
func TestStageResumeChildArtifacts_ConsumerStepStagesWithoutRouteGuard(t *testing.T) {
	parent := &persistence.Task{ID: "task_deepresearch"}
	delegChild := &persistence.Task{
		ID:             "task_synthchild01",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	// A checkpoint-retry child (DelegationMode == nil) sharing the parent — it
	// must be filtered out by gatherChildArtifacts and never staged.
	ckptChild := &persistence.Task{
		ID:           "task_checkpoint01",
		ParentTaskID: strptr(parent.ID),
		CreatedAt:    t0.Add(1),
	}
	tr := NewMockTaskRepo()
	tr.AddTask(parent)
	tr.AddTask(delegChild)
	tr.AddTask(ckptChild)

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_synth", delegChild.ID, t0),
		completedExec("exec_ckpt", ckptChild.ID, t0),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_synth", delegChild.ID, "findings.md", "/store/findings.md"),
		artifactFor("exec_ckpt", ckptChild.ID, "checkpoint.md", "/store/checkpoint.md"),
	}}
	e := newStageExecutor(er, ar, tr)

	// The consumer step (deep-research's `synthesize`, = decompose.on_success)
	// declares the flag. It is NOT the resume_after_children entrypoint — the
	// prod bug was that the gate required a resume-guard signal that only fires
	// at the entrypoint, so it could never fire here.
	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), parent, synthesize, opts, "deep-research")

	require.Len(t, opts.InputArtifacts, 2, "baseline + the delegation child's findings, appended")
	assert.Equal(t, "upload.pdf", opts.InputArtifacts[0]["name"], "baseline preserved first")
	assert.Contains(t, opts.InputArtifacts[1]["name"], "findings.md")
	assert.Equal(t, "/store/findings.md", opts.InputArtifacts[1]["sourcePath"])
	// The checkpoint child's artifact must never be staged.
	for _, a := range opts.InputArtifacts {
		assert.NotContains(t, a["name"], "checkpoint.md", "checkpoint child (DelegationMode nil) must not be staged")
	}

	require.NotNil(t, opts.InputArtifactsSummary, "summary injected when the gate fires")
	assert.Equal(t, 1, opts.InputArtifactsSummary.Expected, "Expected counts delegation children only")
	assert.Equal(t, 1, opts.InputArtifactsSummary.Staged)
	assert.Empty(t, opts.InputArtifactsSummary.Missing)
	assert.Empty(t, opts.InputArtifactsSummary.Empty)
}

// Re-resume (the declaring step re-entered) must not accumulate: each pass
// assembles opts fresh, so re-staging is byte-identical (idempotent, §3.2).
func TestStageResumeChildArtifacts_ReResumeNoDuplication(t *testing.T) {
	parent, e := parentWithOneDelegationChild()
	step := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	// First pass — fresh opts.
	opts1 := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	e.stageResumeChildArtifacts(context.Background(), parent, step, opts1, "")

	// Second pass — fresh opts again (mirrors the loop rebuilding opts per step
	// entry).
	opts2 := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	e.stageResumeChildArtifacts(context.Background(), parent, step, opts2, "")

	assert.Equal(t, len(opts1.InputArtifacts), len(opts2.InputArtifacts),
		"re-resume stages the same count — no accumulation")
	require.Len(t, opts2.InputArtifacts, 2)
	require.NotNil(t, opts1.InputArtifactsSummary)
	require.NotNil(t, opts2.InputArtifactsSummary)
	assert.Equal(t, opts1.InputArtifactsSummary.Staged, opts2.InputArtifactsSummary.Staged)
}

// The summary reaches the agent input context only when set (gate held); a nil
// summary must not add the key (non-opting steps never see it).
func TestBuildAgentContextMap_InputArtifactsSummaryInjection(t *testing.T) {
	withSummary := &agentInputOpts{
		InputArtifacts:        baselineArtifacts(),
		InputArtifactsSummary: &childArtifactSummary{Expected: 3, Staged: 2, Missing: []string{"task_x"}},
	}
	cm := buildAgentContextMap("dev", "prompt", currentDateTimeContext{}, withSummary)
	require.Contains(t, cm, "inputArtifactsSummary")
	got, ok := cm["inputArtifactsSummary"].(*childArtifactSummary)
	require.True(t, ok, "summary surfaced as *childArtifactSummary")
	assert.Equal(t, 2, got.Staged)

	without := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	cm2 := buildAgentContextMap("dev", "prompt", currentDateTimeContext{}, without)
	assert.NotContains(t, cm2, "inputArtifactsSummary", "no summary → key absent")
}
