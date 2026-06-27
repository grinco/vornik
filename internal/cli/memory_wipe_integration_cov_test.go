//go:build integration

package cli

// Integration coverage for `vornikctl memory wipe` (memory_wipe.go).
// Seeds the full set of project-scoped tables, then exercises the
// count/preview path, the keep-* flags, the stdin-confirmation gate,
// and the real transactional delete.

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

func memoryWipe_reset() {
	memoryWipeProject = ""
	memoryWipeDryRun, memoryWipeYes = false, false
	memoryWipeKeepQuarantine, memoryWipeKeepAudit, memoryWipeKeepGraph = false, false, false
}

// wipeSeedAll populates one row in each table memory wipe touches so
// every count + delete pass has something to act on.
func wipeSeedAll(t *testing.T, db *sql.DB, proj string) (chunkID, entID string) {
	t.Helper()
	chunkID = dbcovSeedChunk(t, db, proj, "wipe-src", "wipe-content", "")

	entID = "ent-" + proj
	entID2 := "ent2-" + proj
	for _, e := range []struct{ id, name string }{{entID, "Thing"}, {entID2, "Other"}} {
		if _, err := db.Exec(`
			INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
			VALUES ($1, $2, 'concept', $3)`, e.id, proj, e.name); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
	}
	// from != to: knowledge_edges has a no-self-loop check constraint.
	if _, err := db.Exec(`
		INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		VALUES ($1, $2, $3, $4, 'relates_to', ARRAY[$5]::text[])`,
		"edge-"+proj, proj, entID, entID2, chunkID); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO project_memory_quarantine (id, project_id, content, content_hash, failed_gate)
		VALUES ($1, $2, 'bad', $3, 'prompt_injection')`,
		"q-"+proj, proj, "qhash-"+proj); err != nil {
		t.Fatalf("seed quarantine: %v", err)
	}
	// project_ingest_queue.source_artifact_id FKs to artifacts; seed a
	// parent artifact first. Wipe does NOT touch artifacts, so clean it
	// up explicitly.
	artID := "art-" + proj
	if _, err := db.Exec(`
		INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path)
		VALUES ($1, $2, 'a', 'INPUT', '/tmp/a')`, artID, proj); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM artifacts WHERE id=$1`, artID) })
	if _, err := db.Exec(`
		INSERT INTO project_ingest_queue (id, project_id, source_artifact_id, producer_role)
		VALUES ($1, $2, $3, 'researcher')`, "iq-"+proj, proj, artID); err != nil {
		t.Fatalf("seed ingest_queue: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO corpus_epochs (id, project_id) VALUES ($1, $2)`,
		"epoch-"+proj, proj); err != nil {
		t.Fatalf("seed corpus_epochs: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO memory_retrieval_audit (id, project_id, query) VALUES ($1, $2, 'q')`,
		"audit-"+proj, proj); err != nil {
		t.Fatalf("seed retrieval_audit: %v", err)
	}
	return chunkID, entID
}

func TestIntegration_MemoryWipe_DryRunCountsNoDelete(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-dry")
	dbcovCleanupProject(t, db, proj)
	wipeSeedAll(t, db, proj)

	memoryWipeProject = proj
	memoryWipeDryRun = true
	out, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) })
	if err != nil {
		t.Fatalf("wipe dry-run: %v", err)
	}
	for _, want := range []string{"Memory wipe plan", "chunks", "kg_entities", "dry-run: no changes made"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, proj).Scan(&n)
	if n != 1 {
		t.Errorf("dry-run deleted rows: chunks=%d, want 1", n)
	}
}

func TestIntegration_MemoryWipe_KeepFlagsShowKept(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-keep")
	dbcovCleanupProject(t, db, proj)
	wipeSeedAll(t, db, proj)

	memoryWipeProject = proj
	memoryWipeDryRun = true
	memoryWipeKeepGraph = true
	memoryWipeKeepQuarantine = true
	memoryWipeKeepAudit = true
	out, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) })
	if err != nil {
		t.Fatalf("wipe keep: %v", err)
	}
	if strings.Count(out, "(kept by flag)") < 3 {
		t.Errorf("expected >=3 kept-by-flag lines:\n%s", out)
	}
}

