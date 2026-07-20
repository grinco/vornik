package executor

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// --- test fakes ---------------------------------------------------------
//
// The shared MockExecRepo.List ignores ExecutionFilter.TaskID (it only
// honors Status) and MockArtifactRepo.List always returns nil, so neither
// can prove per-child isolation. These fakes mirror the production
// postgres repos: List honors the TaskID filter (executions) and the
// ExecutionID filter (artifacts). Only the List methods are exercised by
// gatherChildArtifacts; the embedded interface supplies the rest of the
// method set (a call to any of them would panic — none happen here).

type gatherFakeExecRepo struct {
	ExecutionRepository
	execs []*persistence.Execution
}

func (f *gatherFakeExecRepo) List(_ context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	var out []*persistence.Execution
	for _, e := range f.execs {
		if filter.TaskID != nil && e.TaskID != *filter.TaskID {
			continue
		}
		if filter.Status != nil && e.Status != *filter.Status {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	return out, nil
}

type gatherFakeArtifactRepo struct {
	ArtifactRepository
	artifacts []*persistence.Artifact
}

func (f *gatherFakeArtifactRepo) List(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	var out []*persistence.Artifact
	for _, a := range f.artifacts {
		if filter.ExecutionID != nil {
			if a.ExecutionID == nil || *a.ExecutionID != *filter.ExecutionID {
				continue
			}
		}
		if filter.TaskID != nil {
			if a.TaskID == nil || *a.TaskID != *filter.TaskID {
				continue
			}
		}
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

// --- test helpers -------------------------------------------------------

func delegationMode(m persistence.DelegationMode) *persistence.DelegationMode { return &m }

func newGatherExecutor(er *gatherFakeExecRepo, ar *gatherFakeArtifactRepo) *Executor {
	return &Executor{
		execRepo:     er,
		artifactRepo: ar,
		logger:       zerolog.Nop(),
		config:       DefaultConfig(),
	}
}

func completedExec(id, taskID string, createdAt time.Time) *persistence.Execution {
	return &persistence.Execution{
		ID:        id,
		TaskID:    taskID,
		Status:    persistence.ExecutionStatusCompleted,
		CreatedAt: createdAt,
	}
}

func artifactFor(execID, taskID, name, storagePath string) *persistence.Artifact {
	return &persistence.Artifact{
		ID:          name + "-" + execID,
		ExecutionID: strptr(execID),
		TaskID:      strptr(taskID),
		Name:        name,
		StoragePath: storagePath,
	}
}

// entryNames returns the "name" field of each staged entry, sorted, for
// order-independent membership assertions.
func entryNames(entries []map[string]string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e["name"])
	}
	sort.Strings(out)
	return out
}

var t0 = time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

// TestGatherChildArtifacts_ExcludesCheckpointAndCalleeChildren proves the
// membership discriminator: only children with DelegationMode != nil (the
// delegation engine's spawns) are gathered. Checkpoint-retry children and
// call_project/spawn_project callees (which carry CreationSource=DELEGATION
// but NO DelegationMode) must be excluded — the airtight filter from §3.2.
func TestGatherChildArtifacts_ExcludesCheckpointAndCalleeChildren(t *testing.T) {
	// checkpoint child (DelegationMode nil), callee child (DelegationMode
	// nil), delegation child (DelegationMode set).
	checkpoint := &persistence.Task{ID: "task_checkpointxxx", CreatedAt: t0}
	callee := &persistence.Task{
		ID:             "task_calleexxxxxx",
		CreationSource: persistence.TaskCreationSource("DELEGATION"),
		CreatedAt:      t0.Add(time.Minute),
	}
	deleg := &persistence.Task{
		ID:             "task_delegatexxxx",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0.Add(2 * time.Minute),
	}

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_cp", checkpoint.ID, t0),
		completedExec("exec_callee", callee.ID, t0),
		completedExec("exec_deleg", deleg.ID, t0),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_cp", checkpoint.ID, "cp.md", "/store/cp.md"),
		artifactFor("exec_callee", callee.ID, "callee.md", "/store/callee.md"),
		artifactFor("exec_deleg", deleg.ID, "findings.md", "/store/findings.md"),
	}}
	e := newGatherExecutor(er, ar)

	entries, summary := e.gatherChildArtifacts(context.Background(),
		[]*persistence.Task{checkpoint, callee, deleg})

	require.Len(t, entries, 1, "only the delegation child should be staged")
	assert.Contains(t, entries[0]["name"], "findings.md")
	assert.Equal(t, "/store/findings.md", entries[0]["sourcePath"])
	assert.Equal(t, 1, summary.Expected, "Expected counts delegation children only")
	assert.Equal(t, 1, summary.Staged)
	assert.Empty(t, summary.Missing)
	assert.Empty(t, summary.Empty)
}

