//go:build integration

package memory_test

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

// Deleting a project left its cached embeddings behind.
//
// persistence.ProjectDataTables drives the project-wide wipe as
// `DELETE FROM <table> WHERE project_id = $1`, and it covers the memory graph
// correctly. embedding_cache is not on it and CANNOT be: it is keyed
// (content_hash, model) with no project_id column, so a project-scoped DELETE has
// nothing to match on. The vector computed from a project's text therefore
// survived the project — 78,176 cache rows in production when this was filed
// (2026-08-21).
//
// Every other deletion path already evicted (DeleteByArtifact,
// DeleteByExtractedDocument, the 5c redaction transaction, HardEvict), which is
// what made this one the outlier rather than the norm.
//
// Against a real database because the guarantee is transactional: the chunk and
// its vector go together or neither goes.
func TestIntegrationDeleteProjectData_EvictsTheProjectsCachedEmbeddings(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()

	const (
		projectID  = "p-wipe-cache-evict"
		otherProj  = "p-wipe-cache-survivor"
		sourceName = "project-wipe-cache-test"
		model      = "test-embed-model"
	)
	seed := []struct{ id, project, text string }{
		{"chunk-pw-1", projectID, "a sentence belonging to the wiped project"},
		{"chunk-pw-2", projectID, "a second sentence in the wiped project"},
		// A different project's chunk with DIFFERENT text: an over-broad eviction
		// is its own bug, and "everything is gone" would pass a weaker assertion.
		{"chunk-pw-3", otherProj, "a sentence belonging to a project that stays"},
	}

	t.Cleanup(func() {
		for _, s := range seed {
			_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, s.id)
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, memory.EmbedInputHash(sourceName, s.text))
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, memory.ContentHash(s.text))
		}
	})

	cache := memory.NewEmbeddingCache(db)
	for _, s := range seed {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO project_memory_chunks
			   (id, project_id, source_name, content, content_hash, created_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())`,
			s.id, s.project, sourceName, s.text, memory.ContentHash(s.text)); err != nil {
			t.Fatalf("seed chunk %s: %v", s.id, err)
		}
		// Production caches under the CONTEXTUALISED embed input, not the raw
		// content — seeding the raw hash is what let an eviction that deleted
		// nothing pass the sibling suite.
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

	deleter := postgres.NewProjectDataCleanupRepository(db, memory.EvictProjectEmbeddingCache)
	stats, err := deleter.DeleteProjectData(ctx, projectID)
	if err != nil {
		t.Fatalf("DeleteProjectData: %v", err)
	}

	for _, s := range seed[:2] {
		if cached(s.text) {
			t.Errorf("project %s was wiped but %s's embedding_cache row survives — "+
				"a vector derived from deleted content is still held", projectID, s.id)
		}
	}
	if !cached(seed[2].text) {
		t.Error("evicted a DIFFERENT project's cache row; the eviction must be scoped " +
			"to the chunks actually deleted")
	}

	// A zero that could mean "nothing to evict" or "never attempted" is the shape
	// of control this codebase treats as a defect, so the stats say which.
	if !stats.CacheEvictionRan {
		t.Error("stats do not record that the cache eviction ran")
	}
	if stats.CachedEmbeddingsEvicted != 2 {
		t.Errorf("CachedEmbeddingsEvicted = %d, want 2", stats.CachedEmbeddingsEvicted)
	}

	// The sweeper retries; "already gone" is the expected second-run state.
	if _, err := deleter.DeleteProjectData(ctx, projectID); err != nil {
		t.Errorf("second DeleteProjectData must be a no-op, got %v", err)
	}
}

// The shared-hash case, pinned rather than discovered later.
//
// project_memory_chunks is UNIQUE on (project_id, content_hash), so a hash
// another project also holds means an IDENTICAL chunk there, not a collision.
// Evicting the shared row costs that project one re-embed on its next cache
// miss; its chunk row is untouched. That is the acceptable direction — a
// re-embed is cost, a retained vector derived from deleted content is not — and
// it is the same trade already pinned for DeleteByExtractedDocument.
func TestIntegrationDeleteProjectData_EvictsAHashSharedAcrossProjects(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()

	const (
		wiped      = "p-wipe-shared-hash-gone"
		survivor   = "p-wipe-shared-hash-stays"
		sourceName = "project-wipe-shared-test"
		model      = "test-embed-model"
		shared     = "identical text held by two projects"
	)
	key := memory.EmbedInputHash(sourceName, shared)

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id IN ($1,$2)`, "chunk-sh-1", "chunk-sh-2")
		_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, key)
	})

	for _, c := range []struct{ id, project string }{
		{"chunk-sh-1", wiped},
		{"chunk-sh-2", survivor},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO project_memory_chunks
			   (id, project_id, source_name, content, content_hash, created_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())`,
			c.id, c.project, sourceName, shared, memory.ContentHash(shared)); err != nil {
			t.Fatalf("seed chunk %s: %v", c.id, err)
		}
	}
	cache := memory.NewEmbeddingCache(db)
	if err := cache.Put(ctx, key, model, testVector()); err != nil {
		t.Fatalf("seed shared cache row: %v", err)
	}

	deleter := postgres.NewProjectDataCleanupRepository(db, memory.EvictProjectEmbeddingCache)
	if _, err := deleter.DeleteProjectData(ctx, wiped); err != nil {
		t.Fatalf("DeleteProjectData: %v", err)
	}

	var cacheRows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embedding_cache WHERE content_hash = $1`, key).Scan(&cacheRows); err != nil {
		t.Fatalf("count cache rows: %v", err)
	}
	if cacheRows != 0 {
		t.Error("the shared cache row survived the wipe: a vector derived from the " +
			"deleted project's content is still held, and sharing a hash with a live " +
			"project is not a reason to keep it")
	}

	var survivorChunks int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE id = $1`, "chunk-sh-2").Scan(&survivorChunks); err != nil {
		t.Fatalf("count survivor chunks: %v", err)
	}
	if survivorChunks != 1 {
		t.Error("the surviving project's CHUNK was deleted; only the derived vector " +
			"may go, and it costs that project one re-embed, not its data")
	}
}

// The eviction runs INSIDE the wipe's transaction, so its failure must take the
// whole wipe with it. The alternative — chunks gone, vectors kept, error
// returned — is the exact state the eviction exists to prevent, reached by the
// error path instead of the happy one.
func TestIntegrationDeleteProjectData_AFailedEvictionRollsBackTheWipe(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx := context.Background()

	const (
		projectID  = "p-wipe-evict-rollback"
		chunkID    = "chunk-rb-1"
		sourceName = "project-wipe-rollback-test"
	)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = $1`, chunkID)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO project_memory_chunks
		   (id, project_id, source_name, content, content_hash, created_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())`,
		chunkID, projectID, sourceName, "text that must survive a failed eviction",
		memory.ContentHash("text that must survive a failed eviction")); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	boom := errors.New("the cache backend is unavailable")
	deleter := postgres.NewProjectDataCleanupRepository(db,
		func(context.Context, persistence.DBTX, string) (int, error) { return 0, boom })

	if _, err := deleter.DeleteProjectData(ctx, projectID); !errors.Is(err, boom) {
		t.Fatalf("DeleteProjectData error = %v, want the evictor's error", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE id = $1`, chunkID).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if n != 1 {
		t.Error("the chunk was deleted despite the eviction failing: the wipe committed " +
			"without its derived vectors, which is the state the eviction prevents")
	}
}