func TestIntegration_MemoryWipe_NothingToDelete(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-none")
	dbcovCleanupProject(t, db, proj)

	memoryWipeProject = proj
	out, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) })
	if err != nil {
		t.Fatalf("wipe empty: %v", err)
	}
	if !strings.Contains(out, "nothing to delete") {
		t.Errorf("expected nothing-to-delete:\n%s", out)
	}
}

func TestIntegration_MemoryWipe_ConfirmationMismatchAborts(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-confirm")
	dbcovCleanupProject(t, db, proj)
	wipeSeedAll(t, db, proj)

	// Feed a wrong confirmation token on stdin.
	r, w, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	_, _ = w.WriteString("WRONG\n")
	_ = w.Close()

	memoryWipeProject = proj
	_, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) })
	_ = r.Close()
	if err == nil || !strings.Contains(err.Error(), "confirmation mismatch") {
		t.Fatalf("expected confirmation-mismatch abort, got %v", err)
	}
	// Nothing deleted on abort.
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, proj).Scan(&n)
	if n != 1 {
		t.Errorf("abort still deleted rows: chunks=%d, want 1", n)
	}
}

func TestIntegration_MemoryWipe_FullDeleteWithYes(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-full")
	dbcovCleanupProject(t, db, proj)
	chunkID, entID := wipeSeedAll(t, db, proj)
	// entity_mention bridges entity↔chunk; it should cascade away.
	if _, err := db.Exec(`INSERT INTO entity_mentions (chunk_id, entity_id) VALUES ($1, $2)`, chunkID, entID); err != nil {
		t.Fatalf("seed mention: %v", err)
	}

	memoryWipeProject = proj
	memoryWipeYes = true
	out, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) })
	if err != nil {
		t.Fatalf("wipe full: %v", err)
	}
	if !strings.Contains(out, "memory wiped") {
		t.Fatalf("expected wiped confirmation:\n%s", out)
	}
	for _, tbl := range []string{
		"project_memory_chunks", "knowledge_entities", "knowledge_edges",
		"project_memory_quarantine", "project_ingest_queue", "corpus_epochs",
		"memory_retrieval_audit",
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE project_id=$1`, proj).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("%s not wiped: %d rows remain", tbl, n)
		}
	}
	// entity_mentions cascaded (no project_id column → check by chunk).
	var mN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM entity_mentions WHERE chunk_id=$1`, chunkID).Scan(&mN)
	if mN != 0 {
		t.Errorf("entity_mentions not cascaded: %d", mN)
	}
}

func TestIntegration_MemoryWipe_InteractiveConfirmProceeds(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-confirm-ok")
	dbcovCleanupProject(t, db, proj)
	wipeSeedAll(t, db, proj)

	// Type the exact project name → confirmation passes (no --yes).
	r, w, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	_, _ = w.WriteString(proj + "\n")
	_ = w.Close()

	memoryWipeProject = proj
	out, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) })
	_ = r.Close()
	if err != nil {
		t.Fatalf("wipe confirm: %v", err)
	}
	if !strings.Contains(out, "memory wiped") {
		t.Fatalf("expected wipe to proceed after correct confirmation:\n%s", out)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, proj).Scan(&n)
	if n != 0 {
		t.Errorf("chunks not wiped after confirm: %d", n)
	}
}

func TestIntegration_MemoryWipe_KeepGraphPreservesEntities(t *testing.T) {
	db := dbcovSetup(t)
	memoryWipe_reset()
	proj := dbcovUniqueProject("wipe-keepgraph")
	dbcovCleanupProject(t, db, proj)
	wipeSeedAll(t, db, proj)

	memoryWipeProject = proj
	memoryWipeYes = true
	memoryWipeKeepGraph = true
	if _, err := dbcovCapture(t, func() error { return runMemoryWipe(memoryWipeCmd, nil) }); err != nil {
		t.Fatalf("wipe keep-graph: %v", err)
	}
	var ents, chunks int
	_ = db.QueryRow(`SELECT COUNT(*) FROM knowledge_entities WHERE project_id=$1`, proj).Scan(&ents)
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id=$1`, proj).Scan(&chunks)
	if ents != 2 {
		t.Errorf("--keep-graph deleted entities: %d, want 2", ents)
	}
	if chunks != 0 {
		t.Errorf("chunks should still be wiped: %d", chunks)
	}
}
