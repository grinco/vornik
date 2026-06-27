//go:build integration

package cli

// Integration coverage for `vornikctl memory reassign` (the DB path in
// memory.go: runMemoryReassign). Covers validation guards, dry-run,
// the collision-drop + transactional move, and the empty-source case.

import (
	"strings"
	"testing"
)

func memoryReassign_reset() {
	memoryReassignFrom, memoryReassignTo = "", ""
	memoryReassignDryRun = false
}

func TestIntegration_MemoryReassign_RequiresFromTo(t *testing.T) {
	dbcovSetup(t)
	memoryReassign_reset()
	err := runMemoryReassign(memoryReassignCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--from and --to are required") {
		t.Fatalf("expected required guard, got %v", err)
	}
}

func TestIntegration_MemoryReassign_FromToMustDiffer(t *testing.T) {
	dbcovSetup(t)
	memoryReassign_reset()
	memoryReassignFrom, memoryReassignTo = "same", "same"
	err := runMemoryReassign(memoryReassignCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("expected differ guard, got %v", err)
	}
}

func TestIntegration_MemoryReassign_DryRunCounts(t *testing.T) {
	db := dbcovSetup(t)
	memoryReassign_reset()
	from := dbcovUniqueProject("re-from")
	to := dbcovUniqueProject("re-to")
	dbcovCleanupProject(t, db, from)
	dbcovCleanupProject(t, db, to)
	dbcovSeedChunk(t, db, from, "s1", "alpha", "")
	dbcovSeedChunk(t, db, from, "s2", "beta", "")

	memoryReassignFrom, memoryReassignTo = from, to
	memoryReassignDryRun = true
	out, err := dbcovCapture(t, func() error { return runMemoryReassign(memoryReassignCmd, nil) })
	if err != nil {
		t.Fatalf("reassign dry-run: %v", err)
	}
	if !strings.Contains(out, "2 rows") || !strings.Contains(out, "dry-run: no changes made") {
		t.Fatalf("dry-run output wrong:\n%s", out)
	}
	// Source untouched.
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, from).Scan(&n)
	if n != 2 {
		t.Errorf("dry-run moved rows: source has %d, want 2", n)
	}
}

func TestIntegration_MemoryReassign_EmptySourceNothingToDo(t *testing.T) {
	db := dbcovSetup(t)
	memoryReassign_reset()
	from := dbcovUniqueProject("re-empty-from")
	to := dbcovUniqueProject("re-empty-to")
	dbcovCleanupProject(t, db, from)
	dbcovCleanupProject(t, db, to)

	memoryReassignFrom, memoryReassignTo = from, to
	out, err := dbcovCapture(t, func() error { return runMemoryReassign(memoryReassignCmd, nil) })
	if err != nil {
		t.Fatalf("reassign empty: %v", err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("expected nothing-to-do:\n%s", out)
	}
}

func TestIntegration_MemoryReassign_MovesAndDropsCollisions(t *testing.T) {
	db := dbcovSetup(t)
	memoryReassign_reset()
	from := dbcovUniqueProject("re-move-from")
	to := dbcovUniqueProject("re-move-to")
	dbcovCleanupProject(t, db, from)
	dbcovCleanupProject(t, db, to)

	// Two source chunks. One shares a content_hash with a dest chunk
	// (collision → dropped); the other is unique (moved).
	// dbcovSeedChunk derives content_hash from id+content, so to force
	// a collision we insert matching hashes manually.
	_, _ = db.Exec(`INSERT INTO project_memory_chunks (id, project_id, source_name, chunk_index, content, content_hash, created_at)
		VALUES ('s-dup','` + from + `','src','0','dup','SHARED-HASH', now())`)
	_, _ = db.Exec(`INSERT INTO project_memory_chunks (id, project_id, source_name, chunk_index, content, content_hash, created_at)
		VALUES ('d-dup','` + to + `','src','0','dup','SHARED-HASH', now())`)
	_, _ = db.Exec(`INSERT INTO project_memory_chunks (id, project_id, source_name, chunk_index, content, content_hash, created_at)
		VALUES ('s-uniq','` + from + `','src','0','uniq','UNIQ-HASH', now())`)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE id IN ('s-dup','d-dup','s-uniq')`)
	})

	memoryReassignFrom, memoryReassignTo = from, to
	out, err := dbcovCapture(t, func() error { return runMemoryReassign(memoryReassignCmd, nil) })
	if err != nil {
		t.Fatalf("reassign move: %v", err)
	}
	if !strings.Contains(out, "reassigned 1 rows") || !strings.Contains(out, "1 duplicates dropped") {
		t.Fatalf("expected 1 moved + 1 dropped:\n%s", out)
	}
	// Source now empty.
	var srcN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, from).Scan(&srcN)
	if srcN != 0 {
		t.Errorf("source not drained: %d", srcN)
	}
	// Dest has its original + the moved-unique = 2.
	var dstN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, to).Scan(&dstN)
	if dstN != 2 {
		t.Errorf("dest count = %d, want 2", dstN)
	}
}
