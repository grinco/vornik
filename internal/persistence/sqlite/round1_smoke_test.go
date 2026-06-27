package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// errDBTX is a minimal DBTX stub that always returns the given error from
// ExecContext. Used to exercise error-propagation branches that a real SQLite
// driver never produces.
type errDBTX struct{ err error }

func (e *errDBTX) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return nil, e.err
}
func (e *errDBTX) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, e.err
}
func (e *errDBTX) QueryRowContext(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	return nil
}

// Round-1 smoke tests for the 10 new simple-CRUD SQLite repos.
// Each test covers the write path + the most-used read path. The
// goal is "the SQL works end-to-end against a real SQLite database"
// — full protocol-contract coverage lives in the shared repotest
// package; this file is the per-repo equivalent of the postgres
// smoke tests that already exist.

func TestAPIKeyRepository_Roundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewAPIKeyRepository(db.DB)
	key := &persistence.APIKey{
		ID:        "akey-1",
		ProjectID: "proj",
		Name:      "demo",
		KeyHash:   "hash-1",
		KeyPrefix: "sk-demo",
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.LookupActiveByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("LookupActiveByHash: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("wrong row: %s", got.ID)
	}

	if err := repo.TouchLastUsed(ctx, key.ID); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	if err := repo.Revoke(ctx, key.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := repo.LookupActiveByHash(ctx, "hash-1"); err == nil {
		t.Fatal("expected ErrAPIKeyNotFound after Revoke")
	}
	rows, err := repo.ListByProject(ctx, "proj")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListByProject = %d rows, want 1", len(rows))
	}
}

