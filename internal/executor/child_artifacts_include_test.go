package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stage_child_artifacts_include — the opt-in name filter (T-1089 follow-up).
//
// gatherChildArtifacts stages ALL artifacts of each child's latest COMPLETED
// execution. That is the deliberate canonical rule from the delegated-child-
// artifact-handoff design (§3.2: "ALL artifacts of that one execution; one
// canonical rule, no cross-execution per-name mixing") and it stays the default.
//
// But for deep-research it meant the consumer got far more than the findings:
// each research subtask harvests its declared `findings.md` AND the executor's
// own `<step>-response-*.md` transcript artifact, plus another pair per shape
// retry. The production run staged 26 entries for 10 children — the findings
// files and the verbose response files that largely duplicate them. That roughly
// doubled the writer's input and contributed directly to the prompt-token-budget
// exhaustion that stopped it calling file_write.
//
// The fix is an OPT-IN glob on the declaring consumer step. Unset ⇒ byte-
// identical to before.

// includeStep builds a declaring consumer step with the given include glob.
func includeStep(glob string) registry.WorkflowStep {
	return registry.WorkflowStep{
		Type:                       "agent",
		StageChildArtifacts:        true,
		StageChildArtifactsInclude: glob,
	}
}

// twoChildrenWithFindingsAndResponses mirrors the production shape: each child
// harvests its declared findings file plus the executor's response transcript,
// and one child also carries a shape-retry pair.
func twoChildrenWithFindingsAndResponses() (*persistence.Task, *Executor, *MockTaskRepo) {
	parent := &persistence.Task{ID: "task_inclparent01"}
	c1 := &persistence.Task{
		ID:             "task_inclchild001",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeSequential),
		CreatedAt:      t0,
	}
	c2 := &persistence.Task{
		ID:             "task_inclchild002",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeSequential),
		CreatedAt:      t0.Add(1),
	}
	tr := NewMockTaskRepo()
	tr.AddTask(parent)
	tr.AddTask(c1)
	tr.AddTask(c2)

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_incl1", c1.ID, t0),
		completedExec("exec_incl2", c2.ID, t0),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		// Child 1: declared findings + the auto-harvested response transcript.
		artifactFor("exec_incl1", c1.ID, "findings-20260728-0c32.md", "/store/f1.md"),
		artifactFor("exec_incl1", c1.ID, "research-response-20260728-0c32.md", "/store/r1.md"),
		// Child 2: same, plus a shape-retry response (the real run had these).
		artifactFor("exec_incl2", c2.ID, "findings-20260728-07bf.md", "/store/f2.md"),
		artifactFor("exec_incl2", c2.ID, "research-response-20260728-07bf.md", "/store/r2.md"),
		artifactFor("exec_incl2", c2.ID, "research_shape_retry-response-20260728-07bf.md", "/store/r2b.md"),
	}}
	e := &Executor{execRepo: er, artifactRepo: ar, taskRepo: tr, logger: zerolog.Nop(), config: DefaultConfig()}
	return parent, e, tr
}

// The incident case: with the include glob set, only the findings files stage.
func TestStageChildArtifacts_IncludeGlobDropsResponseTranscripts_T1089(t *testing.T) {
	parent, e, _ := twoChildrenWithFindingsAndResponses()

	opts := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, includeStep("findings-*.md"), opts, "deep-research")

	names := entryNames(opts.InputArtifacts)
	require.Len(t, names, 2, "only the two findings files, not the response transcripts")
	for _, n := range names {
		assert.Contains(t, n, "findings-")
		assert.NotContains(t, n, "response", "response transcripts must be filtered out")
	}
	require.NotNil(t, opts.InputArtifactsSummary)
	assert.Equal(t, 2, opts.InputArtifactsSummary.Expected)
	assert.Equal(t, 2, opts.InputArtifactsSummary.Staged)
	assert.Empty(t, opts.InputArtifactsSummary.Missing)
	assert.Empty(t, opts.InputArtifactsSummary.Empty)
}

