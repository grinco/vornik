//go:build integration

package memory_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// The artifact-erasure cascade must not leave a vector derived from erased text.
//
// Slice 5c §4.4 of the Art 17 design says every erasure path evicts the deleted
// chunk's embedding_cache row: the cache is keyed by the hash of the very text we
// were asked to erase, the vector is data DERIVED from it, and nothing else removes
// it — the retention sweeper prunes by last_hit_at age and only when
// retention.embedding_cache_days is set, which is cold-entry pruning rather than
// compliance with an erasure request.
//
// The retro-fit landed on DataSubjectRepository.DeleteRow, on the 5c redaction path,
// and on Repository.DeleteByArtifact — but NOT on DeleteByExtractedDocument, which is
// the cascade's FIRST call (internal/erasure/erasure.go:216, once per extracted
// document) and the majority path for anything ingested through document or video
// extraction. So erasing a document left every derived chunk's vector behind while
// reporting the erasure complete.
//
// Against a real database because the guarantee is transactional: the chunk and its
// vector must go together or neither goes.
//
// Design: https://docs.vornik.io §4.4
func TestIntegrationDeleteByExtractedDocument_EvictsDerivedChunkVectors(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const (
		projectID = "p-erasure-cache-extdoc"
		docID     = "extdoc-erasure-cache-1"
		otherDoc  = "extdoc-erasure-cache-2"
	)
	// Two chunks derived from the erased extraction, one from a different
	// extraction that must survive untouched — an over-broad eviction is its own
	// bug, and "everything is gone" would pass a weaker assertion.
	seed := []struct{ id, doc, text string }{
		{"chunk-ec-1", docID, "the subject's phone number is in this sentence"},
		{"chunk-ec-2", docID, "a second sentence about the same subject"},
		{"chunk-ec-3", otherDoc, "an unrelated sentence from another document"},
	}
	model := "test-embed-model"
	// A non-empty source name is the normal case (the column is NOT NULL and every
	// real ingest sets it), and it is what makes the embed input differ from the raw
	// content — i.e. what makes the cache key differ from content_hash.
	const sourceName = "erasure-cache-test"

	t.Cleanup(func() {
		for _, s := range seed {
			_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, s.id)
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, memory.EmbedInputHash(sourceName, s.text))
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, memory.ContentHash(s.text))
		}
	})

	cache := memory.NewEmbeddingCache(db)
	for _, s := range seed {
		hash := memory.ContentHash(s.text)
		// Production caches under the CONTEXTUALISED embed input, not the raw content
		// (memory.EmbedInputHash) — seeding the raw hash is what let an eviction that
		// deleted nothing pass this test.
		cacheKey := memory.EmbedInputHash(sourceName, s.text)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO project_memory_chunks
			   (id, project_id, source_name, content, content_hash, derived_from_extracted_document_id, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
			s.id, projectID, sourceName, s.text, hash, s.doc); err != nil {
			t.Fatalf("seed chunk %s: %v", s.id, err)
		}
		if err := cache.Put(ctx, cacheKey, model, testVector()); err != nil {
			t.Fatalf("seed cache row for %s: %v", s.id, err)
		}
	}

	cached := func(text string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM embedding_cache WHERE content_hash = $1`, memory.EmbedInputHash(sourceName, text),
		).Scan(&n); err != nil {
			t.Fatalf("count cache rows: %v", err)
		}
		return n > 0
	}
	// The seed itself must be visible, or the assertions below prove nothing.
	for _, s := range seed {
		if !cached(s.text) {
			t.Fatalf("precondition: no cache row seeded for %s", s.id)
		}
	}

	n, err := repo.DeleteByExtractedDocument(ctx, docID)
	if err != nil {
		t.Fatalf("DeleteByExtractedDocument: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d chunks, want 2", n)
	}

	for _, s := range seed[:2] {
		if cached(s.text) {
			t.Errorf("chunk %s was erased but its embedding_cache row survives — "+
				"a vector derived from erased text is still held", s.id)
		}
	}
	if !cached(seed[2].text) {
		t.Error("evicted the cache row of a chunk from a DIFFERENT extraction; " +
			"the eviction must be scoped to what was actually deleted")
	}

	// Idempotent: erasure paths retry, and "already gone" is the expected
	// second-run state rather than an error.
	if _, err := repo.DeleteByExtractedDocument(ctx, docID); err != nil {
		t.Errorf("second DeleteByExtractedDocument must be a no-op, got %v", err)
	}
}

// TestIntegrationDeleteByExtractedDocument_EvictsAHashSharedAcrossProjects pins the
// documented trade-off rather than discovering it later.
//
// embedding_cache is keyed (content_hash, model) with NO project column, while
// project_memory_chunks has a UNIQUE (project_id, content_hash) — so identical text
// cannot collide inside one project, only across projects, and then the two chunks
// share ONE cache row. Erasing one project's copy therefore evicts a row the other
// project's chunk would have hit. That is deliberate and cheap: the cost is one
// re-embed on the next miss, against retaining a vector derived from erased text.
// The surviving project keeps its chunk — only the derived vector goes.
func TestIntegrationDeleteByExtractedDocument_EvictsAHashSharedAcrossProjects(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const (
		erasedProject  = "p-erasure-cache-shared-a"
		survivingProj  = "p-erasure-cache-shared-b"
		erasedDoc      = "extdoc-erasure-cache-shared-a"
		survivingDoc   = "extdoc-erasure-cache-shared-b"
		text           = "the same sentence was ingested into two projects"
		erasedChunkID  = "chunk-ec-sh-a"
		survivingChunk = "chunk-ec-sh-b"
	)
	hash := memory.ContentHash(text)
	// Both projects ingest under the same source name, so both derive the SAME embed
	// input and share one cache row — the shape this test is about.
	const sourceName = "erasure-cache-test"
	cacheKey := memory.EmbedInputHash(sourceName, text)

	t.Cleanup(func() {
		for _, id := range []string{erasedChunkID, survivingChunk} {
			_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, id)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, cacheKey)
	})

	for _, row := range []struct{ id, project, doc string }{
		{erasedChunkID, erasedProject, erasedDoc},
		{survivingChunk, survivingProj, survivingDoc},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO project_memory_chunks
			   (id, project_id, source_name, content, content_hash, derived_from_extracted_document_id, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, NOW())`,
			row.id, row.project, sourceName, text, hash, row.doc); err != nil {
			t.Fatalf("seed chunk %s: %v", row.id, err)
		}
	}
	if err := memory.NewEmbeddingCache(db).Put(ctx, cacheKey, "test-embed-model", testVector()); err != nil {
		t.Fatalf("seed cache row: %v", err)
	}

	if _, err := repo.DeleteByExtractedDocument(ctx, erasedDoc); err != nil {
		t.Fatalf("DeleteByExtractedDocument: %v", err)
	}

	var cacheRows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embedding_cache WHERE content_hash = $1`, cacheKey).Scan(&cacheRows); err != nil {
		t.Fatalf("count cache rows: %v", err)
	}
	if cacheRows != 0 {
		t.Errorf("cache rows for the shared hash = %d, want 0 — the vector derived from the erased text is still held", cacheRows)
	}

	var survivors int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE id = $1`, survivingChunk).Scan(&survivors); err != nil {
		t.Fatalf("count surviving chunk: %v", err)
	}
	if survivors != 1 {
		t.Errorf("the other project's CHUNK was deleted (%d rows) — only its derived vector should go", survivors)
	}
}

