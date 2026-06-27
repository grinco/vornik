//go:build integration
// +build integration

package integration_test

// Integration test for ChunkGraphExtractionRepository.ReflagChunksMissingEdges
// against a real postgres. Validates the actual SQL semantics
// (NOT EXISTS with TEXT[] containment) that sqlmock can't exercise
// because it just matches query strings.
//
// Use case context: after the 2026-05-25 evidence-substring
// normalisation fix (commit ed1a501), the operator wants to re-flag
// the ~1330 isolated entities so the KG worker reprocesses them
// with the new logic. This test pins the contract that powers
// `vornikctl memory regraph`.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence/postgres"
)

func TestChunkGraphExtraction_ReflagChunksMissingEdges_SemanticsAgainstRealDB(t *testing.T) {
	db := connectDB(t)
	defer db.Close()

	// Per-test unique project so concurrent runs don't see each
	// other's chunks. Test cleans up afterwards.
	projectID := fmt.Sprintf("regraph-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM knowledge_edges WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE project_id = $1`, projectID)
	})

	// Seed three chunks under one project. IDs are per-test-unique
	// (projectID-suffixed) so concurrent / re-run cycles don't
	// collide on the project_memory_chunks_pkey.
	cWithEdges := projectID + "-with-edges"
	cNoEdges := projectID + "-no-edges"
	cAlreadyFlagged := projectID + "-already-flagged"
	mustInsertChunk(t, db, cWithEdges, projectID, "ACME", false)
	mustInsertChunk(t, db, cNoEdges, projectID, "Globex", false)
	mustInsertChunk(t, db, cAlreadyFlagged, projectID, "Cypress", true)

	// And one chunk under a DIFFERENT project with zero edges —
	// must NOT be touched (project scope discipline).
	otherProject := projectID + "-other"
	cForeign := otherProject + "-c"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE project_id = $1`, otherProject)
	})
	mustInsertChunk(t, db, cForeign, otherProject, "Foreign", false)

	// Seed two entities + an edge referencing the with-edges chunk.
	// The NOT EXISTS subquery's array containment lookup must find
	// that chunk via the source_chunks TEXT[].
	e1 := projectID + "-e1"
	e2 := projectID + "-e2"
	edge1 := projectID + "-edge1"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM knowledge_entities WHERE project_id = $1`, projectID)
	})
	mustInsertEntity(t, db, e1, projectID, "VENDOR", "ACME")
	mustInsertEntity(t, db, e2, projectID, "VENDOR", "Globex")
	mustInsertEdge(t, db, edge1, projectID, e1, e2, "RELATES_TO", []string{cWithEdges})

	repo := postgres.NewChunkGraphExtractionRepository(db)
	ctx := context.Background()

	// Dry-run first: count the candidates.
	n, err := repo.ReflagChunksMissingEdges(ctx, projectID, true)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only c-no-edges should be a candidate (c-with-edges has an edge; c-already-flagged is already true; c-foreign is in another project)")

	// Dry-run must NOT have mutated state.
	require.False(t, chunkNeedsExtraction(t, db, cNoEdges))

	// Real run: should flip exactly cNoEdges.
	n, err = repo.ReflagChunksMissingEdges(ctx, projectID, false)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.True(t, chunkNeedsExtraction(t, db, cNoEdges), "cNoEdges must have been re-flagged")
	require.False(t, chunkNeedsExtraction(t, db, cWithEdges), "cWithEdges must stay un-flagged (has an edge)")
	require.True(t, chunkNeedsExtraction(t, db, cAlreadyFlagged), "cAlreadyFlagged stays true (no-op)")
	require.False(t, chunkNeedsExtraction(t, db, cForeign), "foreign-project chunk must not be touched")

	// Idempotent: running again should be a zero-op since c-no-edges
	// is now needs_graph_extraction=true (the WHERE filter excludes
	// chunks already flagged).
	n, err = repo.ReflagChunksMissingEdges(ctx, projectID, false)
	require.NoError(t, err)
	require.Equal(t, 0, n, "second run must be a no-op")
}

func TestChunkGraphExtraction_ReflagChunksMissingEdges_IgnoresQuarantinedEdges(t *testing.T) {
	// Edges with lifecycle_state != 'published' should NOT count as
	// "the chunk produced an edge" — quarantined / superseded edges
	// don't surface in recall, so the chunk effectively has zero
	// active edges and should be re-flagged.
	db := connectDB(t)
	defer db.Close()

	projectID := fmt.Sprintf("regraph-quar-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM knowledge_edges WHERE project_id = $1`, projectID)
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE project_id = $1`, projectID)
	})

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM knowledge_entities WHERE project_id = $1`, projectID)
	})
	cQuar := projectID + "-c-quar"
	eQ1 := projectID + "-e-q1"
	eQ2 := projectID + "-e-q2"
	edgeQ := projectID + "-edge-q"
	mustInsertChunk(t, db, cQuar, projectID, "QuarChunk", false)
	mustInsertEntity(t, db, eQ1, projectID, "VENDOR", "Q1")
	mustInsertEntity(t, db, eQ2, projectID, "VENDOR", "Q2")
	// Quarantined edge — lifecycle != 'published'.
	_, err := db.Exec(`
		INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks, lifecycle_state, confidence)
		VALUES ($1, $2, $3, $4, 'RELATES_TO', $5, 'quarantined', 0.5)`,
		edgeQ, projectID, eQ1, eQ2, pq.Array([]string{cQuar}))
	require.NoError(t, err)

	repo := postgres.NewChunkGraphExtractionRepository(db)
	n, err := repo.ReflagChunksMissingEdges(context.Background(), projectID, false)
	require.NoError(t, err)
	require.Equal(t, 1, n, "chunk with only quarantined edges must be treated as having no active edges")
}

// mustInsertChunk inserts a minimal project_memory_chunks row.
// content_hash is needed (NOT NULL); the value can be any unique
// per-test string. source_name is NOT NULL too.
func mustInsertChunk(t *testing.T, db *sql.DB, id, projectID, content string, needsGraph bool) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO project_memory_chunks (
		    id, project_id, source_name, chunk_index, content, content_hash,
		    needs_graph_extraction, created_at
		) VALUES ($1, $2, 'test', 0, $3, $4, $5, NOW())`,
		id, projectID, content, "hash-"+id, needsGraph)
	require.NoError(t, err, "insert chunk %s", id)
}

func mustInsertEntity(t *testing.T, db *sql.DB, id, projectID, kind, name string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO knowledge_entities (
		    id, project_id, type, canonical_name, aliases, description, properties,
		    confidence, lifecycle_state
		) VALUES ($1, $2, $3, $4, '[]'::jsonb, '', '{}'::jsonb, 1.0, 'published')`,
		id, projectID, kind, name)
	require.NoError(t, err, "insert entity %s", id)
}

func mustInsertEdge(t *testing.T, db *sql.DB, id, projectID, from, to, predicate string, sourceChunks []string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO knowledge_edges (
		    id, project_id, from_entity, to_entity, predicate, source_chunks,
		    lifecycle_state, confidence
		) VALUES ($1, $2, $3, $4, $5, $6, 'published', 0.9)`,
		id, projectID, from, to, predicate, pq.Array(sourceChunks))
	require.NoError(t, err, "insert edge %s", id)
}

func chunkNeedsExtraction(t *testing.T, db *sql.DB, chunkID string) bool {
	t.Helper()
	var v bool
	err := db.QueryRow(
		`SELECT needs_graph_extraction FROM project_memory_chunks WHERE id = $1`, chunkID,
	).Scan(&v)
	require.NoError(t, err)
	return v
}
