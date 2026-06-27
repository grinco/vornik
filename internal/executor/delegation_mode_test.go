package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// childrenOf returns the delegation children of parentID from the mock,
// in no particular order.
func childrenOf(t *testing.T, tr *MockTaskRepo, parentID string) []*persistence.Task {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []*persistence.Task
	for _, task := range tr.tasks {
		if task.ParentTaskID != nil && *task.ParentTaskID == parentID {
			out = append(out, task)
		}
	}
	return out
}

func newDelegationExecutor(tr *MockTaskRepo) *Executor {
	return &Executor{taskRepo: tr, logger: zerolog.Nop(), config: DefaultConfig()}
}

// TestParseDelegationMode pins the string→enum mapping, including the
// unknown/empty → PARALLEL default (operator-chosen 2026-05-30; bounded
// by the N4 fan-out guard). "sequential" must be explicit.
func TestParseDelegationMode(t *testing.T) {
	cases := map[string]persistence.DelegationMode{
		"PARALLEL":   persistence.DelegationModeParallel,
		"parallel":   persistence.DelegationModeParallel,
		" FAN_OUT ":  persistence.DelegationModeFanOut,
		"fan_out":    persistence.DelegationModeFanOut,
		"SEQUENTIAL": persistence.DelegationModeSequential,
		"sequential": persistence.DelegationModeSequential,
		"":           persistence.DelegationModeParallel, // empty → default (parallel)
		"garbage":    persistence.DelegationModeParallel, // unknown → default (parallel)
		"JOIN":       persistence.DelegationModeParallel, // legacy/unknown → default (parallel)
	}
	for in, want := range cases {
		assert.Equalf(t, want, parseDelegationMode(in), "parseDelegationMode(%q)", in)
	}
}

// TestCreateDelegatedTasks_SequentialChainsChildren is the SEQUENTIAL
// topology proof + regression: children form a serial dependency chain
// (child[i] depends on child[i-1]) so the lease query releases them one
// at a time. The first child has no dependency (head of the chain).
func TestCreateDelegatedTasks_SequentialChainsChildren(t *testing.T) {
	tr := NewMockTaskRepo()
	e := newDelegationExecutor(tr)
	parent := &persistence.Task{ID: "parent-seq", ProjectID: "proj", Priority: 50}
	specs := []delegatedTaskSpec{
		{Prompt: "first"},
		{Prompt: "second"},
		{Prompt: "third"},
	}
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeSequential))

	kids := childrenOf(t, tr, "parent-seq")
	require.Len(t, kids, 3)

	// Exactly one head (no deps); every other child depends on exactly
	// one sibling; together they form a single chain (no two children
	// share a predecessor, no child depends on a non-sibling).
	heads := 0
	depCount := map[string]int{} // predecessor → how many children list it
	ids := map[string]bool{}
	for _, k := range kids {
		ids[k.ID] = true
	}
	for _, k := range kids {
		switch len(k.Dependencies) {
		case 0:
			heads++
		case 1:
			pred := k.Dependencies[0]
			require.Truef(t, ids[pred], "child %s depends on non-sibling %s", k.ID, pred)
			depCount[pred]++
		default:
			t.Fatalf("SEQUENTIAL child %s has %d deps, want 0 or 1", k.ID, len(k.Dependencies))
		}
		assert.Equal(t, persistence.DelegationModeSequential, *k.DelegationMode)
	}
	assert.Equal(t, 1, heads, "exactly one chain head")
	for pred, n := range depCount {
		assert.LessOrEqualf(t, n, 1, "predecessor %s fanned to %d successors — not a serial chain", pred, n)
	}
}

