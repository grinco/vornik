package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestLeaseTask_DependencyGatingSerializesChain proves the topology
// lever the delegation engine relies on: a SEQUENTIAL delegation chain
// (child[i].Dependencies = [child[i-1].ID]) is released by LeaseTask one
// task at a time, while independent (PARALLEL / FAN_OUT) children with no
// dependencies are all immediately leasable.
//
// This is the persistence-layer half of the N2/R1 fix: the executor sets
// Dependencies based on parent.DelegationMode, and the lease query's
// NOT EXISTS-on-dependencies clause turns that into real serial vs.
// parallel scheduling. See https://docs.vornik.io §3.
func TestLeaseTask_DependencyGatingSerializesChain(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	// --- SEQUENTIAL chain: c1 ← c2 ← c3 ---
	c1 := &persistence.Task{ID: "seq-c1", ProjectID: "seqproj", Priority: 50}
	c2 := &persistence.Task{ID: "seq-c2", ProjectID: "seqproj", Priority: 50, Dependencies: []string{"seq-c1"}}
	c3 := &persistence.Task{ID: "seq-c3", ProjectID: "seqproj", Priority: 50, Dependencies: []string{"seq-c2"}}
	for _, c := range []*persistence.Task{c1, c2, c3} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.ID, err)
		}
	}

	// Only c1 (no unmet deps) is leasable; c2/c3 are gated behind it.
	got, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID: "seqproj", LeaseHolder: "w", LeaseDurationSeconds: 30,
	})
	if err != nil {
		t.Fatalf("lease c1: %v", err)
	}
	if got.ID != "seq-c1" {
		t.Fatalf("first lease = %q, want seq-c1 (only task with no unmet deps)", got.ID)
	}

	// c1 is now LEASED (not COMPLETED), so c2 is still gated → no work.
	if _, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID: "seqproj", LeaseHolder: "w", LeaseDurationSeconds: 30,
	}); err != persistence.ErrNoTasksAvailable {
		t.Fatalf("second lease while c1 in-flight: err=%v, want ErrNoTasksAvailable (chain gated)", err)
	}

	// Complete c1; now c2 becomes eligible, c3 still gated behind c2.
	if err := repo.ReleaseLease(ctx, "seq-c1", *got.LeaseID, persistence.TaskStatusCompleted, persistence.ReleaseOptions{}); err != nil {
		t.Fatalf("complete c1: %v", err)
	}
	got2, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID: "seqproj", LeaseHolder: "w", LeaseDurationSeconds: 30,
	})
	if err != nil {
		t.Fatalf("lease c2 after c1 done: %v", err)
	}
	if got2.ID != "seq-c2" {
		t.Fatalf("post-c1 lease = %q, want seq-c2", got2.ID)
	}

	// --- PARALLEL / FAN_OUT: independent children, all leasable now ---
	p1 := &persistence.Task{ID: "par-1", ProjectID: "parproj", Priority: 50}
	p2 := &persistence.Task{ID: "par-2", ProjectID: "parproj", Priority: 50}
	p3 := &persistence.Task{ID: "par-3", ProjectID: "parproj", Priority: 50}
	for _, c := range []*persistence.Task{p1, p2, p3} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.ID, err)
		}
	}
	leased := map[string]bool{}
	for i := 0; i < 3; i++ {
		g, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID: "parproj", LeaseHolder: "w", LeaseDurationSeconds: 30,
		})
		if err != nil {
			t.Fatalf("parallel lease %d: %v (no dependency should gate)", i, err)
		}
		leased[g.ID] = true
	}
	if len(leased) != 3 {
		t.Fatalf("parallel: leased %d distinct tasks, want 3 (all independent → all immediately leasable)", len(leased))
	}
}