// TestGatherChildArtifacts_LatestCompletedExecution proves the per-child
// execution selection: max created_at among COMPLETED executions, and a
// created_at collision is broken deterministically by executionID (max id).
func TestGatherChildArtifacts_LatestCompletedExecution(t *testing.T) {
	// child A: older COMPLETED + newer COMPLETED + a newer RUNNING (ignored).
	childA := &persistence.Task{
		ID:             "task_childAxxxxxx",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	oldExec := completedExec("execA_old", childA.ID, t0)
	newExec := completedExec("execA_new", childA.ID, t0.Add(time.Hour))
	runningExec := &persistence.Execution{
		ID:        "execA_running",
		TaskID:    childA.ID,
		Status:    persistence.ExecutionStatusRunning,
		CreatedAt: t0.Add(2 * time.Hour), // newest, but NOT completed → ignored
	}

	// child B: two COMPLETED executions with EQUAL created_at → tiebreak by
	// max executionID ("execB_zzz" > "execB_aaa").
	childB := &persistence.Task{
		ID:             "task_childBxxxxxx",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0.Add(time.Minute),
	}
	tie := t0.Add(30 * time.Minute)
	execBaaa := completedExec("execB_aaa", childB.ID, tie)
	execBzzz := completedExec("execB_zzz", childB.ID, tie)

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		oldExec, newExec, runningExec, execBaaa, execBzzz,
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("execA_old", childA.ID, "old.md", "/store/old.md"),
		artifactFor("execA_new", childA.ID, "new.md", "/store/new.md"),
		artifactFor("execA_running", childA.ID, "running.md", "/store/running.md"),
		artifactFor("execB_aaa", childB.ID, "aaa.md", "/store/aaa.md"),
		artifactFor("execB_zzz", childB.ID, "zzz.md", "/store/zzz.md"),
	}}
	e := newGatherExecutor(er, ar)

	entries, summary := e.gatherChildArtifacts(context.Background(),
		[]*persistence.Task{childA, childB})

	names := entryNames(entries)
	// childA → newest COMPLETED only; childB → max-id COMPLETED only.
	require.Len(t, entries, 2)
	assert.Contains(t, names[0]+names[1], "new.md")
	assert.Contains(t, names[0]+names[1], "zzz.md")
	assert.NotContains(t, names[0]+names[1], "old.md")
	assert.NotContains(t, names[0]+names[1], "running.md")
	assert.NotContains(t, names[0]+names[1], "aaa.md")
	assert.Equal(t, 2, summary.Staged)
	assert.Empty(t, summary.Missing)
	assert.Empty(t, summary.Empty)
}

// TestGatherChildArtifacts_EmptyAndMissing proves the completeness
// contract: a child that COMPLETED with zero artifacts is surfaced in
// Empty (the T-06b5 failure class — "succeeded" but contributed nothing);
// a child with NO COMPLETED execution is surfaced in Missing. Neither is
// silently dropped.
func TestGatherChildArtifacts_EmptyAndMissing(t *testing.T) {
	emptyChild := &persistence.Task{
		ID:             "task_emptyxxxxxxx",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	missingChild := &persistence.Task{
		ID:             "task_missingxxxxx",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0.Add(time.Minute),
	}

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		// emptyChild: COMPLETED but produces no artifacts.
		completedExec("exec_empty", emptyChild.ID, t0),
		// missingChild: only a FAILED execution → no COMPLETED.
		{
			ID:        "exec_failed",
			TaskID:    missingChild.ID,
			Status:    persistence.ExecutionStatusFailed,
			CreatedAt: t0,
		},
	}}
	ar := &gatherFakeArtifactRepo{artifacts: nil}
	e := newGatherExecutor(er, ar)

	entries, summary := e.gatherChildArtifacts(context.Background(),
		[]*persistence.Task{emptyChild, missingChild})

	assert.Empty(t, entries)
	assert.Equal(t, 2, summary.Expected)
	assert.Equal(t, 0, summary.Staged)
	assert.Equal(t, []string{emptyChild.ID}, summary.Empty)
	assert.Equal(t, []string{missingChild.ID}, summary.Missing)
}