// TestCreateDelegatedTasks_ParallelHasNoSiblingDeps proves PARALLEL
// changes the topology: no child carries an inter-sibling dependency, so
// every child is immediately leasable (concurrency then bounded by the
// per-project caps the scheduler enforces — not by an artificial chain).
func TestCreateDelegatedTasks_ParallelHasNoSiblingDeps(t *testing.T) {
	tr := NewMockTaskRepo()
	e := newDelegationExecutor(tr)
	parent := &persistence.Task{ID: "parent-par", ProjectID: "proj", Priority: 50}
	specs := []delegatedTaskSpec{{Prompt: "a"}, {Prompt: "b"}, {Prompt: "c"}}
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeParallel))

	kids := childrenOf(t, tr, "parent-par")
	require.Len(t, kids, 3)
	for _, k := range kids {
		assert.Emptyf(t, k.Dependencies, "PARALLEL child %s must have no sibling dependency", k.ID)
		require.NotNil(t, k.DelegationMode)
		assert.Equal(t, persistence.DelegationModeParallel, *k.DelegationMode)
	}
}

// TestCreateDelegatedTasks_FanOutHasNoSiblingDeps — FAN_OUT shares the
// independent-children topology with PARALLEL (documented interpretation:
// parallel dispatch governed by the per-batch fan-out guard).
func TestCreateDelegatedTasks_FanOutHasNoSiblingDeps(t *testing.T) {
	tr := NewMockTaskRepo()
	e := newDelegationExecutor(tr)
	parent := &persistence.Task{ID: "parent-fan", ProjectID: "proj", Priority: 50}
	specs := []delegatedTaskSpec{{Prompt: "a"}, {Prompt: "b"}}
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeFanOut))

	kids := childrenOf(t, tr, "parent-fan")
	require.Len(t, kids, 2)
	for _, k := range kids {
		assert.Empty(t, k.Dependencies, "FAN_OUT child must have no sibling dependency")
		assert.Equal(t, persistence.DelegationModeFanOut, *k.DelegationMode)
	}
}

// TestCreateDelegatedTasks_UnknownModeDefaultsParallel — an empty mode
// (the most common omission) defaults to PARALLEL (operator-chosen
// 2026-05-30): independent children with no sibling dependency, still
// bounded by the N4 fan-out guard.
func TestCreateDelegatedTasks_UnknownModeDefaultsParallel(t *testing.T) {
	tr := NewMockTaskRepo()
	e := newDelegationExecutor(tr)
	parent := &persistence.Task{ID: "parent-unk", ProjectID: "proj", Priority: 50}
	specs := []delegatedTaskSpec{{Prompt: "a"}, {Prompt: "b"}}
	// Empty mode arg → default.
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationMode("")))

	kids := childrenOf(t, tr, "parent-unk")
	require.Len(t, kids, 2)
	for _, k := range kids {
		assert.Equal(t, persistence.DelegationModeParallel, *k.DelegationMode)
		assert.Emptyf(t, k.Dependencies, "empty mode defaults to PARALLEL — child %s must have no sibling dependency", k.ID)
	}
}

// TestCreateDelegatedTasks_FanOutGuardRejects — a batch larger than the
// fan-out limit is refused with a typed *delegationGuardError BEFORE any
// child is created.
func TestCreateDelegatedTasks_FanOutGuardRejects(t *testing.T) {
	tr := NewMockTaskRepo()
	e := &Executor{taskRepo: tr, logger: zerolog.Nop(), config: &Config{DelegationFanOutLimit: 2}}
	parent := &persistence.Task{ID: "p-fanguard", ProjectID: "proj"}
	specs := []delegatedTaskSpec{{Prompt: "1"}, {Prompt: "2"}, {Prompt: "3"}}

	err := e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeParallel)
	require.Error(t, err)
	var ge *delegationGuardError
	require.True(t, errors.As(err, &ge), "want *delegationGuardError, got %T", err)
	assert.Equal(t, "fanout", ge.reason)
	assert.Equal(t, persistence.TaskFailureClassDelegationGuard, ge.FailureClass())
	assert.Equal(t, persistence.TaskFailureClassDelegationGuard, ClassifyExecutionFailure(err, ""))
	assert.Empty(t, childrenOf(t, tr, "p-fanguard"), "no child may be created on fan-out violation")
}

