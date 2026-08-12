//go:build integration

package memory_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
)

// The embed queue must survive a restart.
//
// vornik states that restarts are safe, and customers run continuously with no
// quiet window — so the fragile path is hit routinely rather than rarely.
//
// DequeueEmbedBatch used to DELETE its queue rows and COMMIT before the worker
// embedded anything. A stop in that window left chunks with no queue row, no DLQ
// row and no embedding: permanently unretrievable by semantic search, and invisible
// to every signal the product exposes — GET /api/v1/memory/stats reported "embedded"
// counts that simply never reached the total, with an EMPTY queue and an EMPTY DLQ.
// Observed for real on 2026-08-11: 100 of 3,214 chunks orphaned by ordinary daemon
// restarts during a benchmark.
//
// The contract these tests pin is a LEASE, i.e. at-least-once delivery: claiming a
// chunk does not remove it, only a successful embed (or a DLQ hand-off) does, and an
// unacknowledged claim becomes reclaimable. At-least-once is safe here because
// re-embedding a chunk overwrites the same value.

func TestIntegration_EmbedQueue_ClaimDoesNotRemoveTheRow(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	project, chunkID := seedQueuedChunk(t, db)

	claimed, err := repo.DequeueEmbedBatch(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !containsChunk(claimed, chunkID) {
		t.Fatalf("claim did not return the queued chunk %s", chunkID)
	}

	// The row must still be there: nothing has been embedded yet, so a crash now
	// must not lose the work.
	if n := queueRowCount(t, db, chunkID); n != 1 {
		t.Errorf("queue row count = %d after claim, want 1 — a crash here would "+
			"orphan the chunk with no queue row and no DLQ trace", n)
	}
	_ = project
}

func TestIntegration_EmbedQueue_AckRemovesTheRow(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	_, chunkID := seedQueuedChunk(t, db)

	if _, err := repo.DequeueEmbedBatch(ctx, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := repo.AckEmbedQueue(ctx, []string{chunkID}); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n := queueRowCount(t, db, chunkID); n != 0 {
		t.Errorf("queue row count = %d after ack, want 0 — an acked chunk must not "+
			"be embedded again on every tick", n)
	}
}

// TestIntegration_EmbedQueue_UnackedClaimIsReclaimable — the restart-safety property
// itself. A claim that never completed must come back, or the chunk is lost exactly
// as it was before this fix.
func TestIntegration_EmbedQueue_UnackedClaimIsReclaimable(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	_, chunkID := seedQueuedChunk(t, db)

	if _, err := repo.DequeueEmbedBatch(ctx, 10); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// Simulate the crash: the claim happened, nothing was acked. Age the claim past
	// the reclaim window rather than sleeping through it.
	if _, err := db.ExecContext(ctx,
		`UPDATE memory_embed_queue SET claimed_at = now() - $2::interval WHERE chunk_id = $1`,
		chunkID, (memory.EmbedClaimReclaimAfter + time.Minute).String()); err != nil {
		t.Fatalf("age the claim: %v", err)
	}

	again, err := repo.DequeueEmbedBatch(ctx, 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !containsChunk(again, chunkID) {
		t.Errorf("an abandoned claim was NOT reclaimed; the chunk is orphaned exactly " +
			"as it was before the lease existed")
	}
}

// TestIntegration_EmbedQueue_FreshClaimIsNotStolen — the other half: leasing must not
// let two workers embed the same chunk concurrently on every tick.
func TestIntegration_EmbedQueue_FreshClaimIsNotStolen(t *testing.T) {
	db := openIngestRecallDB(t)
	repo := memory.NewRepository(db)
	ctx := context.Background()
	_, chunkID := seedQueuedChunk(t, db)

	if _, err := repo.DequeueEmbedBatch(ctx, 10); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := repo.DequeueEmbedBatch(ctx, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if containsChunk(second, chunkID) {
		t.Error("a freshly claimed chunk was handed out again immediately — two " +
			"workers would embed it in parallel every tick")
	}
}

// --- helpers ---

// seedQueuedChunk inserts one chunk and queues it, returning (projectID, chunkID).
// A unique project per call keeps parallel runs from seeing each other's rows, and
// cleanup removes only what this test made.
func seedQueuedChunk(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	project := "lease-test-" + suffix
	chunkID := "lease-chunk-" + suffix

	if _, err := db.ExecContext(ctx, `
INSERT INTO project_memory_chunks (id, project_id, source_name, chunk_index, content, content_hash)
VALUES ($1, $2, 'lease-test.md', 0, $3, $4)`,
		chunkID, project, "content for the embed-queue lease test", "hash-"+suffix); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO memory_embed_queue (chunk_id, project_id) VALUES ($1, $2)`,
		chunkID, project); err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
	t.Cleanup(func() {
		// The queue row cascades on chunk delete; remove both explicitly anyway so
		// a schema change to the FK cannot silently leave rows behind.
		_, _ = db.Exec(`DELETE FROM memory_embed_queue WHERE chunk_id = $1`, chunkID)
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE id = $1`, chunkID)
	})
	return project, chunkID
}

func queueRowCount(t *testing.T, db *sql.DB, chunkID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memory_embed_queue WHERE chunk_id = $1`,
		chunkID).Scan(&n); err != nil {
		t.Fatalf("count queue rows: %v", err)
	}
	return n
}

func containsChunk(chunks []memory.MemoryChunk, id string) bool {
	for _, c := range chunks {
		if c.ID == id {
			return true
		}
	}
	return false
}