// Default (glob unset) must be byte-identical to pre-change behaviour: every
// artifact of the latest COMPLETED execution stages, per design §3.2.
func TestStageChildArtifacts_NoIncludeGlobStagesEverything(t *testing.T) {
	parent, e, _ := twoChildrenWithFindingsAndResponses()

	opts := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, includeStep(""), opts, "deep-research")

	assert.Len(t, opts.InputArtifacts, 5, "unset glob keeps the canonical stage-everything rule")
	require.NotNil(t, opts.InputArtifactsSummary)
	assert.Equal(t, 5, opts.InputArtifactsSummary.Staged)
}

// A child whose artifacts ALL fail the glob completed but contributed nothing
// the consumer can use — it must surface in Empty[], never be silently dropped.
// That preserves the T-06b5 completeness contract under filtering.
func TestStageChildArtifacts_IncludeGlobFiltersAllForOneChildReportsEmpty(t *testing.T) {
	parent, e, tr := twoChildrenWithFindingsAndResponses()
	// A third child that only ever produced a response transcript.
	c3 := &persistence.Task{
		ID:             "task_inclchild003",
		ParentTaskID:   strptr(parent.ID),
		DelegationMode: delegationMode(persistence.DelegationModeSequential),
		CreatedAt:      t0.Add(2),
	}
	tr.AddTask(c3)
	er := e.execRepo.(*gatherFakeExecRepo)
	ar := e.artifactRepo.(*gatherFakeArtifactRepo)
	er.execs = append(er.execs, completedExec("exec_incl3", c3.ID, t0))
	ar.artifacts = append(ar.artifacts,
		artifactFor("exec_incl3", c3.ID, "research-response-20260728-dead.md", "/store/r3.md"))

	opts := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, includeStep("findings-*.md"), opts, "deep-research")

	require.NotNil(t, opts.InputArtifactsSummary)
	assert.Equal(t, 3, opts.InputArtifactsSummary.Expected)
	assert.Equal(t, 2, opts.InputArtifactsSummary.Staged)
	assert.Equal(t, []string{c3.ID}, opts.InputArtifactsSummary.Empty,
		"a child with no MATCHING artifact must be reported empty, not dropped silently")
	assert.Empty(t, opts.InputArtifactsSummary.Missing,
		"it had a COMPLETED execution, so it is empty — not missing")
}

// The glob matches the artifact's own name, NOT the <childShortID>- prefixed
// staged name. Authors write the filename their child produced; the prefix is
// an executor implementation detail they shouldn't have to encode.
func TestStageChildArtifacts_IncludeGlobMatchesUnprefixedName(t *testing.T) {
	parent, e, _ := twoChildrenWithFindingsAndResponses()

	// A glob anchored at the start of the real name matches...
	optsOK := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, includeStep("findings-*"), optsOK, "deep-research")
	assert.Len(t, optsOK.InputArtifacts, 2, "glob applies to the artifact name, unprefixed")

	// ...whereas one that assumed the staged prefix matches nothing, proving the
	// prefix is not part of the matched string.
	optsPrefixed := &agentInputOpts{}
	e.stageResumeChildArtifacts(context.Background(), parent, includeStep("*-findings-*"), optsPrefixed, "deep-research")
	assert.Empty(t, optsPrefixed.InputArtifacts,
		"the childShortID prefix must NOT be part of the matched name")
}

// A non-declaring step ignores the include glob entirely (the gate is still
// keyed on stage_child_artifacts).
func TestStageChildArtifacts_IncludeGlobInertWithoutFlag(t *testing.T) {
	parent, e, _ := twoChildrenWithFindingsAndResponses()

	opts := &agentInputOpts{}
	step := registry.WorkflowStep{Type: "agent", StageChildArtifacts: false, StageChildArtifactsInclude: "findings-*.md"}
	e.stageResumeChildArtifacts(context.Background(), parent, step, opts, "deep-research")

	assert.Empty(t, opts.InputArtifacts)
	assert.Nil(t, opts.InputArtifactsSummary)
}