// TestGatherChildArtifacts_ChildPrefixedNamesNoCollision proves the
// staged-name policy: two children emitting the same top-level artifact
// name (findings.md) are staged as distinct <childShortID>-findings.md
// with no overwrite (§3.2 F4 — flat artifacts/in/ collision guard).
func TestGatherChildArtifacts_ChildPrefixedNamesNoCollision(t *testing.T) {
	childA := &persistence.Task{
		ID:             "task_aaaaaaaaaaaa",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0,
	}
	childB := &persistence.Task{
		ID:             "task_bbbbbbbbbbbb",
		DelegationMode: delegationMode(persistence.DelegationModeParallel),
		CreatedAt:      t0.Add(time.Minute),
	}

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_a", childA.ID, t0),
		completedExec("exec_b", childB.ID, t0),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_a", childA.ID, "findings.md", "/store/a/findings.md"),
		artifactFor("exec_b", childB.ID, "findings.md", "/store/b/findings.md"),
	}}
	e := newGatherExecutor(er, ar)

	entries, summary := e.gatherChildArtifacts(context.Background(),
		[]*persistence.Task{childA, childB})

	require.Len(t, entries, 2)
	names := entryNames(entries)
	// distinct staged names, no collision.
	assert.NotEqual(t, names[0], names[1])
	assert.Equal(t, "aaaaaaaaaaaa-findings.md", names[0])
	assert.Equal(t, "bbbbbbbbbbbb-findings.md", names[1])
	// distinct source paths preserved (no overwrite).
	seen := map[string]string{}
	for _, e := range entries {
		seen[e["name"]] = e["sourcePath"]
	}
	assert.Equal(t, "/store/a/findings.md", seen["aaaaaaaaaaaa-findings.md"])
	assert.Equal(t, "/store/b/findings.md", seen["bbbbbbbbbbbb-findings.md"])
	assert.Equal(t, 2, summary.Staged)
}

// TestGatherChildArtifacts_NoCrossJobLeak_T06b5 is the isolation
// regression for incident T-06b5 (task_20260720004932_70f3ca0abdc206b5):
// a deep-research job's synthesize read a PRIOR job's findings off the
// shared branch. Here two parents' delegation children are interleaved in
// the store; gathering parentA's children must return ONLY parentA's
// children's artifacts — never parentB's. Isolation is by construction:
// gatherChildArtifacts only processes the children passed to it and looks
// up each child's executions/artifacts by that child's unique TaskID.
func TestGatherChildArtifacts_NoCrossJobLeak_T06b5(t *testing.T) {
	// parent A children.
	cA1 := &persistence.Task{ID: "task_jobA_child01", DelegationMode: delegationMode(persistence.DelegationModeParallel), CreatedAt: t0}
	cA2 := &persistence.Task{ID: "task_jobA_child02", DelegationMode: delegationMode(persistence.DelegationModeParallel), CreatedAt: t0.Add(2 * time.Minute)}
	// parent B children (a different, concurrent job).
	cB1 := &persistence.Task{ID: "task_jobB_child01", DelegationMode: delegationMode(persistence.DelegationModeParallel), CreatedAt: t0.Add(time.Minute)}
	cB2 := &persistence.Task{ID: "task_jobB_child02", DelegationMode: delegationMode(persistence.DelegationModeParallel), CreatedAt: t0.Add(3 * time.Minute)}

	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("exec_A1", cA1.ID, t0),
		completedExec("exec_A2", cA2.ID, t0.Add(2*time.Minute)),
		completedExec("exec_B1", cB1.ID, t0.Add(time.Minute)),
		completedExec("exec_B2", cB2.ID, t0.Add(3*time.Minute)),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		artifactFor("exec_A1", cA1.ID, "a1.md", "/store/A1"),
		artifactFor("exec_A2", cA2.ID, "a2.md", "/store/A2"),
		artifactFor("exec_B1", cB1.ID, "b1.md", "/store/B1"),
		artifactFor("exec_B2", cB2.ID, "b2.md", "/store/B2"),
	}}
	e := newGatherExecutor(er, ar)

	// Gather ONLY parent A's children.
	entries, summary := e.gatherChildArtifacts(context.Background(),
		[]*persistence.Task{cA1, cA2})

	require.Len(t, entries, 2)
	for _, ent := range entries {
		assert.NotContains(t, ent["sourcePath"], "/store/B", "parentB findings must never leak into parentA gather (T-06b5)")
		assert.NotContains(t, ent["name"], "b1.md")
		assert.NotContains(t, ent["name"], "b2.md")
	}
	names := entryNames(entries)
	assert.Contains(t, names[0]+names[1], "a1.md")
	assert.Contains(t, names[0]+names[1], "a2.md")
	assert.Equal(t, 2, summary.Staged)
	assert.Equal(t, 2, summary.Expected)
}