// TestAPIKeyRepo_AllowPush verifies the allow_push column: new keys default
// to false, UpdateAllowPush flips it to true, and an unknown key ID returns
// ErrAPIKeyNotFound. This is the TDD anchor for migration 108 + the new
// UpdateAllowPush method on both backends.
func TestAPIKeyRepo_AllowPush(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewAPIKeyRepository(db.DB)

	k := &persistence.APIKey{
		ID: "akey_p", ProjectID: "proj_x", Name: "n", KeyHash: "h_allow_push", KeyPrefix: "pre",
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(ctx, k); err != nil {
		t.Fatal(err)
	}
	got, err := repo.LookupActiveByHash(ctx, "h_allow_push")
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowPush {
		t.Fatal("new key must default to read-only (AllowPush=false)")
	}
	if err := repo.UpdateAllowPush(ctx, k.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err = repo.LookupActiveByHash(ctx, "h_allow_push")
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowPush {
		t.Fatal("AllowPush should be true after UpdateAllowPush(true)")
	}
	if err := repo.UpdateAllowPush(ctx, "nope", true); err != persistence.ErrAPIKeyNotFound {
		t.Fatalf("want ErrAPIKeyNotFound, got %v", err)
	}
}

// TestAPIKeyRepo_AllowPush_EmptyKeyID verifies the empty-keyID guard returns a
// non-nil error immediately, without touching the database.
func TestAPIKeyRepo_AllowPush_EmptyKeyID(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewAPIKeyRepository(db.DB)
	if err := repo.UpdateAllowPush(context.Background(), "", true); err == nil {
		t.Fatal("UpdateAllowPush with empty keyID must return an error")
	}
}

// TestAPIKeyRepo_AllowPush_ExecError covers the ExecContext-error branch.
// A real SQLite driver never returns an error here (driver result is always
// valid), so we inject a stub DBTX that fails unconditionally.
// Note: the RowsAffected()-error branch (line 303-304 in api_key_repository.go)
// is genuinely unreachable via mattn/go-sqlite3 — the driver's Result always
// returns 0 error from RowsAffected — and is excluded from the ≥90% target.
func TestAPIKeyRepo_AllowPush_ExecError(t *testing.T) {
	execErr := errors.New("disk full")
	repo := sqlite.NewAPIKeyRepository(&errDBTX{err: execErr})
	err := repo.UpdateAllowPush(context.Background(), "any-key", true)
	if err == nil {
		t.Fatal("expected ExecContext error to propagate, got nil")
	}
}

func TestWebhookEventRepository_Roundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewWebhookEventRepository(db.DB)
	if err := repo.Record(ctx, &persistence.WebhookEvent{
		ID:        "wh-1",
		ProjectID: "p",
		Source:    "github",
		Status:    persistence.WebhookEventStatusAccepted,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	project := "p"
	list, err := repo.List(ctx, persistence.WebhookEventFilter{ProjectID: &project})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != "wh-1" {
		t.Fatalf("List = %v", list)
	}
}

func TestTaskMessageRepository_RoundtripWithCheckpoint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	taskRepo := sqlite.NewTaskRepository(db.DB)
	if err := taskRepo.Create(ctx, &persistence.Task{ID: "tmsg-task", ProjectID: "p", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	repo := sqlite.NewTaskMessageRepository(db.DB)
	checkpoint := &persistence.TaskMessage{
		ID:          "msg-cp",
		TaskID:      "tmsg-task",
		AuthorKind:  persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint,
		Content:     "Need confirmation",
	}
	if err := repo.Insert(ctx, checkpoint); err != nil {
		t.Fatalf("Insert checkpoint: %v", err)
	}
	openCP, err := repo.GetOpenCheckpoint(ctx, "tmsg-task")
	if err != nil {
		t.Fatalf("GetOpenCheckpoint: %v", err)
	}
	if openCP == nil || openCP.ID != "msg-cp" {
		t.Fatalf("GetOpenCheckpoint = %v, want msg-cp", openCP)
	}
	if err := repo.MarkCheckpointResolved(ctx, "tmsg-task", "msg-cp"); err != nil {
		t.Fatalf("MarkCheckpointResolved: %v", err)
	}
	openCP, _ = repo.GetOpenCheckpoint(ctx, "tmsg-task")
	if openCP != nil {
		t.Fatalf("checkpoint should be cleared, got %v", openCP)
	}
}

func TestTaskScratchpadRepository_UpsertRoundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskScratchpadRepository(db.DB)
	sp := &persistence.TaskScratchpad{
		TaskID:        "sp-task",
		Summary:       "first",
		Facts:         []byte(`{"a":1}`),
		OpenQuestions: []byte(`["q1"]`),
	}
	if err := repo.Upsert(ctx, sp); err != nil {
		t.Fatalf("Upsert#1: %v", err)
	}
	got, err := repo.Get(ctx, "sp-task")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Summary != "first" {
		t.Errorf("Summary = %q", got.Summary)
	}
	sp.Summary = "second"
	if err := repo.Upsert(ctx, sp); err != nil {
		t.Fatalf("Upsert#2: %v", err)
	}
	got, _ = repo.Get(ctx, "sp-task")
	if got.Summary != "second" {
		t.Errorf("Upsert did not replace: %q", got.Summary)
	}
}

func TestTelegramThreadRepository_Roundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTelegramThreadRepository(db.DB)
	thread := &persistence.TelegramTaskThread{
		TaskID:    "tt-task",
		ChatID:    100,
		ThreadID:  42,
		TopicName: "Task tt-task",
	}
	if err := repo.Insert(ctx, thread); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := repo.GetByTask(ctx, "tt-task")
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	if got.ThreadID != 42 {
		t.Errorf("ThreadID = %d", got.ThreadID)
	}
	got2, err := repo.GetByThread(ctx, 100, 42)
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	if got2.TaskID != "tt-task" {
		t.Errorf("TaskID = %q", got2.TaskID)
	}
	if err := repo.Insert(ctx, &persistence.TelegramTaskThread{
		TaskID: "other", ChatID: 100, ThreadID: 42, TopicName: "dup",
	}); err == nil {
		t.Fatal("expected duplicate-key error on (chat_id, thread_id) reuse")
	}
}

func TestAutonomyEvaluationRepository_RoundtripAndCounts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewAutonomyEvaluationRepository(db.DB)
	for i, outcome := range []string{"CREATED", "REJECTED", "CREATED"} {
		if err := repo.Record(ctx, &persistence.AutonomyEvaluation{
			ID:        "eval-" + string(rune('a'+i)),
			ProjectID: "p",
			Outcome:   outcome,
			Reason:    "test",
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	counts, err := repo.CountByOutcome(ctx, "p", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("CountByOutcome: %v", err)
	}
	if counts["CREATED"] != 2 || counts["REJECTED"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestIntentVerdictRepository_RoundtripWithLLMRefinement(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewIntentVerdictRepository(db.DB)
	v := &persistence.IntentVerdict{
		ID:                      "iv-1",
		ProjectID:               "p",
		ToolName:                "place_order",
		HeuristicRisk:           "medium",
		HeuristicConfidence:     0.7,
		HeuristicRecommendation: "approve",
		HeuristicReasoning:      "first heuristic",
		FinalRisk:               "medium",
		FinalRecommendation:     "approve",
	}
	if err := repo.Insert(ctx, v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	llmRisk := "high"
	llmRec := "deny"
	v.LLMRisk = &llmRisk
	v.LLMRecommendation = &llmRec
	if err := repo.UpdateLLMRefinement(ctx, v); err != nil {
		t.Fatalf("UpdateLLMRefinement: %v", err)
	}
	got, err := repo.ListRecent(ctx, "p", 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d verdicts", len(got))
	}
	if got[0].LLMRisk == nil || *got[0].LLMRisk != "high" {
		t.Errorf("LLMRisk = %v", got[0].LLMRisk)
	}
	if got[0].RefinedAt == nil {
		t.Error("RefinedAt should be set after UpdateLLMRefinement")
	}
}

func TestTaskJudgeVerdictRepository_RoundtripWithIdempotency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskJudgeVerdictRepository(db.DB)
	v := &persistence.TaskJudgeVerdict{
		ID:        "jv-1",
		ProjectID: "p",
		TaskID:    "task-jv",
		Role:      "judge",
		Model:     "claude-opus",
		Verdict:   "factual",
		Signals:   []byte(`{"score":0.9}`),
	}
	if err := repo.Record(ctx, v); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Second Record for the same task must return ErrDuplicateKey.
	if err := repo.Record(ctx, &persistence.TaskJudgeVerdict{
		ID: "jv-2", ProjectID: "p", TaskID: "task-jv", Role: "judge", Model: "claude-opus", Verdict: "factual",
	}); err == nil || err != persistence.ErrDuplicateKey {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
	got, err := repo.GetByTask(ctx, "task-jv")
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	if got.ID != "jv-1" || string(got.Signals) != `{"score":0.9}` {
		t.Errorf("got = %+v", got)
	}
}

func TestTaskPostMortemRepository_UpsertReplaces(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskPostMortemRepository(db.DB)
	pm := &persistence.TaskPostMortem{
		TaskID:    "pm-task",
		ProjectID: "p",
		Summary:   "first",
		Model:     "x",
	}
	if err := repo.Record(ctx, pm); err != nil {
		t.Fatalf("Record#1: %v", err)
	}
	pm.Summary = "second"
	if err := repo.Record(ctx, pm); err != nil {
		t.Fatalf("Record#2: %v", err)
	}
	got, err := repo.Get(ctx, "pm-task")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Summary != "second" {
		t.Errorf("upsert did not replace: %q", got.Summary)
	}
}

func TestMemoryRetrievalAuditRepository_RecordRoundtrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewMemoryRetrievalAuditRepository(db.DB)
	// Seed three chunks; the audit row references two of them.
	for _, id := range []string{"c1", "c2", "c3"} {
		if _, err := db.Exec(`
			INSERT INTO project_memory_chunks (id, project_id, content, created_at)
			VALUES (?, ?, ?, ?)`,
			id, "p", "txt", sqliteTimeNow(),
		); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
	}
	if err := repo.Record(ctx, &persistence.MemoryRetrievalAudit{
		ProjectID: "p",
		Query:     "what is x",
		ChunkIDs:  []string{"c1", "c2"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// FeedbackStats: 3 total chunks, 1 search, 2 distinct retrieved, 1 unretrieved.
	stats, err := repo.FeedbackStats(ctx, "p", time.Time{})
	if err != nil {
		t.Fatalf("FeedbackStats: %v", err)
	}
	if stats.TotalChunks != 3 || stats.TotalSearches != 1 ||
		stats.RetrievedChunks != 2 || stats.UnretrievedChunks != 1 {
		t.Errorf("FeedbackStats = %+v, want 3 total / 1 search / 2 retrieved / 1 unretrieved", stats)
	}

	// UnretrievedChunkIDs: only c3 is missing from the retrieval row.
	missing, err := repo.UnretrievedChunkIDs(ctx, "p", time.Time{}, 10)
	if err != nil {
		t.Fatalf("UnretrievedChunkIDs: %v", err)
	}
	if len(missing) != 1 || missing[0] != "c3" {
		t.Errorf("UnretrievedChunkIDs = %v, want [c3]", missing)
	}
}