// testVector is a 1024-dim vector — the embedding_cache column is fixed at the
// default local embedder's dimension (bge-m3), so a short slice is rejected by
// pgvector rather than stored.
func testVector() []float32 {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = 0.01
	}
	return v
}

// TestIntegrationDeleteByExtractedDocument_EvictsTheRecordedKeyNotARecomputedOne is
// the reason embed_input_hash (migration 150) exists.
//
// Before it, erasure found the cache row by RECOMPUTING the contextualisation prefix,
// which silently assumes buildEmbedContext is byte-stable for the life of the
// deployment. Change the prefix format — a new label, a different separator, a
// heading rule — and every vector cached under the old format becomes unreachable by
// erasure while still sitting in the table.
//
// This test models exactly that: the chunk's recorded embed_input_hash does NOT match
// what today's prefix would produce (as if it were embedded under an older format),
// and the cache row exists under the RECORDED key. Erasure must still evict it.
func TestIntegrationDeleteByExtractedDocument_EvictsTheRecordedKeyNotARecomputedOne(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()
	repo := memory.NewRepository(db)

	const (
		projectID = "p-erasure-cache-recorded-key"
		docID     = "extdoc-erasure-cache-recorded"
		chunkID   = "chunk-ec-recorded"
		source    = "erasure-cache-test"
		text      = "content embedded back when the prefix format was different"
	)
	// A key today's code would never derive for this row: neither the raw content
	// hash nor the current embed-input hash.
	legacyKey := memory.ContentHash("LEGACY PREFIX v0\n\n" + text)
	if legacyKey == memory.EmbedInputHash(source, text) || legacyKey == memory.ContentHash(text) {
		t.Fatal("fixture is not modelling a format change")
	}

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, chunkID)
		_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, legacyKey)
	})

	if _, err := db.ExecContext(ctx,
		`INSERT INTO project_memory_chunks
		   (id, project_id, source_name, content, content_hash, embed_input_hash,
		    derived_from_extracted_document_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
		chunkID, projectID, source, text, memory.ContentHash(text), legacyKey, docID); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	if err := memory.NewEmbeddingCache(db).Put(ctx, legacyKey, "test-embed-model", testVector()); err != nil {
		t.Fatalf("seed cache row under the recorded key: %v", err)
	}

	if _, err := repo.DeleteByExtractedDocument(ctx, docID); err != nil {
		t.Fatalf("DeleteByExtractedDocument: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embedding_cache WHERE content_hash = $1`, legacyKey).Scan(&n); err != nil {
		t.Fatalf("count cache rows: %v", err)
	}
	if n != 0 {
		t.Error("the chunk recorded which cache key its vector was stored under, and erasure " +
			"ignored it in favour of a recomputed one — a prefix-format change now strands " +
			"vectors derived from erased text")
	}
}
