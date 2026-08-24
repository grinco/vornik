package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// Regression, 2026-08-22: a lineage ancestor whose row is gone must END the
// walk, not fail it.
//
// All four walks handled the end of the chain as `if anc == nil` — a branch
// that is DEAD against production, because TaskRepository.Get is
// MissErrNotFound and returns (nil, persistence.ErrNotFound) for an absent row.
// What a missing ancestor actually took was the fatal branch: a checkpoint
// whose depth could not be counted, and a delegation refused outright with
// "delegation cycle check failed: not found".
//
// It survived because every task-repo double in this package answered a miss
// with (nil, nil) — looser than production, so the miss path was certified
// without ever being run. Tightening the doubles is what exposed it; these
// tests pin the behaviour directly so it cannot regress if a double drifts back.
func TestLineageWalks_TerminateWhenAnAncestorRowIsGone(t *testing.T) {
	ctx := context.Background()

	// child -> "ghost" (never created, so Get answers ErrNotFound).
	ghost := "ghost-ancestor"
	newExec := func() *Executor {
		return &Executor{taskRepo: NewMockTaskRepo(), logger: zerolog.Nop(), config: DefaultConfig()}
	}
	child := func(source persistence.TaskCreationSource) *persistence.Task {
		return &persistence.Task{ID: "child", ProjectID: "p1", ParentTaskID: &ghost, CreationSource: source}
	}

	t.Run("countCheckpointDepth", func(t *testing.T) {
		got, err := newExec().countCheckpointDepth(ctx, child(persistence.TaskCreationSourceCheckpoint))
		if err != nil {
			t.Fatalf("a missing ancestor must end the walk, not fail it: %v", err)
		}
		if got != 0 {
			t.Errorf("depth = %d, want 0", got)
		}
	})

	t.Run("countRouteDepth", func(t *testing.T) {
		got, err := newExec().countRouteDepth(ctx, child(persistence.TaskCreationSourceRoute))
		if err != nil {
			t.Fatalf("a missing ancestor must end the walk, not fail it: %v", err)
		}
		if got != 0 {
			t.Errorf("depth = %d, want 0", got)
		}
	})

	t.Run("countDelegationDepth", func(t *testing.T) {
		// The parent itself is DELEGATION-source, so depth counts it and stops.
		got, err := newExec().countDelegationDepth(ctx, child(persistence.TaskCreationSourceDelegation))
		if err != nil {
			t.Fatalf("a missing ancestor must end the walk, not fail it: %v", err)
		}
		if got != 1 {
			t.Errorf("depth = %d, want 1 (the parent itself)", got)
		}
	})

	t.Run("ancestorIDs", func(t *testing.T) {
		seen, err := newExec().ancestorIDs(ctx, child(persistence.TaskCreationSourceDelegation))
		if err != nil {
			t.Fatalf("a missing ancestor must end the walk, not fail the cycle check: %v", err)
		}
		if _, ok := seen["child"]; !ok {
			t.Errorf("the starting task must be in the lineage set, got %v", seen)
		}
	})
}

// A REAL failure — connection lost, context cancelled — must still be fatal.
// Reading it as "the lineage ends here" would silently under-count a depth
// guard, which is the failure mode the depth guard exists to prevent.
func TestAncestorOrEnd_KeepsARealFailureFatal(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.err = context.Canceled
	e := &Executor{taskRepo: repo, logger: zerolog.Nop(), config: DefaultConfig()}

	if _, err := e.ancestorOrEnd(context.Background(), "anything"); err == nil {
		t.Fatal("a transport failure must not be mistaken for an absent ancestor")
	}
}
