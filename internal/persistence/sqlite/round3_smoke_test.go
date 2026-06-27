package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// Round-3 memory + KG repo smoke tests.

func TestExecutionStepOutcomeRepository_RecordFinalizeSweep(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionStepOutcomeRepository(db.DB)

	exec := "exec-1"
	// Two pending rows under one execution.
	for i, step := range []string{"s1", "s2"} {
		if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
			ID:          "oc-" + step,
			ProjectID:   "p",
			TaskID:      "t",
			ExecutionID: exec,
			StepID:      step,
			Role:        "worker",
			Model:       "m",
			Outcome:     "pending_validation",
			RecordedAt:  time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Record %s: %v", step, err)
		}
	}
	// Finalize one explicitly.
	role, model, err := repo.FinalizePending(ctx, exec, "s1", "ok", "", "", nil)
	if err != nil {
		t.Fatalf("FinalizePending: %v", err)
	}
	if role != "worker" || model != "m" {
		t.Errorf("FinalizePending returned (%q, %q), want (worker, m)", role, model)
	}
	// Sweep the rest.
	swept, err := repo.SweepPending(ctx, exec, "ok")
	if err != nil {
		t.Fatalf("SweepPending: %v", err)
	}
	if len(swept) != 1 || swept[0].StepID != "s2" {
		t.Fatalf("SweepPending returned %v", swept)
	}
	// CountByRoleModelOutcome: 2 ok rows.
	counts, err := repo.CountByRoleModelOutcome(ctx, "ok", time.Time{}, time.Time{}, "p")
	if err != nil {
		t.Fatalf("CountByRoleModelOutcome: %v", err)
	}
	if len(counts) != 1 || counts[0].Count != 2 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestKnowledgeEntityRepository_CRUDAndAlias(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewKnowledgeEntityRepository(db.DB)
	ent := &persistence.KnowledgeEntity{
		ProjectID:     "p",
		Type:          "person",
		CanonicalName: "Vadim",
		Aliases:       []byte(`["Vad"]`),
		Embedding:     []float32{0.1, 0.2, 0.3},
	}
	if err := repo.Insert(ctx, ent); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := repo.GetByCanonical(ctx, "p", "person", "Vadim")
	if err != nil {
		t.Fatalf("GetByCanonical: %v", err)
	}
	if got.ID != ent.ID {
		t.Errorf("ID mismatch: %s vs %s", got.ID, ent.ID)
	}
	if len(got.Embedding) != 3 || got.Embedding[1] != 0.2 {
		t.Errorf("Embedding round-trip lost data: %v", got.Embedding)
	}
	// Duplicate insert on same (project, type, name) → ErrDuplicateKey.
	dup := &persistence.KnowledgeEntity{
		ID: "ent-dup", ProjectID: "p", Type: "person", CanonicalName: "Vadim",
	}
	if err := repo.Insert(ctx, dup); err != persistence.ErrDuplicateKey {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}

	if err := repo.AddAlias(ctx, ent.ID, "Vad"); err != nil {
		t.Fatalf("AddAlias (existing): %v", err)
	}
	if err := repo.AddAlias(ctx, ent.ID, "VG"); err != nil {
		t.Fatalf("AddAlias (new): %v", err)
	}
	got, _ = repo.Get(ctx, ent.ID)
	if string(got.Aliases) != `["Vad","VG"]` {
		t.Errorf("Aliases after AddAlias: %s", got.Aliases)
	}
	if err := repo.UpdateLifecycle(ctx, ent.ID, "quarantined"); err != nil {
		t.Fatalf("UpdateLifecycle: %v", err)
	}
	got, _ = repo.Get(ctx, ent.ID)
	if got.LifecycleState != "quarantined" {
		t.Errorf("lifecycle = %s", got.LifecycleState)
	}

	// SimilarByEmbedding → ErrUnimplemented on SQLite.
	if _, err := repo.SimilarByEmbedding(ctx, "p", "person", []float32{1, 2, 3}, 5); err != sqlite.ErrUnimplemented {
		t.Errorf("expected ErrUnimplemented from SimilarByEmbedding, got %v", err)
	}
}

