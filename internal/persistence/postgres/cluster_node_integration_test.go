//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegrationClusterNode_UpsertListDelete exercises the full
// cluster_nodes repo against a live PostgreSQL instance.
//
// Run: go test -tags=integration ./internal/persistence/postgres/...
//
// Test sequence:
//  1. Upsert a node → List returns it.
//  2. Upsert again with a new last_seen → updates in place (List still
//     len 1, last_seen advanced — no duplicate row).
//  3. DeleteByInstanceID → List returns empty.
func TestIntegrationClusterNode_UpsertListDelete(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := NewClusterNodeRepository(db.DB)

	const instanceID = "integration-test-node:8080:boot999"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE instance_id = $1`, instanceID)
	}
	cleanup()
	t.Cleanup(cleanup)

	t1 := time.Now().UTC().Truncate(time.Millisecond)
	node := &persistence.ClusterNode{
		InstanceID:   instanceID,
		Profile:      "worker",
		Version:      "v2026.6.0-test",
		Address:      "integration-test-node:8080",
		Capabilities: map[string]bool{"RunWorkers": true, "ServeAPI": false},
		LastSeen:     t1,
	}

	// 1. First upsert — should insert.
	if err := repo.Upsert(ctx, node); err != nil {
		t.Fatalf("Upsert (insert): %v", err)
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after insert: %v", err)
	}
	var found *persistence.ClusterNode
	for _, r := range rows {
		if r.InstanceID == instanceID {
			found = r
			break
		}
	}
	if found == nil {
		t.Fatal("inserted node not found in List")
	}
	if found.Profile != "worker" || found.Version != "v2026.6.0-test" {
		t.Errorf("unexpected profile/version: %+v", found)
	}
	if !found.Capabilities["RunWorkers"] {
		t.Errorf("capabilities round-trip failed: %v", found.Capabilities)
	}

	// 2. Second upsert with a later last_seen — must update in place.
	t2 := t1.Add(30 * time.Second)
	node.LastSeen = t2
	node.Version = "v2026.6.1-test" // also bump version to confirm update
	if err := repo.Upsert(ctx, node); err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}

	rows2, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after update: %v", err)
	}
	var count int
	var found2 *persistence.ClusterNode
	for _, r := range rows2 {
		if r.InstanceID == instanceID {
			count++
			found2 = r
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for instanceID after upsert, got %d", count)
	}
	if found2.Version != "v2026.6.1-test" {
		t.Errorf("version not updated: got %q", found2.Version)
	}
	// last_seen is DB-authoritative — Upsert stamps NOW() and ignores the
	// caller-supplied value (eb1515d9, clock-skew immunity). So the second
	// upsert re-stamps last_seen to a fresh DB-clock time, NOT to the caller's
	// t2. Assert it was re-stamped recently rather than equal to t2.
	if found2.LastSeen.Equal(t2) {
		t.Errorf("last_seen should be DB-stamped NOW(), not the caller-supplied t2 (%v)", t2)
	}
	if d := time.Since(found2.LastSeen); d < 0 || d > 5*time.Minute {
		t.Errorf("last_seen not re-stamped to a recent DB-clock value: got %v (%v ago)", found2.LastSeen, d)
	}

	// 3. Delete — List should no longer contain this node.
	if err := repo.DeleteByInstanceID(ctx, instanceID); err != nil {
		t.Fatalf("DeleteByInstanceID: %v", err)
	}

	rows3, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	for _, r := range rows3 {
		if r.InstanceID == instanceID {
			t.Fatalf("node still present after DeleteByInstanceID")
		}
	}
}
