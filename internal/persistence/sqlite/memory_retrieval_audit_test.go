package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestMemoryRetrievalAuditRepository_FeedbackStatsAndUnretrieved drives
// the SQLite-specific json_each path for both FeedbackStats and
// UnretrievedChunkIDs.
func TestMemoryRetrievalAuditRepository_FeedbackStatsAndUnretrieved(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewMemoryRetrievalAuditRepository(db.DB)
	ctx := context.Background()

	// Seed 3 chunks; retrieval audit will reference 2 of them.
	now := time.Now().UTC()
	for i, id := range []string{"c1", "c2", "c3"} {
		_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, needs_graph_extraction, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, "p1", "body", "h"+string(rune('0'+i)), 0,
			now.Add(time.Duration(i)*time.Second).Format(time.RFC3339))
		if err != nil {
			t.Fatalf("seed chunk %s: %v", id, err)
		}
	}

	// Two audit rows referencing c1 + c2 across them.
	if err := repo.Record(ctx, &persistence.MemoryRetrievalAudit{
		ProjectID: "p1", Query: "q", ChunkIDs: []string{"c1", "c2"}, RetrievedAt: now,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := repo.Record(ctx, &persistence.MemoryRetrievalAudit{
		ProjectID: "p1", Query: "q", ChunkIDs: []string{"c2"}, RetrievedAt: now,
	}); err != nil {
		t.Fatalf("Record 2: %v", err)
	}

	stats, err := repo.FeedbackStats(ctx, "p1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("FeedbackStats: %v", err)
	}
	if stats.TotalChunks != 3 {
		t.Errorf("TotalChunks = %d, want 3", stats.TotalChunks)
	}
	if stats.TotalSearches != 2 {
		t.Errorf("TotalSearches = %d, want 2", stats.TotalSearches)
	}
	if stats.RetrievedChunks != 2 {
		t.Errorf("RetrievedChunks = %d, want 2", stats.RetrievedChunks)
	}
	if stats.UnretrievedChunks != 1 {
		t.Errorf("UnretrievedChunks = %d, want 1", stats.UnretrievedChunks)
	}

	// FeedbackStats guard.
	if _, err := repo.FeedbackStats(ctx, "", now); err == nil {
		t.Error("empty project should error")
	}

	// UnretrievedChunkIDs.
	ids, err := repo.UnretrievedChunkIDs(ctx, "p1", now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UnretrievedChunkIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "c3" {
		t.Errorf("UnretrievedChunkIDs = %v, want [c3]", ids)
	}
	// Guard.
	if _, err := repo.UnretrievedChunkIDs(ctx, "", now, 0); err == nil {
		t.Error("empty project should error")
	}

	// Record nil guard.
	if err := repo.Record(ctx, nil); err == nil {
		t.Error("nil audit should error")
	}
}
