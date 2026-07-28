package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Regression suite for incident T-1089 (2026-07-28): the operator forked
// deep-research's `synthesize` step to re-run a bad synthesis. The forked
// execution staged NOTHING — no artifacts/in/, no inputArtifactsSummary — so
// the writer correctly refused to fabricate a report and the fork failed
// exactly like the run it was meant to repair.
//
// Root cause: stageResumeChildArtifacts resolved the children to gather with
// GetChildren(task.ID). replay.Forker sets a fork task's ParentTaskID to the
// SOURCE EXECUTION's task, so the delegated children live under the origin —
// they are the fork's SIBLINGS. A fork task never has delegated children of
// its own, so the gather always came back empty and every fork of a
// stage_child_artifacts consumer step was structurally guaranteed to fail.
//
// The fix walks a FORK task to its origin before gathering. These tests pin
// that walk, its containment, and its fail-safe behaviour.

// forkOf builds a FORK task parented to origin, mirroring what
// replay.Forker.Fork writes (CreationSource=FORK, ParentTaskID=source.TaskID,
// DelegationMode nil).
func forkOf(id, originID string) *persistence.Task {
	return &persistence.Task{
		ID:             id,
		ParentTaskID:   strptr(originID),
		CreationSource: persistence.TaskCreationSourceFork,
		CreatedAt:      t0.Add(100),
	}
}

// originWithDelegationChild seeds an origin task carrying one COMPLETED
// delegation child that produced a findings artifact — the deep-research shape
// after its subtask chain has run.
func originWithDelegationChild(originID, childID string) (*MockTaskRepo, *gatherFakeExecRepo, *gatherFakeArtifactRepo) {
	origin := &persistence.Task{ID: originID, CreationSource: persistence.TaskCreationSourceUser}
	child := &persistence.Task{
		ID:             childID,
		ParentTaskID:   strptr(originID),
		DelegationMode: delegationMode(persistence.DelegationModeSequential),
		CreatedAt:      t0,
	}
	tr := NewMockTaskRepo()
	tr.AddTask(origin)
	tr.AddTask(child)

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_origin_child", childID, t0),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_origin_child", childID, "findings.md", "/store/findings.md"),
	}}
	return tr, er, ar
}

// The incident itself: a fork of the declaring consumer step must see the
// ORIGIN job's delegated children's findings. Pre-fix this staged nothing.
func TestStageResumeChildArtifacts_ForkStagesOriginChildren_T1089(t *testing.T) {
	tr, er, ar := originWithDelegationChild("task_origin0001", "task_subq0001")
	fork := forkOf("task_fork1089", "task_origin0001")
	tr.AddTask(fork)
	e := newStageExecutor(er, ar, tr)

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), fork, synthesize, opts, "deep-research")

	require.Len(t, opts.InputArtifacts, 2,
		"fork must stage the ORIGIN's delegated children (T-1089: staged nothing)")
	assert.Equal(t, "upload.pdf", opts.InputArtifacts[0]["name"], "baseline preserved first")
	assert.Contains(t, opts.InputArtifacts[1]["name"], "findings.md")
	assert.Equal(t, "/store/findings.md", opts.InputArtifacts[1]["sourcePath"])

	require.NotNil(t, opts.InputArtifactsSummary,
		"fork must receive inputArtifactsSummary (T-1089: it was absent)")
	assert.Equal(t, 1, opts.InputArtifactsSummary.Expected)
	assert.Equal(t, 1, opts.InputArtifactsSummary.Staged)
	assert.Empty(t, opts.InputArtifactsSummary.Missing)
	assert.Empty(t, opts.InputArtifactsSummary.Empty)
}

// Containment: walking to the origin must not turn sibling FORK tasks (or the
// forking task itself) into staged "findings". They carry DelegationMode nil,
// so gatherChildArtifacts filters them — assert it, because the walk is what
// newly exposes them to the gather.
func TestStageResumeChildArtifacts_ForkExcludesSiblingForkOutput(t *testing.T) {
	tr, er, ar := originWithDelegationChild("task_origin0002", "task_subq0002")
	earlierFork := forkOf("task_forkearlier", "task_origin0002")
	fork := forkOf("task_forklater", "task_origin0002")
	tr.AddTask(earlierFork)
	tr.AddTask(fork)
	// The earlier fork produced its own (useless) synthesize response artifact.
	er.execs = append(er.execs, completedExec("exec_earlierfork", earlierFork.ID, t0.Add(200)))
	ar.artifacts = append(ar.artifacts,
		artifactFor("exec_earlierfork", earlierFork.ID, "synthesize-response.md", "/store/synth-resp.md"))
	e := newStageExecutor(er, ar, tr)

	opts := &agentInputOpts{}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), fork, synthesize, opts, "deep-research")

	require.Len(t, opts.InputArtifacts, 1, "only the delegation child's findings")
	assert.Contains(t, opts.InputArtifacts[0]["name"], "findings.md")
	for _, a := range opts.InputArtifacts {
		assert.NotContains(t, a["name"], "synthesize-response.md",
			"a sibling fork (DelegationMode nil) must never be staged as findings")
	}
	require.NotNil(t, opts.InputArtifactsSummary)
	assert.Equal(t, 1, opts.InputArtifactsSummary.Expected,
		"Expected counts delegation children only — forks excluded")
}

