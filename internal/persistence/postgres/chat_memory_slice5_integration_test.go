//go:build integration

package postgres

// End-to-end (real Postgres) coverage for chat memory-write slice 5:
//
//   - TestIntegration_IngestChatMemoryMetadata pins the write path: a deposit
//     driven through the REAL memory.Pipeline lands one project_memory_chunks
//     row stamped content_class='chat_memory', confidence≈0.3 and
//     expires_at≈now+90d (design §2/§3/§5.5), which also proves the
//     ProvenanceCompleteGate chat: carve-out passed (a gated-out deposit would
//     leave no row).
//
//   - TestIntegration_ChatMemorySweepCascade pins the TTL-verified-to-DELETE
//     mechanism (design §5 / parent §6.4, review C2): a past-expires_at chunk
//     is HARD-DELETED by the retention sweep, and — the mechanism, not a bare
//     "no orphans" — the swept chunk leaves NO entity_mentions (FK cascade) and
//     NO data_subject_links (polymorphic, no FK → the sweep's paired delete)
//     referencing it, while a future-expires_at chunk survives.
//
// Lives in package postgres (not memory) on purpose: postgres already imports
// memory (chunk_redactor.go), so a memory-package integration test importing
// postgres is an import cycle — a PRE-EXISTING condition affecting
// internal/memory's integration lane, independent of this work. Placing these
// here reuses the package's TestMain schema bootstrap and avoids the cycle.

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/retention"
)

