//go:build integration

package cli

// Integration coverage for `vornikctl memory dlq {list,replay}`
// (memory_dlq.go). Seeds memory_embed_dlq directly and drives the
// run* handlers against the live test Postgres.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func memoryDLQ_reset() {
	memoryDLQProject, memoryDLQLimit, memoryDLQJSON = "", 100, false
}

// dlqSeedChunk inserts a chunk (DLQ rows need a parent chunk because
// replay re-enqueues via memory_embed_queue which FKs to chunks) and
// a matching memory_embed_dlq row. Returns the chunk id.
func dlqSeedChunk(t *testing.T, db *sql.DB, project, reason, lastErr string, retryCount int) string {
	t.Helper()
	chunkID := dbcovSeedChunk(t, db, project, "dlq-src", "dlq-content-"+reason, "")
	_, err := db.Exec(`
		INSERT INTO memory_embed_dlq
		    (chunk_id, project_id, reason, last_error, retry_count, retry_after, first_failed_at, last_failed_at)
		VALUES ($1, $2, $3, $4, $5, now(), now(), now())`,
		chunkID, project, reason, lastErr, retryCount)
	if err != nil {
		t.Fatalf("seed dlq: %v", err)
	}
	return chunkID
}

func TestIntegration_MemoryDLQList_Table(t *testing.T) {
	db := dbcovSetup(t)
	memoryDLQ_reset()
	proj := dbcovUniqueProject("dlq-list")
	dbcovCleanupProject(t, db, proj)
	dlqSeedChunk(t, db, proj, "embedder-down", "connection refused", 3)
	dlqSeedChunk(t, db, proj, "parked", strings.Repeat("x", 120), -1) // long err → truncated, parked label

	memoryDLQProject = proj
	out, err := dbcovCapture(t, func() error { return runMemoryDLQList(memoryDLQListCmd, nil) })
	if err != nil {
		t.Fatalf("dlq list: %v", err)
	}
	if !strings.Contains(out, "CHUNK") || !strings.Contains(out, "REASON") {
		t.Fatalf("missing header:\n%s", out)
	}
	if !strings.Contains(out, "parked") {
		t.Errorf("expected parked label for retry_count=-1:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("expected truncated long error:\n%s", out)
	}
	if !strings.Contains(out, "Total: 2") {
		t.Errorf("expected total 2:\n%s", out)
	}
}

func TestIntegration_MemoryDLQList_JSON(t *testing.T) {
	db := dbcovSetup(t)
	memoryDLQ_reset()
	proj := dbcovUniqueProject("dlq-json")
	dbcovCleanupProject(t, db, proj)
	dlqSeedChunk(t, db, proj, "dim-mismatch", "expected 768 got 1024", 0)

	memoryDLQProject = proj
	memoryDLQJSON = true
	out, err := dbcovCapture(t, func() error { return runMemoryDLQList(memoryDLQListCmd, nil) })
	if err != nil {
		t.Fatalf("dlq list json: %v", err)
	}
	var parsed struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if parsed.Total != 1 || len(parsed.Entries) != 1 {
		t.Fatalf("unexpected json: %+v", parsed)
	}
}

func TestIntegration_MemoryDLQList_EmptyAndLimitClamp(t *testing.T) {
	db := dbcovSetup(t)
	memoryDLQ_reset()
	proj := dbcovUniqueProject("dlq-empty")
	dbcovCleanupProject(t, db, proj)

	memoryDLQProject = proj
	memoryDLQLimit = -5 // exercise the <=0 clamp branch
	out, err := dbcovCapture(t, func() error { return runMemoryDLQList(memoryDLQListCmd, nil) })
	if err != nil {
		t.Fatalf("dlq list: %v", err)
	}
	if !strings.Contains(out, "no DLQ entries") {
		t.Errorf("expected empty notice:\n%s", out)
	}
	if memoryDLQLimit != 100 {
		t.Errorf("limit clamp failed: got %d, want 100", memoryDLQLimit)
	}
}

func TestIntegration_MemoryDLQList_LimitOver1000Clamps(t *testing.T) {
	db := dbcovSetup(t)
	memoryDLQ_reset()
	proj := dbcovUniqueProject("dlq-clamp")
	dbcovCleanupProject(t, db, proj)

	memoryDLQProject = proj
	memoryDLQLimit = 5000
	_, err := dbcovCapture(t, func() error { return runMemoryDLQList(memoryDLQListCmd, nil) })
	if err != nil {
		t.Fatalf("dlq list: %v", err)
	}
	if memoryDLQLimit != 1000 {
		t.Errorf("upper clamp failed: got %d, want 1000", memoryDLQLimit)
	}
}

func TestIntegration_MemoryDLQReplay_MovesBackToQueue(t *testing.T) {
	db := dbcovSetup(t)
	memoryDLQ_reset()
	proj := dbcovUniqueProject("dlq-replay")
	dbcovCleanupProject(t, db, proj)
	chunkID := dlqSeedChunk(t, db, proj, "transient", "timeout", 2)

	out, err := dbcovCapture(t, func() error { return runMemoryDLQReplay(memoryDLQReplayCmd, []string{chunkID}) })
	if err != nil {
		t.Fatalf("dlq replay: %v", err)
	}
	if !strings.Contains(out, "replayed 1 chunk(s)") {
		t.Fatalf("expected replayed 1:\n%s", out)
	}
	// Gone from DLQ.
	var dlqN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_embed_dlq WHERE chunk_id=$1`, chunkID).Scan(&dlqN)
	if dlqN != 0 {
		t.Errorf("DLQ row still present after replay: %d", dlqN)
	}
	// Present in the embed queue.
	var qN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_embed_queue WHERE chunk_id=$1`, chunkID).Scan(&qN)
	if qN != 1 {
		t.Errorf("chunk not re-enqueued: queue count %d", qN)
	}
	// Cleanup queue residue.
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM memory_embed_queue WHERE chunk_id=$1`, chunkID) })
}

func TestIntegration_MemoryDLQReplay_UnknownChunkErrors(t *testing.T) {
	db := dbcovSetup(t)
	memoryDLQ_reset()
	proj := dbcovUniqueProject("dlq-replay-miss")
	dbcovCleanupProject(t, db, proj)

	// Replaying a chunk that has no DLQ row makes the re-enqueue
	// subselect yield a NULL project_id → the INSERT trips the
	// memory_embed_queue NOT NULL constraint and the whole tx rolls
	// back. The handler surfaces that as an error (production
	// behaviour: you can't replay something that was never parked).
	missing := fmt.Sprintf("nonexistent-%d", time.Now().UnixNano())
	_, err := dbcovCapture(t, func() error { return runMemoryDLQReplay(memoryDLQReplayCmd, []string{missing}) })
	if err == nil || !strings.Contains(err.Error(), "dlq replay insert") {
		t.Fatalf("expected replay-insert error for unknown chunk, got %v", err)
	}
	// Tx rolled back: nothing leaked into the queue.
	var qN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_embed_queue WHERE chunk_id=$1`, missing).Scan(&qN)
	if qN != 0 {
		t.Errorf("queue should be empty after rollback: %d", qN)
	}
}