// A fork of a fork must keep walking to the delegating origin: the operator
// can fork a failed fork, and forking twice must not silently lose staging.
func TestStageResumeChildArtifacts_ForkOfForkWalksToOrigin(t *testing.T) {
	tr, er, ar := originWithDelegationChild("task_origin0003", "task_subq0003")
	firstFork := forkOf("task_fork0003a", "task_origin0003")
	secondFork := forkOf("task_fork0003b", firstFork.ID)
	tr.AddTask(firstFork)
	tr.AddTask(secondFork)
	e := newStageExecutor(er, ar, tr)

	opts := &agentInputOpts{}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), secondFork, synthesize, opts, "deep-research")

	require.Len(t, opts.InputArtifacts, 1, "fork-of-a-fork resolves through to the origin")
	assert.Contains(t, opts.InputArtifacts[0]["name"], "findings.md")
	require.NotNil(t, opts.InputArtifactsSummary)
	assert.Equal(t, 1, opts.InputArtifactsSummary.Staged)
}

// Fail-safe: an unreadable origin must stage NOTHING rather than fall back to
// the fork's own (empty) children in a way that looks like a real gather.
// Staging nothing is already the honest outcome — the writer refuses to
// fabricate — so the requirement is "no crash, no bogus summary".
func TestStageResumeChildArtifacts_ForkOriginLookupErrorStagesNothing(t *testing.T) {
	tr, er, ar := originWithDelegationChild("task_origin0004", "task_subq0004")
	fork := forkOf("task_fork0004", "task_origin0004")
	tr.AddTask(fork)
	tr.err = errors.New("boom: task store unreachable")
	e := newStageExecutor(er, ar, tr)

	opts := &agentInputOpts{InputArtifacts: baselineArtifacts()}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), fork, synthesize, opts, "deep-research")

	assert.Len(t, opts.InputArtifacts, 1, "origin lookup error → baseline untouched")
	assert.Nil(t, opts.InputArtifactsSummary, "no summary fabricated on lookup failure")
}

// A fork whose origin row is gone (deleted) must also stage nothing, and must
// not panic on the nil task MockTaskRepo/Get returns for a miss.
func TestStageResumeChildArtifacts_ForkMissingOriginStagesNothing(t *testing.T) {
	tr := NewMockTaskRepo()
	fork := forkOf("task_fork0005", "task_origin_deleted")
	tr.AddTask(fork)
	e := newStageExecutor(&gatherFakeExecRepo{}, &gatherFakeArtifactRepo{}, tr)

	opts := &agentInputOpts{}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), fork, synthesize, opts, "deep-research")

	assert.Empty(t, opts.InputArtifacts)
	assert.Nil(t, opts.InputArtifactsSummary)
}

// A parent-pointer cycle among FORK tasks must terminate (depth cap) rather
// than spin. Staging nothing is the correct outcome for an unresolvable chain.
func TestStageResumeChildArtifacts_ForkCycleTerminates(t *testing.T) {
	tr := NewMockTaskRepo()
	a := forkOf("task_forkcycleA", "task_forkcycleB")
	b := forkOf("task_forkcycleB", "task_forkcycleA")
	tr.AddTask(a)
	tr.AddTask(b)
	e := newStageExecutor(&gatherFakeExecRepo{}, &gatherFakeArtifactRepo{}, tr)

	opts := &agentInputOpts{}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), a, synthesize, opts, "deep-research")

	assert.Empty(t, opts.InputArtifacts, "cycle resolves to no staging")
	assert.Nil(t, opts.InputArtifactsSummary)
}

// Non-fork tasks must be byte-identical to pre-fix behaviour: a DELEGATION
// child task that itself declares the flag still gathers ITS OWN children,
// with no walk to its parent. This pins that the walk is FORK-gated only —
// otherwise a nested delegation would start staging its parent job's siblings.
func TestStageResumeChildArtifacts_NonForkNeverWalksToParent(t *testing.T) {
	tr, er, ar := originWithDelegationChild("task_origin0006", "task_subq0006")
	// A delegation child of the origin that declares the flag but has no
	// children of its own. It must NOT inherit the origin's other children.
	nested := &persistence.Task{
		ID:             "task_nested0006",
		ParentTaskID:   strptr("task_origin0006"),
		CreationSource: persistence.TaskCreationSourceDelegation,
		DelegationMode: delegationMode(persistence.DelegationModeSequential),
		CreatedAt:      t0.Add(50),
	}
	tr.AddTask(nested)
	e := newStageExecutor(er, ar, tr)

	opts := &agentInputOpts{}
	synthesize := registry.WorkflowStep{Type: "agent", StageChildArtifacts: true}

	e.stageResumeChildArtifacts(context.Background(), nested, synthesize, opts, "deep-research")

	assert.Empty(t, opts.InputArtifacts,
		"a non-FORK task gathers only its own children — no parent walk")
	assert.Nil(t, opts.InputArtifactsSummary)
}
