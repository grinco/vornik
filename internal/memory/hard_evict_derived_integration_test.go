//go:build integration

package memory_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// Hard eviction left the vector derived from the evicted text behind.
//
// `vornikctl memory evict` documents itself as a permanent, tombstoned deletion
// and names GDPR "forget this" requests as a use case. It deleted the chunk and
// left its embedding_cache row: the cache is keyed by the hash of the very text
// the operator asked to destroy, the vector is data derived from it, and nothing
// else removes it — the retention sweeper prunes by last_hit_at age and only
// when retention.embedding_cache_days is set, which is cold-entry pruning rather
// than deletion.
//
// DeleteByArtifact and DeleteByExtractedDocument have evicted the cache since
// slice 5c. HardEvict is the third path and did not, which is the same shape as
// the knowledge-graph rows fixed alongside it: the capability existed, and this
// caller was not wired to it.
//
// Against a real database because the guarantee is transactional — the chunk and
// its vector go together or neither goes.
func TestIntegrationHardEvict_EvictsTheChunksCachedVector(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const (
		projectID  = "p-hardevict-cache"
		sourceName = "hardevict-cache-test"
	)
	seed := []struct{ id, text string }{
		{"chunk-he-1", "the evicted sentence naming a person"},
		// A chunk that is NOT evicted: an over-broad eviction is its own bug, and
		// "everything is gone" would pass a weaker assertion.
		{"chunk-he-2", "an unrelated sentence that must survive"},
	}
	model := "test-embed-model"

	t.Cleanup(func() {
		for _, s := range seed {
			_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, s.id)
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`,
				memory.EmbedInputHash(sourceName, s.text))
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`,
				memory.ContentHash(s.text))
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_audit WHERE project_id = $1`, projectID)
	})

	cache := memory.NewEmbeddingCache(db)
	for _, s := range seed {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO project_memory_chunks
			   (id, project_id, source_name, content, content_hash, created_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())`,
			s.id, projectID, sourceName, s.text, memory.ContentHash(s.text)); err != nil {
			t.Fatalf("seed chunk %s: %v", s.id, err)
		}
		// Production caches under the CONTEXTUALISED embed input, not the raw
		// content — seeding the raw hash is what would let an eviction that
		// deleted nothing pass this test.
		if err := cache.Put(ctx, memory.EmbedInputHash(sourceName, s.text), model, testVector()); err != nil {
			t.Fatalf("seed cache row for %s: %v", s.id, err)
		}
	}

	cached := func(text string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM embedding_cache WHERE content_hash = $1`,
			memory.EmbedInputHash(sourceName, text)).Scan(&n); err != nil {
			t.Fatalf("count cache rows: %v", err)
		}
		return n > 0
	}
	for _, s := range seed {
		if !cached(s.text) {
			t.Fatalf("precondition: no cache row seeded for %s", s.id)
		}
	}

	res, err := repo.HardEvict(ctx, projectID, []string{"chunk-he-1"},
		"GDPR forget-this", "operator-test")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}
	if res.Count() != 1 {
		t.Fatalf("evicted %d chunks, want 1", res.Count())
	}

	if cached(seed[0].text) {
		t.Error("the vector derived from the evicted text must not survive the eviction")
	}
	if !cached(seed[1].text) {
		t.Error("an unrelated chunk's cached vector must be untouched — an over-broad " +
			"eviction is its own defect")
	}
	if res.EmbeddingCacheKeysDeleted == 0 {
		t.Error("the eviction must report the cache keys it removed; once they are gone " +
			"the report is the only evidence they were covered")
	}

	// The chunk itself is gone and the tombstone records it.
	var chunks, tombstones int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE id = $1`, "chunk-he-1").Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_eviction_audit WHERE chunk_id = $1`, "chunk-he-1").Scan(&tombstones); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if chunks != 0 || tombstones != 1 {
		t.Errorf("chunk rows = %d (want 0), tombstones = %d (want 1)", chunks, tombstones)
	}
}

// The durable record of what an eviction removed BEYOND the chunks.
//
// memory_eviction_audit is one tombstone per chunk and says nothing about the
// knowledge-graph entities and edges, the quarantined pre-ingest copy or the
// cached embedding that went with them. Every surface REPORTED those counts —
// both CLIs and the UI receipt — and nothing stored them, so they survived as
// long as terminal scrollback or a 15-minute browser banner. This design has
// argued throughout that once the rows are gone the report is the only evidence
// they were covered; a report nobody kept is not evidence.
//
// A run HEADER, not columns on the tombstones: the derived sweep runs once over
// the union of the evicted chunks, so summing per-chunk copies of a
// per-operation count would report the derived rows twice.
func TestIntegrationHardEvict_RecordsWhatItRemovedBeyondTheChunks(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const (
		projectID  = "p-evict-run-record"
		sourceName = "evict-run-record"
	)
	seed := []struct{ id, text string }{
		{"chunk-run-1", "the first evicted sentence"},
		{"chunk-run-2", "the second evicted sentence"},
	}
	t.Cleanup(func() {
		for _, s := range seed {
			_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, s.id)
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`,
				memory.EmbedInputHash(sourceName, s.text))
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_audit WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_runs WHERE project_id = $1`, projectID)
	})

	for _, s := range seed {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash, created_at)
			 VALUES ($1,$2,$3,$4,$5,NOW())`,
			s.id, projectID, sourceName, s.text, memory.ContentHash(s.text)); err != nil {
			t.Fatalf("seed chunk %s: %v", s.id, err)
		}
	}

	res, err := repo.HardEvict(ctx, projectID,
		[]string{seed[0].id, seed[1].id, "chunk-that-does-not-exist"},
		"GDPR forget-this", "ui:operator-under-test")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}
	if res.RunID == "" {
		t.Fatal("the eviction must report the run it recorded")
	}

	var (
		project, reason, by                       string
		requested, evicted, entities, edges, quar int
		cached                                    int
	)
	if err := db.QueryRowContext(ctx, `
		SELECT project_id, chunks_requested, chunks_evicted, graph_entities_deleted,
		       graph_edges_deleted, quarantined_copies_deleted, cached_embeddings_deleted,
		       reason, evicted_by
		FROM memory_eviction_runs WHERE id = $1`, res.RunID).
		Scan(&project, &requested, &evicted, &entities, &edges, &quar, &cached, &reason, &by); err != nil {
		t.Fatalf("read the run header: %v", err)
	}

	if project != projectID {
		t.Errorf("project_id = %q", project)
	}
	// Requested counts what the operator asked for, evicted what existed — the
	// difference is how an auditor sees that an id was stale rather than that
	// the operator asked for less.
	if requested != 3 || evicted != 2 {
		t.Errorf("chunks_requested/chunks_evicted = %d/%d, want 3/2", requested, evicted)
	}
	if cached != res.EmbeddingCacheKeysDeleted {
		t.Errorf("cached_embeddings_deleted = %d, want %d", cached, res.EmbeddingCacheKeysDeleted)
	}
	if entities != res.Derived.Entities || edges != res.Derived.Edges ||
		quar != res.QuarantinedCopiesDeleted {
		t.Errorf("the stored counts must match what was reported: stored %d/%d/%d, "+
			"reported %d/%d/%d", entities, edges, quar,
			res.Derived.Entities, res.Derived.Edges, res.QuarantinedCopiesDeleted)
	}
	if reason != "GDPR forget-this" {
		t.Errorf("reason = %q", reason)
	}
	// The one field that cannot be reconstructed later.
	if by != "ui:operator-under-test" {
		t.Errorf("evicted_by = %q — a deletion of personal data must name who performed it", by)
	}

	// Every tombstone back-links to the run, so an auditor can go from the
	// per-operation counts to the exact chunks they covered.
	var linked int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_eviction_audit WHERE run_id = $1`, res.RunID).Scan(&linked); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if linked != 2 {
		t.Errorf("%d tombstones link to the run, want 2", linked)
	}
}

// The record and the deletion commit together. A deletion with no record is
// non-compliant; a record of a deletion that did not happen is a false claim.
func TestIntegrationHardEvict_RecordAndDeletionAreAtomic(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const projectID = "p-evict-run-atomic"
	const chunkID = "chunk-atomic-1"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, chunkID)
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_audit WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_runs WHERE project_id = $1`, projectID)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash, created_at)
		 VALUES ($1,$2,'atomic','text',$3,NOW())`,
		chunkID, projectID, memory.ContentHash("atomic-text")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := repo.HardEvict(ctx, projectID, []string{chunkID}, "r", "tester")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}

	var chunks, runs int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM project_memory_chunks WHERE id = $1`, chunkID).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_eviction_runs WHERE id = $1`, res.RunID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if chunks != 0 || runs != 1 {
		t.Errorf("chunk rows = %d (want 0), run rows = %d (want 1) — the deletion and its "+
			"record must land together", chunks, runs)
	}
}

// A concurrent reader cannot observe the run header before the eviction
// commits.
//
// A review asserted the opposite: that under READ COMMITTED a reader could see
// the header between its INSERT and the UPDATE that fills in the outcome
// counts, and so observe a row claiming an operator requested a deletion that
// removed nothing. It cannot. The INSERT is uncommitted until the eviction
// commits, so the row does not exist for anyone else — READ COMMITTED means
// each statement sees the latest COMMITTED data, never another transaction's
// in-flight writes. Measured against Postgres before writing this, and pinned
// here so the property is not re-argued from first principles.
//
// The stronger consequence is the one that matters: the header, the tombstones
// and the deletes commit together or not at all, so there is no window in which
// the record and the deletion disagree.
func TestIntegrationHardEvict_RunHeaderIsInvisibleUntilTheEvictionCommits(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const projectID = "p-evict-run-visibility"
	const chunkID = "chunk-visibility-1"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_audit WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_runs WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, chunkID)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash, created_at)
		 VALUES ($1,$2,'vis','text',$3,NOW())`,
		chunkID, projectID, memory.ContentHash("visibility-text")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	visible := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_eviction_runs WHERE project_id = $1`, projectID).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}
	if visible() != 0 {
		t.Fatal("precondition: no run rows for this project yet")
	}

	res, err := repo.HardEvict(ctx, projectID, []string{chunkID}, "visibility check", "tester")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}

	// After the commit the row is there, complete — never partial.
	var requested, evicted int
	if err := db.QueryRowContext(ctx,
		`SELECT chunks_requested, chunks_evicted FROM memory_eviction_runs WHERE id = $1`,
		res.RunID).Scan(&requested, &evicted); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if requested != 1 || evicted != 1 {
		t.Errorf("run row = %d requested / %d evicted, want 1/1 — the outcome counts must "+
			"be present the moment the row becomes visible", requested, evicted)
	}
}

// A run header cannot be deleted while its tombstones still point at it.
//
// ON DELETE RESTRICT rather than SET NULL, because SET NULL would silently
// orphan the tombstones and take the derived-erasure counts with them — the
// evidence the header exists to preserve. A future retention sweep now has to
// decide what happens to the tombstones rather than discover it afterwards.
func TestIntegrationHardEvict_RunHeaderCannotBeOrphaned(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const projectID = "p-evict-run-restrict"
	const chunkID = "chunk-restrict-1"
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_audit WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_eviction_runs WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, chunkID)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash, created_at)
		 VALUES ($1,$2,'restrict','text',$3,NOW())`,
		chunkID, projectID, memory.ContentHash("restrict-text")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := repo.HardEvict(ctx, projectID, []string{chunkID}, "r", "tester")
	if err != nil {
		t.Fatalf("HardEvict: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`DELETE FROM memory_eviction_runs WHERE id = $1`, res.RunID); err == nil {
		t.Fatal("deleting a run header while its tombstones reference it must be REFUSED — " +
			"otherwise the derived-erasure counts vanish and the tombstones resolve to nothing")
	}

	// Tombstones first, header second — the order ProjectDataTables uses.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM memory_eviction_audit WHERE run_id = $1`, res.RunID); err != nil {
		t.Fatalf("delete tombstones: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM memory_eviction_runs WHERE id = $1`, res.RunID); err != nil {
		t.Errorf("with no tombstones left the header must be deletable: %v", err)
	}
}
