//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
)

// Slice 5c §4.4 retro-fit, against a real database because the guarantee is
// transactional and a fake cannot demonstrate it.
//
// A deleted memory chunk used to leave its embedding_cache row behind, keyed by the
// hash of the very text we were asked to erase. That vector is data derived from the
// subject's personal data, and nothing else removes it — the retention sweeper prunes
// by last_hit_at age and only when retention.embedding_cache_days is set, which is
// cold-entry pruning rather than compliance with an erasure request.
//
// This was a gap in code ALREADY SHIPPED by slice 5b, which is why it is tested
// against the delete path rather than only the redact path.
//
// Design: https://docs.vornik.io §4.4
func TestIntegrationDeleteRow_EvictsTheChunksEmbeddingCacheEntry(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := &DataSubjectRepository{db: db.DB}

	const projectID = "erasure-cache-evict-test"
	content := "Called the subject about their appointment; Peter also joined."
	hash := memory.ContentHash(content)

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, projectID)
		_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, hash)
	})

	// id is app-supplied text, not a sequence, so the seed provides it.
	const chunkID = "chunk-erasure-cache-evict-1"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO project_memory_chunks (id, project_id, content, content_hash, source_name)
		VALUES ($1, $2, $3, $4, 'integration-test')`,
		chunkID, projectID, content, hash); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	// Two models' vectors for the same text: erasing must remove BOTH, because an
	// operator may have re-embedded over the deployment's life and leaving an older
	// model's vector behind would defeat the erasure.
	for _, model := range []string{"text-embedding-3-small", "text-embedding-3-large"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO embedding_cache (content_hash, model, embedding)
			VALUES ($1, $2, $3)
			ON CONFLICT (content_hash, model) DO NOTHING`,
			hash, model, pgVectorLiteral(1024)); err != nil {
			t.Fatalf("seed cache row for %s: %v", model, err)
		}
	}

	var before int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM embedding_cache WHERE content_hash = $1`, hash).Scan(&before); err != nil {
		t.Fatalf("count cache rows: %v", err)
	}
	if before != 2 {
		t.Fatalf("precondition: expected 2 seeded cache rows, got %d", before)
	}

	if err := repo.DeleteRow(ctx, datasubject.TableProjectMemoryChunks, chunkID); err != nil {
		t.Fatalf("DeleteRow: %v", err)
	}

	var chunks int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM project_memory_chunks WHERE id = $1`, chunkID).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks != 0 {
		t.Errorf("the chunk itself must be gone, still found %d", chunks)
	}

	var after int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM embedding_cache WHERE content_hash = $1`, hash).Scan(&after); err != nil {
		t.Fatalf("count cache rows after: %v", err)
	}
	if after != 0 {
		t.Errorf("every model's cached vector for the erased text must be evicted, %d survived — "+
			"a surviving vector is data derived from the erased personal data", after)
	}
}

// Deleting a NON-chunk linkable row must not touch embedding_cache at all. The
// eviction is keyed off the chunk's own content_hash, so a table without one has
// nothing to evict and must not fall into the chunk branch.
func TestIntegrationDeleteRow_NonChunkTableLeavesTheCacheAlone(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := &DataSubjectRepository{db: db.DB}

	unrelated := memory.ContentHash("an unrelated chunk that must keep its vector")
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, unrelated)
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO embedding_cache (content_hash, model, embedding)
		VALUES ($1, 'text-embedding-3-small', $2)
		ON CONFLICT (content_hash, model) DO NOTHING`, unrelated, pgVectorLiteral(1024)); err != nil {
		t.Fatalf("seed cache row: %v", err)
	}

	// A row id that does not exist is fine — the delete is a no-op and the point of
	// the test is that the cache is untouched either way.
	_ = repo.DeleteRow(ctx, datasubject.TableOperatorProfile, "no-such-row")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM embedding_cache WHERE content_hash = $1`, unrelated).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("an unrelated cache entry must survive a non-chunk erasure, got %d", n)
	}
}

// pgVectorLiteral builds a zero vector of the given dimension in pgvector's text
// input format, so the seed does not depend on a real embedding provider.
func pgVectorLiteral(dim int) string {
	buf := make([]byte, 0, dim*2+2)
	buf = append(buf, '[')
	for i := 0; i < dim; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '0')
	}
	return string(append(buf, ']'))
}
