package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestClusterNodeRepository_UpsertListDelete exercises the full
// cluster_nodes repo against an in-memory SQLite instance.
//
// Test sequence:
//  1. Upsert a node → List returns it. last_seen is DB-stamped (~now).
//  2. Upsert again with a bumped version → updates in place (List still
//     len 1, fields advanced — no duplicate row). last_seen is re-stamped.
//  3. DeleteByInstanceID → List is empty.
func TestClusterNodeRepository_UpsertListDelete(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewClusterNodeRepository(db.DB)
	ctx := context.Background()

	before := time.Now().UTC().Add(-2 * time.Second) // lower bound for DB-stamped last_seen

	node := &persistence.ClusterNode{
		InstanceID:   "sqlite-test-node:4000:boot1",
		Profile:      "worker",
		Version:      "v2026.6.0-sqlite",
		Address:      "localhost:4000",
		Capabilities: map[string]bool{"RunWorkers": true, "ServeAPI": false},
		// LastSeen intentionally left zero — the DB stamps it.
	}

	// 1. First upsert — should insert.
	if err := repo.Upsert(ctx, node); err != nil {
		t.Fatalf("Upsert (insert): %v", err)
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after insert: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List len = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.InstanceID != node.InstanceID {
		t.Errorf("InstanceID = %q, want %q", got.InstanceID, node.InstanceID)
	}
	if got.Profile != "worker" || got.Version != "v2026.6.0-sqlite" {
		t.Errorf("unexpected profile/version: %+v", got)
	}
	if !got.Capabilities["RunWorkers"] || got.Capabilities["ServeAPI"] {
		t.Errorf("capabilities round-trip failed: %v", got.Capabilities)
	}
	// DB-stamped: must be non-zero and recent (after our before marker).
	if got.LastSeen.IsZero() {
		t.Error("last_seen must not be zero after upsert")
	}
	if got.LastSeen.Before(before) {
		t.Errorf("last_seen %v is older than test start %v — DB clock not used", got.LastSeen, before)
	}

	// 2. Second upsert with a bumped version — last_seen re-stamped by DB.
	node.Version = "v2026.6.1-sqlite"
	if err := repo.Upsert(ctx, node); err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}

	rows2, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after update: %v", err)
	}
	if len(rows2) != 1 {
		t.Fatalf("List len = %d after update, want 1 (no duplicate)", len(rows2))
	}
	got2 := rows2[0]
	if got2.Version != "v2026.6.1-sqlite" {
		t.Errorf("version not updated: got %q", got2.Version)
	}
	if got2.LastSeen.IsZero() {
		t.Error("last_seen must not be zero after second upsert")
	}

	// 3. Delete — List should be empty.
	if err := repo.DeleteByInstanceID(ctx, node.InstanceID); err != nil {
		t.Fatalf("DeleteByInstanceID: %v", err)
	}

	rows3, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(rows3) != 0 {
		t.Fatalf("List len = %d after delete, want 0", len(rows3))
	}
}

// TestClusterNodeRepository_DeleteStale verifies that DeleteStale removes only
// stale-unprotected rows, leaves fresh rows alone, and leaves stale-but-protected
// rows alone. Returns the count of deleted rows.
//
// Seed strategy:
//   - freshRow: just upserted → last_seen ≈ NOW() → should survive.
//   - staleUnprotected: upserted then backdated via direct SQL → should be deleted.
//   - staleProtected: upserted then backdated, but in the protected list → should survive.
func TestClusterNodeRepository_DeleteStale(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewClusterNodeRepository(db.DB)
	ctx := context.Background()

	makeNode := func(id string) *persistence.ClusterNode {
		return &persistence.ClusterNode{
			InstanceID:   id,
			Profile:      "worker",
			Version:      "v1",
			Address:      id,
			Capabilities: map[string]bool{},
		}
	}

	// Insert all three nodes via Upsert (DB stamps last_seen = NOW()).
	if err := repo.Upsert(ctx, makeNode("fresh-node")); err != nil {
		t.Fatalf("Upsert fresh-node: %v", err)
	}
	if err := repo.Upsert(ctx, makeNode("stale-unprotected")); err != nil {
		t.Fatalf("Upsert stale-unprotected: %v", err)
	}
	if err := repo.Upsert(ctx, makeNode("stale-protected")); err != nil {
		t.Fatalf("Upsert stale-protected: %v", err)
	}

	// Backdate the two stale rows to 1 hour ago.
	staleTime := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	for _, id := range []string{"stale-unprotected", "stale-protected"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE cluster_nodes SET last_seen = ? WHERE instance_id = ?`, staleTime, id,
		); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	// DeleteStale with 5-minute grace, protecting "stale-protected".
	grace := 5 * time.Minute
	n, err := repo.DeleteStale(ctx, grace, []string{"stale-protected"})
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteStale returned %d, want 1 (only stale-unprotected)", n)
	}

	// Verify surviving rows.
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after DeleteStale: %v", err)
	}
	surviving := make(map[string]bool)
	for _, r := range rows {
		surviving[r.InstanceID] = true
	}
	if !surviving["fresh-node"] {
		t.Error("fresh-node should survive DeleteStale")
	}
	if surviving["stale-unprotected"] {
		t.Error("stale-unprotected should have been deleted")
	}
	if !surviving["stale-protected"] {
		t.Error("stale-protected should survive (in protected list)")
	}
}

// TestClusterNodeRepository_Upsert_Nil: guard against nil node.
func TestClusterNodeRepository_Upsert_Nil(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewClusterNodeRepository(db.DB)
	if err := repo.Upsert(context.Background(), nil); err == nil {
		t.Error("Upsert(nil) should return error")
	}
}