func slice5DB(t *testing.T) *DB {
	t.Helper()
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	db, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestIntegration_IngestChatMemoryMetadata(t *testing.T) {
	db := slice5DB(t)
	ctx := context.Background()
	const projectID = "slice5-ingest-proj"

	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, projectID)
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM artifacts WHERE project_id = $1`, projectID)
	})
	_, _ = db.DB.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, projectID)

	repo := memory.NewRepository(db.DB)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = ""
	idx := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())
	pipeline := memory.NewPipeline(idx, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		CreateChatMemoryArtifact: func(ctx context.Context, projectID, artifactID, sourceName string, sizeBytes int64) error {
			size := sizeBytes
			return repoCreateArtifact(ctx, db, projectID, artifactID, sourceName, size)
		},
	})

	content := "The team standup moved to 9am on Mondays and this note is long enough to clear the gates."
	res, err := pipeline.IngestChatMemory(ctx, projectID, "slack", "C42", content)
	if err != nil {
		t.Fatalf("IngestChatMemory: %v", err)
	}
	if res.Stats.Admitted != 1 {
		t.Fatalf("Admitted = %d, want 1; stats=%+v", res.Stats.Admitted, res.Stats)
	}
	if len(res.ChunkIDs) == 0 {
		t.Fatal("expected at least one chunk id back")
	}

	var (
		class     string
		conf      float64
		expiresAt time.Time
	)
	if err := db.DB.QueryRowContext(ctx, `
		SELECT content_class, confidence, expires_at
		FROM project_memory_chunks WHERE id = $1`, res.ChunkIDs[0]).
		Scan(&class, &conf, &expiresAt); err != nil {
		t.Fatalf("read chunk metadata: %v", err)
	}
	if class != "chat_memory" {
		t.Errorf("content_class = %q, want chat_memory", class)
	}
	if conf < 0.29 || conf > 0.31 {
		t.Errorf("confidence = %v, want ~0.3", conf)
	}
	// expires_at ≈ now + 90 days (allow a wide slack for test runtime).
	want := time.Now().Add(90 * 24 * time.Hour)
	if diff := expiresAt.Sub(want); diff > time.Hour || diff < -time.Hour {
		t.Errorf("expires_at = %v, want ~%v (diff %v)", expiresAt, want, diff)
	}
}

func TestIntegration_ChatMemorySweepCascade(t *testing.T) {
	db := slice5DB(t)
	ctx := context.Background()
	const projectID = "slice5-sweep-proj"

	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM data_subject_links WHERE project_id = $1`, projectID)
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM data_subjects WHERE id = $1`, projectID+"-subj")
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM entity_mentions WHERE chunk_id IN (SELECT id FROM project_memory_chunks WHERE project_id = $1)`, projectID)
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM knowledge_entities WHERE project_id = $1`, projectID)
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, projectID)
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM artifacts WHERE project_id = $1`, projectID)
	}
	cleanup()
	t.Cleanup(cleanup)

	repoCreateArtifactRaw(t, db, projectID, projectID+"-art", "chat:slack:C1")

	past := time.Now().Add(-24 * time.Hour)
	future := time.Now().Add(24 * time.Hour)
	expiredID := projectID + "-expired"
	freshID := projectID + "-fresh"
	insertChunk(t, db, projectID, projectID+"-art", expiredID, "expired chunk body", &past)
	insertChunk(t, db, projectID, projectID+"-art", freshID, "fresh chunk body", &future)

	// A knowledge_entity + entity_mention referencing the expiring chunk
	// (entity_mentions.chunk_id → project_memory_chunks ON DELETE CASCADE).
	entityID := projectID + "-ent"
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO knowledge_entities (id, project_id, type, canonical_name, confidence)
		VALUES ($1, $2, 'PERSON', 'Alice', 0.9)`, entityID, projectID); err != nil {
		t.Fatalf("insert knowledge_entity: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO entity_mentions (chunk_id, entity_id, char_start, char_end, surface)
		VALUES ($1, $2, 0, 5, 'Alice')`, expiredID, entityID); err != nil {
		t.Fatalf("insert entity_mention: %v", err)
	}

	// A data_subject + data_subject_link referencing the expiring chunk
	// (polymorphic (table_name,row_id), NO FK → will NOT cascade; the sweep
	// must delete it explicitly).
	subjID := projectID + "-subj"
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO data_subjects (id, display_name) VALUES ($1, 'op')`, subjID); err != nil {
		t.Fatalf("insert data_subject: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO data_subject_links (subject_id, table_name, row_id, project_id, source, confidence, exclusivity)
		VALUES ($1, 'project_memory_chunks', $2, $3, 'operator_asserted', 'probable', 'unknown')`,
		subjID, expiredID, projectID); err != nil {
		t.Fatalf("insert data_subject_link: %v", err)
	}

	// Sweep with generous windows for every OTHER table, so only the always-on
	// expires_at sweep acts.
	sweeper := retention.New(db.DB, zerolog.Nop())
	pol := retention.Policy{
		ProjectID:                 projectID,
		TaskLLMUsageDays:          3650,
		ToolAuditDays:             3650,
		TasksDays:                 3650,
		ExecutionsDays:            3650,
		ArtifactsDays:             3650,
		MemoryIngestAuditDays:     3650,
		MemoryPolicyEvalAllowDays: 3650,
		MemoryPolicyEvalBlockDays: 3650,
	}
	counts, err := sweeper.Sweep(ctx, pol)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if counts.MemoryExpired != 1 {
		t.Errorf("MemoryExpired = %d, want 1", counts.MemoryExpired)
	}

	// The expired chunk is GONE (hard delete, not filtered).
	if n := countRows(t, db, `SELECT COUNT(*) FROM project_memory_chunks WHERE id = $1`, expiredID); n != 0 {
		t.Errorf("expired chunk survived (count=%d)", n)
	}
	// The fresh chunk SURVIVES.
	if n := countRows(t, db, `SELECT COUNT(*) FROM project_memory_chunks WHERE id = $1`, freshID); n != 1 {
		t.Errorf("fresh chunk should survive (count=%d)", n)
	}
	// MECHANISM: no entity_mentions reference the swept chunk (FK cascade).
	if n := countRows(t, db, `SELECT COUNT(*) FROM entity_mentions WHERE chunk_id = $1`, expiredID); n != 0 {
		t.Errorf("entity_mentions orphaned for swept chunk (count=%d)", n)
	}
	// MECHANISM: no data_subject_links reference the swept chunk (paired delete).
	if n := countRows(t, db, `SELECT COUNT(*) FROM data_subject_links WHERE table_name='project_memory_chunks' AND row_id=$1`, expiredID); n != 0 {
		t.Errorf("data_subject_links orphaned for swept chunk (count=%d)", n)
	}
}

func repoCreateArtifact(ctx context.Context, db *DB, projectID, artifactID, name string, size int64) error {
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, size_bytes)
		VALUES ($1, $2, $3, 'METADATA', $4, $5)
		ON CONFLICT (id) DO NOTHING`, artifactID, projectID, name, "chat://inline", size)
	return err
}

func repoCreateArtifactRaw(t *testing.T, db *DB, projectID, artifactID, name string) {
	t.Helper()
	if err := repoCreateArtifact(context.Background(), db, projectID, artifactID, name, 0); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
}

func insertChunk(t *testing.T, db *DB, projectID, artifactID, chunkID, content string, expiresAt *time.Time) {
	t.Helper()
	_, err := db.DB.ExecContext(context.Background(), `
		INSERT INTO project_memory_chunks
			(id, project_id, artifact_id, source_name, chunk_index, content, content_hash, content_class, confidence, expires_at)
		VALUES ($1, $2, $3, 'chat:slack:C1', 0, $4, $5, 'chat_memory', 0.3, $6)`,
		chunkID, projectID, artifactID, content, chunkID+"-hash", expiresAt)
	if err != nil {
		t.Fatalf("insert chunk %s: %v", chunkID, err)
	}
}

func countRows(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}