func TestKnowledgeEdgeRepository_UpsertMergesAndDrop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewKnowledgeEdgeRepository(db.DB)
	e1 := &persistence.KnowledgeEdge{
		ID: "e1", ProjectID: "p",
		FromEntity: "A", ToEntity: "B", Predicate: "works_with",
		SourceChunks: []string{"c1"},
		Confidence:   0.5,
	}
	if err := repo.UpsertEdge(ctx, e1); err != nil {
		t.Fatalf("UpsertEdge#1: %v", err)
	}
	// Second upsert with new chunk + higher confidence: merge.
	e2 := &persistence.KnowledgeEdge{
		ID: "e2", ProjectID: "p",
		FromEntity: "A", ToEntity: "B", Predicate: "works_with",
		SourceChunks: []string{"c2"},
		Confidence:   0.9,
		Properties:   []byte(`{"k":"v"}`),
	}
	if err := repo.UpsertEdge(ctx, e2); err != nil {
		t.Fatalf("UpsertEdge#2: %v", err)
	}
	got, err := repo.List(ctx, persistence.KnowledgeEdgeFilter{ProjectID: "p"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Upsert did not merge: %d rows", len(got))
	}
	if len(got[0].SourceChunks) != 2 {
		t.Errorf("merged source_chunks = %v, want 2", got[0].SourceChunks)
	}
	if got[0].Confidence != 0.9 {
		t.Errorf("Confidence not max-merged: %v", got[0].Confidence)
	}

	// DropChunkFromSources removes c1, leaving c2 only.
	n, err := repo.DropChunkFromSources(ctx, "c1")
	if err != nil {
		t.Fatalf("DropChunkFromSources: %v", err)
	}
	if n != 1 {
		t.Errorf("DropChunkFromSources affected = %d, want 1", n)
	}
	// Drop c2 too → edge should go quarantined.
	if _, err := repo.DropChunkFromSources(ctx, "c2"); err != nil {
		t.Fatalf("DropChunkFromSources c2: %v", err)
	}
	got, _ = repo.List(ctx, persistence.KnowledgeEdgeFilter{
		ProjectID: "p", Lifecycle: []string{"quarantined"},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 quarantined edge after losing all chunks, got %d", len(got))
	}
}

func TestEntityMentionRepository_RoundtripAndDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewEntityMentionRepository(db.DB)
	for i := 0; i < 3; i++ {
		if err := repo.Insert(ctx, &persistence.EntityMention{
			ChunkID:   "c-1",
			EntityID:  "e-" + string(rune('a'+i)),
			CharStart: i * 10,
			Surface:   "x",
		}); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	byChunk, err := repo.ListByChunk(ctx, "c-1")
	if err != nil {
		t.Fatalf("ListByChunk: %v", err)
	}
	if len(byChunk) != 3 {
		t.Fatalf("ListByChunk = %d", len(byChunk))
	}
	if err := repo.DeleteForChunk(ctx, "c-1"); err != nil {
		t.Fatalf("DeleteForChunk: %v", err)
	}
	byChunk, _ = repo.ListByChunk(ctx, "c-1")
	if len(byChunk) != 0 {
		t.Fatalf("DeleteForChunk left %d rows", len(byChunk))
	}
}

func TestChunkGraphExtractionRepository_FetchMarkStats(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// Seed two chunks needing extraction + one already extracted.
	now := sqliteTimeNow()
	for _, row := range []struct {
		id    string
		needs int
	}{
		{"chk-1", 1},
		{"chk-2", 1},
		{"chk-3", 0},
	} {
		if _, err := db.Exec(`
			INSERT INTO project_memory_chunks (id, project_id, content, needs_graph_extraction, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			row.id, "p", "txt", row.needs, now,
		); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
	}
	repo := sqlite.NewChunkGraphExtractionRepository(db.DB)
	got, err := repo.FetchUnextracted(ctx, 50)
	if err != nil {
		t.Fatalf("FetchUnextracted: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FetchUnextracted = %d, want 2", len(got))
	}
	if err := repo.MarkExtracted(ctx, "chk-1"); err != nil {
		t.Fatalf("MarkExtracted: %v", err)
	}
	pending, err := repo.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Errorf("PendingCount = %d, want 1", pending)
	}
	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ChunksDone != 2 || stats.ChunksPending != 1 {
		t.Errorf("stats wrong: %+v", stats)
	}
}

func sqliteTimeNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func TestMemoryQuarantineRepository_FullLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewMemoryQuarantineRepository(db.DB)
	item := &persistence.MemoryQuarantineItem{
		ProjectID:        "p",
		SourceArtifactID: "art-1",
		Content:          "leaked secret",
		ContentHash:      "h",
		FailedGate:       "secret_leak",
	}
	if err := repo.Insert(ctx, item); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	pending, err := repo.ListPending(ctx, "p", 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	counts, err := repo.CountByGate(ctx, "p")
	if err != nil {
		t.Fatalf("CountByGate: %v", err)
	}
	if counts["secret_leak"] != 1 {
		t.Errorf("counts = %v", counts)
	}
	if err := repo.MarkReleased(ctx, item.ID, "chunk-x"); err != nil {
		t.Fatalf("MarkReleased: %v", err)
	}
	got, _ := repo.Get(ctx, item.ID)
	if got.ReleasedChunkID == nil || *got.ReleasedChunkID != "chunk-x" {
		t.Errorf("MarkReleased did not link chunk: %v", got.ReleasedChunkID)
	}
}

func TestCorpusEpochRepository_ActivateAndListActive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewCorpusEpochRepository(db.DB)
	for _, id := range []string{"epoch-1", "epoch-2"} {
		ep := &persistence.CorpusEpoch{ID: id, ProjectID: "p"}
		if err := repo.CreateEpoch(ctx, ep); err != nil {
			t.Fatalf("CreateEpoch %s: %v", id, err)
		}
		if err := repo.Activate(ctx, "p", id, "test", "smoke"); err != nil {
			t.Fatalf("Activate %s: %v", id, err)
		}
	}
	active, err := repo.ListActive(ctx, "p")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("ListActive = %d, want 2", len(active))
	}
	// Re-Activate is a no-op.
	if err := repo.Activate(ctx, "p", "epoch-1", "test", "again"); err != nil {
		t.Fatalf("re-Activate: %v", err)
	}
	if err := repo.Deactivate(ctx, "p", "epoch-1", "test"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	active, _ = repo.ListActive(ctx, "p")
	if len(active) != 1 || active[0] != "epoch-2" {
		t.Errorf("after Deactivate: %v", active)
	}
}

func TestIngestQueueRepository_ClaimMarkDoneCycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewIngestQueueRepository(db.DB)
	for i := 0; i < 3; i++ {
		if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{
			ProjectID:        "p",
			SourceArtifactID: "art-" + string(rune('a'+i)),
			ProducerRole:     "worker",
			Priority:         int16(i),
		}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	depth, err := repo.QueueDepth(ctx, "p")
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 3 {
		t.Errorf("QueueDepth = %d, want 3", depth)
	}
	claimed, err := repo.ClaimBatch(ctx, "p", 10)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("ClaimBatch = %d, want 3", len(claimed))
	}
	for _, item := range claimed {
		if item.State != "processing" {
			t.Errorf("claimed item state = %s", item.State)
		}
		if item.Attempts != 1 {
			t.Errorf("claimed item attempts = %d, want 1", item.Attempts)
		}
	}
	if err := repo.MarkDone(ctx, claimed[0].ID); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	terminal, err := repo.MarkFailed(ctx, claimed[1].ID, 1, "boom")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !terminal {
		t.Error("MarkFailed should be terminal (attempts >= maxAttempts)")
	}
	// MarkFailed with maxAttempts=10 — back to queued for retry.
	terminal, err = repo.MarkFailed(ctx, claimed[2].ID, 10, "transient")
	if err != nil {
		t.Fatalf("MarkFailed retry: %v", err)
	}
	if terminal {
		t.Error("MarkFailed should not be terminal at attempts=1, max=10")
	}
}