// TestCreateDelegatedTasks_DepthGuardRejects — a parent already at the
// depth limit (its lineage has >= limit DELEGATION-source ancestors) is
// refused further delegation.
func TestCreateDelegatedTasks_DepthGuardRejects(t *testing.T) {
	tr := NewMockTaskRepo()
	e := &Executor{taskRepo: tr, logger: zerolog.Nop(), config: &Config{DelegationDepthLimit: 2}}

	// Build a DELEGATION lineage: root → d1 → d2. d2 sits 2 delegation
	// levels deep, which equals the limit, so d2 may not delegate.
	root := &persistence.Task{ID: "root", ProjectID: "proj", CreationSource: persistence.TaskCreationSourceUser}
	d1id, d2id := "d1", "d2"
	d1 := &persistence.Task{ID: d1id, ProjectID: "proj", ParentTaskID: &root.ID, CreationSource: persistence.TaskCreationSourceDelegation}
	d2 := &persistence.Task{ID: d2id, ProjectID: "proj", ParentTaskID: &d1id, CreationSource: persistence.TaskCreationSourceDelegation}
	require.NoError(t, tr.Create(context.Background(), root))
	require.NoError(t, tr.Create(context.Background(), d1))
	require.NoError(t, tr.Create(context.Background(), d2))

	before := len(childrenOf(t, tr, "d2"))
	err := e.createDelegatedTasks(context.Background(), d2, []delegatedTaskSpec{{Prompt: "deep"}}, persistence.DelegationModeSequential)
	require.Error(t, err)
	var ge *delegationGuardError
	require.True(t, errors.As(err, &ge), "want *delegationGuardError, got %T", err)
	assert.Equal(t, "depth", ge.reason)
	assert.Len(t, childrenOf(t, tr, "d2"), before, "no child may be created on depth violation")

	// Sanity: d1 (one level deep, under the limit of 2) may still delegate.
	d1Before := len(childrenOf(t, tr, "d1")) // d2 already counts as a fixture child
	require.NoError(t, e.createDelegatedTasks(context.Background(), d1, []delegatedTaskSpec{{Prompt: "ok"}}, persistence.DelegationModeSequential))
	assert.Len(t, childrenOf(t, tr, "d1"), d1Before+1)
}

// TestCreateDelegatedTasks_CycleGuardRejects — a stored lineage that
// already loops (A→B→A) is detected and the delegation refused before
// extending the broken chain.
func TestCreateDelegatedTasks_CycleGuardRejects(t *testing.T) {
	tr := NewMockTaskRepo()
	e := newDelegationExecutor(tr)

	// A's parent is B, B's parent is A — a malformed stored cycle.
	aID, bID := "cyc-a", "cyc-b"
	a := &persistence.Task{ID: aID, ProjectID: "proj", ParentTaskID: &bID, CreationSource: persistence.TaskCreationSourceDelegation}
	b := &persistence.Task{ID: bID, ProjectID: "proj", ParentTaskID: &aID, CreationSource: persistence.TaskCreationSourceDelegation}
	require.NoError(t, tr.Create(context.Background(), a))
	require.NoError(t, tr.Create(context.Background(), b))

	before := len(childrenOf(t, tr, aID)) // b is a fixture child of a
	err := e.createDelegatedTasks(context.Background(), a, []delegatedTaskSpec{{Prompt: "x"}}, persistence.DelegationModeSequential)
	require.Error(t, err)
	var ge *delegationGuardError
	require.True(t, errors.As(err, &ge), "want *delegationGuardError, got %T", err)
	assert.Equal(t, "cycle", ge.reason)
	assert.Len(t, childrenOf(t, tr, aID), before, "no child may be created on cycle violation")
}

// TestCreateDelegatedTasks_AtFanOutLimitAllowed — a batch exactly at the
// limit is allowed (boundary condition: reject only > limit).
func TestCreateDelegatedTasks_AtFanOutLimitAllowed(t *testing.T) {
	tr := NewMockTaskRepo()
	e := &Executor{taskRepo: tr, logger: zerolog.Nop(), config: &Config{DelegationFanOutLimit: 3}}
	parent := &persistence.Task{ID: "p-atlimit", ProjectID: "proj"}
	specs := []delegatedTaskSpec{{Prompt: "1"}, {Prompt: "2"}, {Prompt: "3"}}
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeParallel))
	assert.Len(t, childrenOf(t, tr, "p-atlimit"), 3)
}

// TestCreateDelegatedTasks_WalkRepoErrorSurfaces — when the lineage walk
// (cycle/depth) hits a repo error, it surfaces as a wrapped error (NOT a
// guard error) so the parent fails for the right reason and no child is
// created.
func TestCreateDelegatedTasks_WalkRepoErrorSurfaces(t *testing.T) {
	tr := NewMockTaskRepo()
	tr.err = errors.New("db unavailable") // every Get fails
	e := newDelegationExecutor(tr)
	// Parent has a ParentTaskID so the walk actually calls Get.
	gp := "grandparent"
	parent := &persistence.Task{ID: "p-walkerr", ProjectID: "proj", ParentTaskID: &gp,
		CreationSource: persistence.TaskCreationSourceDelegation}

	err := e.createDelegatedTasks(context.Background(), parent, []delegatedTaskSpec{{Prompt: "x"}}, persistence.DelegationModeSequential)
	require.Error(t, err)
	var ge *delegationGuardError
	assert.False(t, errors.As(err, &ge), "repo error must not masquerade as a guard rejection")
	assert.Contains(t, err.Error(), "cycle check failed")
}

// TestCreateDelegatedTasks_NilAncestorTerminatesWalk — a dangling
// ParentTaskID (ancestor row missing) terminates the walk cleanly at
// depth/no-cycle rather than erroring, so an orphaned-but-valid parent
// can still delegate.
func TestCreateDelegatedTasks_NilAncestorTerminatesWalk(t *testing.T) {
	tr := NewMockTaskRepo()
	e := newDelegationExecutor(tr)
	missing := "ghost-ancestor" // never Created → Get returns (nil, nil)
	parent := &persistence.Task{ID: "p-orphan", ProjectID: "proj", ParentTaskID: &missing,
		CreationSource: persistence.TaskCreationSourceDelegation}
	require.NoError(t, tr.Create(context.Background(), parent))

	require.NoError(t, e.createDelegatedTasks(context.Background(), parent,
		[]delegatedTaskSpec{{Prompt: "x"}}, persistence.DelegationModeSequential))
	// One new child (the parent fixture is itself a child of the ghost,
	// but childrenOf(p-orphan) only counts children of p-orphan).
	assert.Len(t, childrenOf(t, tr, "p-orphan"), 1)
}

// TestDelegationLimitDefaults — unset config falls back to the LLD's
// documented Phase-1 defaults (depth 5, fan-out 20).
func TestDelegationLimitDefaults(t *testing.T) {
	e := &Executor{config: &Config{}}
	assert.Equal(t, defaultDelegationDepthLimit, e.delegationDepthLimit())
	assert.Equal(t, defaultDelegationFanOutLimit, e.delegationFanOutLimit())
	assert.Equal(t, 5, defaultDelegationDepthLimit)
	assert.Equal(t, 20, defaultDelegationFanOutLimit)

	// Nil config is tolerated (defensive — direct struct construction in
	// tests can leave it nil).
	eNil := &Executor{}
	assert.Equal(t, defaultDelegationDepthLimit, eNil.delegationDepthLimit())
	assert.Equal(t, defaultDelegationFanOutLimit, eNil.delegationFanOutLimit())
}
