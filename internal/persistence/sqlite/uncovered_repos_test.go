package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestChatAuditRepository_RoundTrip — Insert + List + prompt helpers.
func TestChatAuditRepository_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChatAuditRepository(db.DB)
	ctx := context.Background()

	// Save + Get prompt cache.
	if err := repo.SavePrompt(ctx, "hash-1", "system prompt body"); err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}
	body, err := repo.GetPrompt(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if body != "system prompt body" {
		t.Errorf("body = %q", body)
	}

	// Idempotent SavePrompt — second call must not error.
	if err := repo.SavePrompt(ctx, "hash-1", "second body"); err != nil {
		t.Fatalf("SavePrompt idempotent: %v", err)
	}

	// GetPrompt for missing hash returns ErrNotFound.
	if _, err := repo.GetPrompt(ctx, "missing"); err != persistence.ErrNotFound {
		t.Errorf("missing prompt err = %v, want ErrNotFound", err)
	}

	// Insert + list.
	base := time.Now().UTC().Truncate(time.Second)
	for i, e := range []*persistence.ChatAuditEntry{
		{ChatID: "c1", ProjectID: "p1", UserID: "u1", Model: "m", UserMessage: "hi", Response: "yo", Iterations: 1, DurationMs: 50, CostUSD: 0.001, Timestamp: base.Add(-3 * time.Hour)},
		{ChatID: "c1", ProjectID: "p1", UserID: "u1", Model: "m", UserMessage: "x", Response: "y", Iterations: 2, DurationMs: 80, CostUSD: 0.002, Timestamp: base.Add(-2 * time.Hour)},
		{ChatID: "c2", ProjectID: "p1", UserID: "u2", Model: "m", UserMessage: "a", Response: "b", Iterations: 1, DurationMs: 30, CostUSD: 0.001, Timestamp: base.Add(-time.Hour)},
		{ChatID: "c1", ProjectID: "p2", UserID: "u1", Model: "m", UserMessage: "p2", Response: "p2r", Iterations: 1, DurationMs: 40, CostUSD: 0.001, Timestamp: base.Add(-30 * time.Minute)},
	} {
		if err := repo.Insert(ctx, e); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		if e.ID == "" {
			t.Fatalf("Insert should populate ID for entry %d", i)
		}
	}

	rows, err := repo.List(ctx, persistence.ChatAuditFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	rows, err = repo.List(ctx, persistence.ChatAuditFilter{ChatID: "c1", PageSize: 10})
	if err != nil {
		t.Fatalf("List(chat): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("chat=c1: want 3, got %d", len(rows))
	}

	rows, err = repo.List(ctx, persistence.ChatAuditFilter{ProjectID: "p2", PageSize: 10})
	if err != nil {
		t.Fatalf("List(project): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("project=p2: want 1, got %d", len(rows))
	}

	rows, err = repo.List(ctx, persistence.ChatAuditFilter{Since: base.Add(-90 * time.Minute), Until: base, PageSize: 10})
	if err != nil {
		t.Fatalf("List(since/until): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("since/until: want 2, got %d", len(rows))
	}
}

// TestChatAuditRepository_PageSizeRequired — empty filter rejected.
func TestChatAuditRepository_PageSizeRequired(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChatAuditRepository(db.DB)
	if _, err := repo.List(context.Background(), persistence.ChatAuditFilter{}); err == nil {
		t.Fatal("PageSize=0 should error")
	}
}

// TestChatAuditRepository_NilEntry — defensive shape.
func TestChatAuditRepository_NilEntry(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChatAuditRepository(db.DB)
	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("Insert(nil) should error")
	}
	if err := repo.SavePrompt(context.Background(), "", "body"); err == nil {
		t.Fatal("SavePrompt with empty hash should error")
	}
}

// TestExecutionRepository_FullLifecycle exercises Create→Get→Update→
// UpdateStatus→SaveStateSnapshot→SetWorkflowSnapshot/GetWorkflowSnapshot→
// RecordCompletion/RecordFailure plus the read-side helpers (CountByStatus,
// List, Count, GetByTaskID, GetByTaskIDs).
func TestExecutionRepository_FullLifecycle(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewExecutionRepository(db.DB)
	taskRepo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	// Need parent tasks so the executions FK can be satisfied. The
	// SQLite schema uses a TEXT FK (lenient compared to PG) so it's
	// fine in practice, but inserting parents keeps the test honest.
	wf := "wf"
	for _, id := range []string{"task-a", "task-b"} {
		if err := taskRepo.Create(ctx, &persistence.Task{ID: id, ProjectID: "p1", WorkflowID: &wf}); err != nil {
			t.Fatalf("Task.Create %s: %v", id, err)
		}
	}

	exec := &persistence.Execution{ID: "e1", TaskID: "task-a", ProjectID: "p1", WorkflowID: "wf"}
	if err := repo.Create(ctx, exec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if exec.Status != persistence.ExecutionStatusPending {
		t.Errorf("default status = %q, want PENDING", exec.Status)
	}
	if exec.WorkflowRevision != "v1" {
		t.Errorf("WorkflowRevision default = %q, want v1", exec.WorkflowRevision)
	}

	got, err := repo.Get(ctx, "e1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TaskID != "task-a" {
		t.Errorf("Get TaskID = %q", got.TaskID)
	}

	// Get for missing returns ErrNotFound.
	if _, err := repo.Get(ctx, "missing"); err != persistence.ErrNotFound {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}

	// Update changes a few fields.
	step := "step-1"
	got.CurrentStepID = &step
	got.CompletedSteps = []string{"step-0"}
	got.WorkflowID = "wf2"
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, err := repo.Get(ctx, "e1")
	if err != nil {
		t.Fatalf("Get-after-update: %v", err)
	}
	if got2.WorkflowID != "wf2" {
		t.Errorf("WorkflowID after Update = %q", got2.WorkflowID)
	}
	if got2.CurrentStepID == nil || *got2.CurrentStepID != "step-1" {
		t.Errorf("CurrentStepID = %v", got2.CurrentStepID)
	}

	// UpdateStatus → RUNNING populates started_at.
	if err := repo.UpdateStatus(ctx, "e1", persistence.ExecutionStatusRunning); err != nil {
		t.Fatalf("UpdateStatus running: %v", err)
	}
	got3, _ := repo.Get(ctx, "e1")
	if got3.StartedAt == nil {
		t.Error("StartedAt should be set after RUNNING")
	}

	// SaveStateSnapshot bumps current_step + state_snapshot.
	if err := repo.SaveStateSnapshot(ctx, "e1", []byte(`{"k":1}`), "step-2", []string{"step-0", "step-1"}); err != nil {
		t.Fatalf("SaveStateSnapshot: %v", err)
	}
	// Also test the empty-currentStep branch.
	if err := repo.SaveStateSnapshot(ctx, "e1", nil, "", nil); err != nil {
		t.Fatalf("SaveStateSnapshot empty: %v", err)
	}

	// SetWorkflowSnapshot / GetWorkflowSnapshot.
	if err := repo.SetWorkflowSnapshot(ctx, "e1", []byte(`{"workflow":"snap"}`)); err != nil {
		t.Fatalf("SetWorkflowSnapshot: %v", err)
	}
	// Empty snapshot is a no-op (not an error).
	if err := repo.SetWorkflowSnapshot(ctx, "e1", nil); err != nil {
		t.Fatalf("SetWorkflowSnapshot empty: %v", err)
	}
	snap, err := repo.GetWorkflowSnapshot(ctx, "e1")
	if err != nil {
		t.Fatalf("GetWorkflowSnapshot: %v", err)
	}
	if string(snap) != `{"workflow":"snap"}` {
		t.Errorf("snap mismatch: %q", snap)
	}
	if _, err := repo.GetWorkflowSnapshot(ctx, "missing"); err != persistence.ErrNotFound {
		t.Errorf("missing snap err = %v, want ErrNotFound", err)
	}

	// RecordCompletion → COMPLETED + result.
	if err := repo.RecordCompletion(ctx, "e1", []byte(`{"r":1}`)); err != nil {
		t.Fatalf("RecordCompletion: %v", err)
	}
	got4, _ := repo.Get(ctx, "e1")
	if got4.Status != persistence.ExecutionStatusCompleted {
		t.Errorf("Status after RecordCompletion = %q", got4.Status)
	}

	// Second execution and RecordFailure.
	exec2 := &persistence.Execution{ID: "e2", TaskID: "task-b", ProjectID: "p1", WorkflowID: "wf"}
	if err := repo.Create(ctx, exec2); err != nil {
		t.Fatalf("Create e2: %v", err)
	}
	if err := repo.RecordFailure(ctx, "e2", "boom", "ERR_CODE"); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	got5, _ := repo.Get(ctx, "e2")
	if got5.Status != persistence.ExecutionStatusFailed {
		t.Errorf("Status after RecordFailure = %q", got5.Status)
	}
	if got5.ErrorMessage == nil || *got5.ErrorMessage != "boom" {
		t.Errorf("ErrorMessage = %v", got5.ErrorMessage)
	}
	// Empty error message / code branches.
	if err := repo.RecordFailure(ctx, "e2", "", ""); err != nil {
		t.Fatalf("RecordFailure empty: %v", err)
	}

	// CountByStatus over all projects.
	counts, err := repo.CountByStatus(ctx, "")
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[persistence.ExecutionStatusCompleted] != 1 || counts[persistence.ExecutionStatusFailed] != 1 {
		t.Errorf("counts wrong: %+v", counts)
	}
	// Project-filtered.
	counts, err = repo.CountByStatus(ctx, "p1")
	if err != nil {
		t.Fatalf("CountByStatus project: %v", err)
	}
	if len(counts) == 0 {
		t.Errorf("expected non-empty project counts")
	}

	// List + Count with filters.
	pid := "p1"
	status := persistence.ExecutionStatusFailed
	taskID := "task-b"
	listed, err := repo.List(ctx, persistence.ExecutionFilter{ProjectID: &pid, Status: &status, TaskID: &taskID, PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "e2" {
		t.Fatalf("List filter expected e2, got %v", listed)
	}
	cnt, err := repo.Count(ctx, persistence.ExecutionFilter{ProjectID: &pid})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if cnt != 2 {
		t.Errorf("Count = %d, want 2", cnt)
	}
	// List with offset path.
	listed2, _ := repo.List(ctx, persistence.ExecutionFilter{ProjectID: &pid, PageSize: 1, Offset: 1})
	if len(listed2) != 1 {
		t.Errorf("List offset: want 1 row got %d", len(listed2))
	}

	// GetByTaskID returns most-recent.
	taskExec, err := repo.GetByTaskID(ctx, "task-a")
	if err != nil {
		t.Fatalf("GetByTaskID: %v", err)
	}
	if taskExec.ID != "e1" {
		t.Errorf("GetByTaskID ID = %q", taskExec.ID)
	}

	// GetByTaskIDs batch — empty list returns empty map.
	em, err := repo.GetByTaskIDs(ctx, nil)
	if err != nil || len(em) != 0 {
		t.Errorf("GetByTaskIDs(nil) = %v, %v", em, err)
	}
	m, err := repo.GetByTaskIDs(ctx, []string{"task-a", "task-b"})
	if err != nil {
		t.Fatalf("GetByTaskIDs: %v", err)
	}
	if len(m) != 2 {
		t.Errorf("GetByTaskIDs result len = %d", len(m))
	}
}

// TestExecutionRepository_NilGuards exercises the nil-payload branches.
func TestExecutionRepository_NilGuards(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewExecutionRepository(db.DB)
	if err := repo.Create(context.Background(), nil); err == nil {
		t.Error("Create(nil) should error")
	}
	if err := repo.Update(context.Background(), nil); err == nil {
		t.Error("Update(nil) should error")
	}
	if _, err := repo.GetRoleQuality(context.Background(), "", 0); err == nil {
		t.Error("GetRoleQuality empty project should error")
	}
}

// TestTaskRepository_LifecycleAndChildren — exercises Ping, Delete,
// TransitionConditional happy + guard paths, CountRecentFailures,
// GetChildren, CountChildrenForParents, GetDependencies.
// TestTaskRepository_ChatTurnIDRoundtrip — migration v46. The
// dispatcher-set chat_turn_id must survive Create→Get and stay
// readable as a non-nil pointer. Also verifies the absence case
// (NULL → nil pointer) so the scanner branches aren't ambiguous.
func TestTaskRepository_ChatTurnIDRoundtrip(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	turn := "chat_20260521190824_aaaa"
	if err := repo.Create(ctx, &persistence.Task{
		ID: "task-tnid", ProjectID: "p1", ChatTurnID: &turn,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, "task-tnid")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ChatTurnID == nil || *got.ChatTurnID != turn {
		t.Errorf("ChatTurnID = %v, want %s", got.ChatTurnID, turn)
	}

	if err := repo.Create(ctx, &persistence.Task{ID: "task-bare", ProjectID: "p1"}); err != nil {
		t.Fatalf("Create bare: %v", err)
	}
	bare, err := repo.Get(ctx, "task-bare")
	if err != nil {
		t.Fatalf("Get bare: %v", err)
	}
	if bare.ChatTurnID != nil {
		t.Errorf("bare ChatTurnID should be nil, got %v", bare.ChatTurnID)
	}
}

func TestTaskRepository_LifecycleAndChildren(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	if err := repo.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	parentID := "task-parent"
	if err := repo.Create(ctx, &persistence.Task{ID: parentID, ProjectID: "p1"}); err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	for _, id := range []string{"child-1", "child-2"} {
		if err := repo.Create(ctx, &persistence.Task{
			ID: id, ProjectID: "p1", ParentTaskID: &parentID,
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	// GetChildren returns 2.
	kids, err := repo.GetChildren(ctx, parentID)
	if err != nil {
		t.Fatalf("GetChildren: %v", err)
	}
	if len(kids) != 2 {
		t.Errorf("GetChildren len = %d", len(kids))
	}

	// CountChildrenForParents — handles empty slice and populated.
	empty, _ := repo.CountChildrenForParents(ctx, nil)
	if len(empty) != 0 {
		t.Errorf("empty parent slice should be empty result")
	}
	counts, err := repo.CountChildrenForParents(ctx, []string{parentID, "unknown"})
	if err != nil {
		t.Fatalf("CountChildrenForParents: %v", err)
	}
	if counts[parentID] != 2 {
		t.Errorf("expected 2 children for %s, got %d", parentID, counts[parentID])
	}

	// GetDependencies — task with no deps returns nil; task with deps
	// returns the listed tasks.
	noDeps, err := repo.GetDependencies(ctx, parentID)
	if err != nil {
		t.Fatalf("GetDependencies empty: %v", err)
	}
	if len(noDeps) != 0 {
		t.Errorf("expected zero deps, got %d", len(noDeps))
	}
	withDepID := "depender"
	if err := repo.Create(ctx, &persistence.Task{
		ID: withDepID, ProjectID: "p1", Dependencies: []string{"child-1", "child-2"},
	}); err != nil {
		t.Fatalf("Create depender: %v", err)
	}
	deps, err := repo.GetDependencies(ctx, withDepID)
	if err != nil {
		t.Fatalf("GetDependencies populated: %v", err)
	}
	if len(deps) != 2 {
		t.Errorf("expected 2 deps, got %d", len(deps))
	}
	// Missing task ID returns ErrNotFound.
	if _, err := repo.GetDependencies(ctx, "missing"); err != persistence.ErrNotFound {
		t.Errorf("GetDependencies missing = %v, want ErrNotFound", err)
	}

	// TransitionConditional happy: child-1 from QUEUED → COMPLETED with closed bookkeeping.
	closedBy := "judge"
	expectedBy := time.Now().UTC().Add(time.Hour)
	currentPhase := "wrap-up"
	lastErr := "ok"
	lastClass := "completion"
	briefAmended := time.Now().UTC()
	ok, err := repo.TransitionConditional(ctx, "child-1",
		[]persistence.TaskStatus{persistence.TaskStatusQueued},
		persistence.TaskStatusCompleted,
		persistence.TransitionOpts{
			SetClosedAtNow: true,
			ClosedBy:       &closedBy,
			ExpectedBy:     &expectedBy,
			CurrentPhase:   &currentPhase,
			BriefAmendedAt: &briefAmended,
			LastError:      &lastErr,
			LastErrorClass: &lastClass,
			ClearLease:     true,
			Attempt:        2,
			MaxAttempts:    3,
		})
	if err != nil {
		t.Fatalf("TransitionConditional: %v", err)
	}
	if !ok {
		t.Error("expected transition to succeed")
	}

	// Guard: invalid args.
	if _, err := repo.TransitionConditional(ctx, "", nil, "", persistence.TransitionOpts{}); err == nil {
		t.Error("expected error for empty args")
	}

	// Delete + re-Delete idempotent.
	if err := repo.Delete(ctx, "child-2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := repo.Delete(ctx, "nope-not-present"); err != nil {
		t.Fatalf("Delete missing should be a no-op: %v", err)
	}

	// CountRecentFailures
	failedID := "failed-1"
	lec := "validation"
	if err := repo.Create(ctx, &persistence.Task{ID: failedID, ProjectID: "p1", LastErrorClass: &lec}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	_, _ = repo.TransitionConditional(ctx, failedID,
		[]persistence.TaskStatus{persistence.TaskStatusQueued},
		persistence.TaskStatusFailed,
		persistence.TransitionOpts{LastErrorClass: strPtr("validation")})
	n, err := repo.CountRecentFailures(ctx, "p1", []string{"validation"}, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountRecentFailures: %v", err)
	}
	if n != 1 {
		t.Errorf("CountRecentFailures = %d, want 1", n)
	}
	if _, err := repo.CountRecentFailures(ctx, "", nil, time.Time{}); err == nil {
		t.Error("CountRecentFailures empty project should error")
	}
}

// TestArtifactRepository_TaskIDAndDelete — UpdateTaskID + DeleteByExecutionID.
func TestArtifactRepository_TaskIDAndDelete(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewArtifactRepository(db.DB)
	ctx := context.Background()
	execID := "exec-1"
	a1 := &persistence.Artifact{ID: "a1", ProjectID: "p1", ExecutionID: &execID, Name: "x", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: "/x"}
	a2 := &persistence.Artifact{ID: "a2", ProjectID: "p1", ExecutionID: &execID, Name: "y", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: "/y"}
	if err := repo.Create(ctx, a1); err != nil {
		t.Fatalf("Create a1: %v", err)
	}
	if err := repo.Create(ctx, a2); err != nil {
		t.Fatalf("Create a2: %v", err)
	}

	if err := repo.UpdateTaskID(ctx, "a1", "task-x"); err != nil {
		t.Fatalf("UpdateTaskID: %v", err)
	}
	g, err := repo.Get(ctx, "a1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if g.TaskID == nil || *g.TaskID != "task-x" {
		t.Errorf("TaskID = %v", g.TaskID)
	}
	// Guards.
	if err := repo.UpdateTaskID(ctx, "", "task"); err == nil {
		t.Error("empty artifact id should error")
	}
	if err := repo.UpdateTaskID(ctx, "a1", ""); err == nil {
		t.Error("empty task id should error")
	}

	if err := repo.DeleteByExecutionID(ctx, execID); err != nil {
		t.Fatalf("DeleteByExecutionID: %v", err)
	}
	if _, err := repo.Get(ctx, "a1"); err != persistence.ErrNotFound {
		t.Errorf("a1 should be gone, got err=%v", err)
	}
}

// TestChunkGraphExtractionRepository_StatsAndLifecycle — Stats reads from
// every counter table; verify it composes after seeding entities/edges/mentions.
func TestChunkGraphExtractionRepository_StatsAndLifecycle(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChunkGraphExtractionRepository(db.DB)
	entRepo := sqlite.NewKnowledgeEntityRepository(db.DB)
	mentionRepo := sqlite.NewEntityMentionRepository(db.DB)
	ctx := context.Background()

	// Insert two chunks needing extraction.
	_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
		(id, project_id, content, content_hash, needs_graph_extraction, created_at)
		VALUES (?,?,?,?,?,?), (?,?,?,?,?,?)`,
		"c1", "p1", "alpha", "h1", 1, time.Now().UTC().Format(time.RFC3339),
		"c2", "p1", "beta", "h2", 0, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("seed chunks: %v", err)
	}

	if n, err := repo.PendingCount(ctx); err != nil || n != 1 {
		t.Errorf("PendingCount = %d, err=%v, want 1", n, err)
	}

	pending, err := repo.FetchUnextracted(ctx, 10)
	if err != nil {
		t.Fatalf("FetchUnextracted: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "c1" {
		t.Errorf("FetchUnextracted = %v", pending)
	}

	if err := repo.MarkExtracted(ctx, "c1"); err != nil {
		t.Fatalf("MarkExtracted: %v", err)
	}
	if err := repo.MarkExtracted(ctx, ""); err == nil {
		t.Error("MarkExtracted empty id should error")
	}

	// Seed an entity + mention so Stats has non-zero counts.
	if err := entRepo.Insert(ctx, &persistence.KnowledgeEntity{
		ProjectID: "p1", CanonicalName: "alpha", Type: "PERSON", Description: "Alpha",
	}); err != nil {
		t.Fatalf("entity insert: %v", err)
	}
	got, err := entRepo.GetByCanonical(ctx, "p1", "PERSON", "alpha")
	if err != nil {
		t.Fatalf("entity get: %v", err)
	}
	if err := mentionRepo.Insert(ctx, &persistence.EntityMention{
		ChunkID: "c1", EntityID: got.ID, CharStart: 0, Surface: "alpha",
	}); err != nil {
		t.Fatalf("mention insert: %v", err)
	}

	stats, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ChunksPending != 0 || stats.ChunksDone != 2 {
		t.Errorf("chunk counts: pending=%d done=%d", stats.ChunksPending, stats.ChunksDone)
	}
	if stats.Entities < 1 || stats.Mentions < 1 {
		t.Errorf("Entities=%d Mentions=%d", stats.Entities, stats.Mentions)
	}
	if stats.EntitiesByType["PERSON"] != 1 {
		t.Errorf("EntitiesByType[PERSON] = %d", stats.EntitiesByType["PERSON"])
	}
}

// TestEntityMentionRepository_ListByEntity round-trips Insert + ListByEntity.
func TestEntityMentionRepository_ListByEntity(t *testing.T) {
	db := newTestDB(t)
	mentionRepo := sqlite.NewEntityMentionRepository(db.DB)
	entRepo := sqlite.NewKnowledgeEntityRepository(db.DB)
	ctx := context.Background()

	if err := entRepo.Insert(ctx, &persistence.KnowledgeEntity{
		ProjectID: "p1", CanonicalName: "alpha", Type: "PERSON", Description: "Alpha",
	}); err != nil {
		t.Fatalf("entity insert: %v", err)
	}
	ent, _ := entRepo.GetByCanonical(ctx, "p1", "PERSON", "alpha")

	end := 10
	for i, chunkID := range []string{"c1", "c2", "c3"} {
		if err := mentionRepo.Insert(ctx, &persistence.EntityMention{
			ChunkID: chunkID, EntityID: ent.ID, CharStart: i, CharEnd: &end, Surface: "alpha",
		}); err != nil {
			t.Fatalf("mention %d: %v", i, err)
		}
	}

	got, err := mentionRepo.ListByEntity(ctx, ent.ID, 0)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("ListByEntity len = %d", len(got))
	}
	// limit guard.
	got2, _ := mentionRepo.ListByEntity(ctx, ent.ID, 2)
	if len(got2) != 2 {
		t.Errorf("ListByEntity limit=2 len = %d", len(got2))
	}
}

// TestKnowledgeEdgeRepository_GetAndLifecycle covers Get, UpdateLifecycle,
// EdgesForEntity.
func TestKnowledgeEdgeRepository_GetAndLifecycle(t *testing.T) {
	db := newTestDB(t)
	entRepo := sqlite.NewKnowledgeEntityRepository(db.DB)
	edgeRepo := sqlite.NewKnowledgeEdgeRepository(db.DB)
	ctx := context.Background()

	for _, e := range []*persistence.KnowledgeEntity{
		{ProjectID: "p1", CanonicalName: "a", Type: "PERSON", Description: "A"},
		{ProjectID: "p1", CanonicalName: "b", Type: "PERSON", Description: "B"},
	} {
		if err := entRepo.Insert(ctx, e); err != nil {
			t.Fatalf("entity insert: %v", err)
		}
	}
	a, _ := entRepo.GetByCanonical(ctx, "p1", "PERSON", "a")
	b, _ := entRepo.GetByCanonical(ctx, "p1", "PERSON", "b")

	edge := &persistence.KnowledgeEdge{
		ProjectID:    "p1",
		FromEntity:   a.ID,
		ToEntity:     b.ID,
		Predicate:    "KNOWS",
		Confidence:   0.9,
		SourceChunks: []string{"c1"},
	}
	if err := edgeRepo.UpsertEdge(ctx, edge); err != nil {
		t.Fatalf("edge upsert: %v", err)
	}
	if edge.ID == "" {
		t.Fatal("edge ID not populated")
	}

	got, err := edgeRepo.Get(ctx, edge.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Predicate != "KNOWS" {
		t.Errorf("Predicate = %q", got.Predicate)
	}

	// EdgesForEntity from each side.
	listA, err := edgeRepo.EdgesForEntity(ctx, a.ID, 0)
	if err != nil {
		t.Fatalf("EdgesForEntity(a): %v", err)
	}
	if len(listA) != 1 {
		t.Errorf("EdgesForEntity(a) len = %d", len(listA))
	}
	listB, _ := edgeRepo.EdgesForEntity(ctx, b.ID, 50)
	if len(listB) != 1 {
		t.Errorf("EdgesForEntity(b) len = %d", len(listB))
	}

	// UpdateLifecycle moves edge out of published.
	if err := edgeRepo.UpdateLifecycle(ctx, edge.ID, "quarantined"); err != nil {
		t.Fatalf("UpdateLifecycle: %v", err)
	}
	listA, _ = edgeRepo.EdgesForEntity(ctx, a.ID, 0)
	if len(listA) != 0 {
		t.Errorf("after quarantine, EdgesForEntity should be empty, got %d", len(listA))
	}

	// Guards.
	if _, err := edgeRepo.Get(ctx, ""); err == nil {
		t.Error("Get empty id should error")
	}
	if _, err := edgeRepo.EdgesForEntity(ctx, "", 0); err == nil {
		t.Error("EdgesForEntity empty id should error")
	}
	if err := edgeRepo.UpdateLifecycle(ctx, "", ""); err == nil {
		t.Error("UpdateLifecycle empty args should error")
	}
}

// TestMemoryQuarantineRepository_MarkDropped — MarkDropped path that
// wasn't exercised by the repotest suite.
func TestMemoryQuarantineRepository_MarkDropped(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewMemoryQuarantineRepository(db.DB)
	artRepo := sqlite.NewArtifactRepository(db.DB)
	ctx := context.Background()
	if err := artRepo.Create(ctx, &persistence.Artifact{
		ID: "a1", ProjectID: "p1", Name: "n", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: "/p",
	}); err != nil {
		t.Fatalf("artifact create: %v", err)
	}
	role := "r"
	q := &persistence.MemoryQuarantineItem{
		ProjectID: "p1", SourceArtifactID: "a1", ProducerRole: &role, FailedGate: "claim", Content: "x", ContentHash: "h",
	}
	if err := repo.Insert(ctx, q); err != nil {
		t.Fatalf("quarantine insert: %v", err)
	}
	if err := repo.MarkDropped(ctx, q.ID); err != nil {
		t.Fatalf("MarkDropped: %v", err)
	}
	// Guard
	if err := repo.MarkDropped(ctx, ""); err == nil {
		t.Error("MarkDropped empty id should error")
	}
}

// TestIngestQueueRepository_StaleHelpers — ProjectsWithQueued,
// ResetStaleProcessing, CountStaleProcessing.
func TestIngestQueueRepository_StaleHelpers(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewIngestQueueRepository(db.DB)
	artRepo := sqlite.NewArtifactRepository(db.DB)
	ctx := context.Background()

	if err := artRepo.Create(ctx, &persistence.Artifact{
		ID: "a1", ProjectID: "p1", Name: "n", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: "/p",
	}); err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{
		ProjectID: "p1", SourceArtifactID: "a1", ProducerRole: "r",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{
		ProjectID: "p2", SourceArtifactID: "a1", ProducerRole: "r",
	}); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}

	projs, err := repo.ProjectsWithQueued(ctx, 0)
	if err != nil {
		t.Fatalf("ProjectsWithQueued: %v", err)
	}
	if len(projs) != 2 {
		t.Errorf("projects len = %d", len(projs))
	}

	// Claim & leave processing so we can stale-reset.
	claimed, err := repo.ClaimBatch(ctx, "p1", 1)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimBatch len = %d", len(claimed))
	}

	// CountStaleProcessing with a generous cutoff (negative => clamps to 0) reports the row.
	n, err := repo.CountStaleProcessing(ctx, -time.Second)
	if err != nil {
		t.Fatalf("CountStaleProcessing: %v", err)
	}
	if n != 1 {
		t.Errorf("CountStaleProcessing = %d, want 1", n)
	}

	// ResetStaleProcessing flips it back to queued.
	reset, err := repo.ResetStaleProcessing(ctx, -time.Second)
	if err != nil {
		t.Fatalf("ResetStaleProcessing: %v", err)
	}
	if reset != 1 {
		t.Errorf("ResetStaleProcessing = %d, want 1", reset)
	}

	// QueueDepth requires a project.
	if _, err := repo.QueueDepth(ctx, ""); err == nil {
		t.Error("QueueDepth empty proj should error")
	}
}

// TestCorpusEpochRepository_FullLifecycle — covers CloseEpoch, ListEpochs,
// GetEpoch, RollbackTo, ListRollbacks beyond what repotest already tests.
func TestCorpusEpochRepository_FullLifecycle(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewCorpusEpochRepository(db.DB)
	ctx := context.Background()

	now := time.Now().UTC()
	e1 := &persistence.CorpusEpoch{ID: "e1", ProjectID: "p1", CreatedAt: now.Add(-3 * time.Hour)}
	e2 := &persistence.CorpusEpoch{ID: "e2", ProjectID: "p1", CreatedAt: now.Add(-2 * time.Hour)}
	e3 := &persistence.CorpusEpoch{ID: "e3", ProjectID: "p1", CreatedAt: now.Add(-time.Hour)}
	for _, e := range []*persistence.CorpusEpoch{e1, e2, e3} {
		if err := repo.CreateEpoch(ctx, e); err != nil {
			t.Fatalf("CreateEpoch %s: %v", e.ID, err)
		}
	}

	// CloseEpoch sets closed_at.
	if err := repo.CloseEpoch(ctx, "e1", persistence.CorpusEpochCounts{Admitted: 5, Quarantined: 1}); err != nil {
		t.Fatalf("CloseEpoch: %v", err)
	}
	if err := repo.CloseEpoch(ctx, "e2", persistence.CorpusEpochCounts{}); err != nil {
		t.Fatalf("CloseEpoch e2: %v", err)
	}
	if err := repo.CloseEpoch(ctx, "", persistence.CorpusEpochCounts{}); err == nil {
		t.Error("CloseEpoch empty id should error")
	}

	// Activate e1 + e2.
	if err := repo.Activate(ctx, "p1", "e1", "operator", "initial"); err != nil {
		t.Fatalf("Activate e1: %v", err)
	}
	if err := repo.Activate(ctx, "p1", "e2", "", "test"); err != nil {
		t.Fatalf("Activate e2: %v", err)
	}
	if err := repo.Activate(ctx, "", "", "", ""); err == nil {
		t.Error("Activate empty should error")
	}

	// ListEpochs.
	eps, err := repo.ListEpochs(ctx, "p1", 0)
	if err != nil {
		t.Fatalf("ListEpochs: %v", err)
	}
	if len(eps) != 3 {
		t.Errorf("ListEpochs len = %d", len(eps))
	}
	if _, err := repo.ListEpochs(ctx, "", 0); err == nil {
		t.Error("ListEpochs empty proj should error")
	}

	// GetEpoch.
	got, err := repo.GetEpoch(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEpoch: %v", err)
	}
	if got.ChunksAdmitted != 5 {
		t.Errorf("ChunksAdmitted = %d", got.ChunksAdmitted)
	}
	if !got.IsActive {
		t.Error("e1 should be active")
	}

	// Deactivate.
	if err := repo.Deactivate(ctx, "p1", "e2", "test"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if err := repo.Deactivate(ctx, "", "", ""); err == nil {
		t.Error("Deactivate empty should error")
	}

	// RollbackTo target e1 (oldest); should deactivate later epochs and
	// re-activate e1.
	deact, act, restored, err := repo.RollbackTo(ctx, "p1", "e1", "operator", "regression")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	_ = deact
	_ = act
	_ = restored
	if _, _, _, err := repo.RollbackTo(ctx, "", "", "", ""); err == nil {
		t.Error("RollbackTo empty args should error")
	}

	// ListRollbacks.
	rb, err := repo.ListRollbacks(ctx, "p1", 0)
	if err != nil {
		t.Fatalf("ListRollbacks: %v", err)
	}
	if len(rb) != 1 {
		t.Errorf("ListRollbacks len = %d", len(rb))
	}
	if _, err := repo.ListRollbacks(ctx, "", 0); err == nil {
		t.Error("ListRollbacks empty proj should error")
	}
}

// TestTaskMessageRepository_ListAndCheckpoint — List filters + GetOpenCheckpoint.
func TestTaskMessageRepository_ListAndCheckpoint(t *testing.T) {
	db := newTestDB(t)
	taskRepo := sqlite.NewTaskRepository(db.DB)
	repo := sqlite.NewTaskMessageRepository(db.DB)
	ctx := context.Background()

	if err := taskRepo.Create(ctx, &persistence.Task{ID: "t1", ProjectID: "p1"}); err != nil {
		t.Fatalf("Create task: %v", err)
	}

	// Insert messages of varying kinds (chronological).
	for i, kind := range []string{"user", "assistant", "checkpoint", "assistant"} {
		m := &persistence.TaskMessage{
			ID:          "m" + string(rune('0'+i)),
			TaskID:      "t1",
			AuthorKind:  "user",
			MessageKind: kind,
			Content:     "msg-" + kind,
		}
		if err := repo.Insert(ctx, m); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	// List returns all 4 in chronological order.
	out, err := repo.List(ctx, persistence.TaskMessageFilter{TaskID: "t1", Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("List len = %d", len(out))
	}

	// After cursor restricts.
	after := out[1].ID
	out2, err := repo.List(ctx, persistence.TaskMessageFilter{TaskID: "t1", After: &after, Limit: 10})
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	if len(out2) != 2 {
		t.Errorf("after cursor len = %d", len(out2))
	}

	// MessageKinds filter.
	out3, err := repo.List(ctx, persistence.TaskMessageFilter{
		TaskID: "t1", MessageKinds: []string{"assistant"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("List kinds: %v", err)
	}
	if len(out3) != 2 {
		t.Errorf("kinds filter len = %d", len(out3))
	}

	// Guards.
	if _, err := repo.List(ctx, persistence.TaskMessageFilter{}); err == nil {
		t.Error("List without task_id should error")
	}
	if _, err := repo.GetOpenCheckpoint(ctx, ""); err == nil {
		t.Error("GetOpenCheckpoint empty id should error")
	}

	// GetOpenCheckpoint returns the checkpoint message inserted at i=2.
	cp, err := repo.GetOpenCheckpoint(ctx, "t1")
	if err != nil {
		t.Fatalf("GetOpenCheckpoint: %v", err)
	}
	if cp == nil || cp.MessageKind != "checkpoint" {
		t.Errorf("expected checkpoint, got %v", cp)
	}

	// Resolve it; GetOpenCheckpoint now returns (nil, nil).
	if err := repo.MarkCheckpointResolved(ctx, "t1", cp.ID); err != nil {
		t.Fatalf("MarkCheckpointResolved: %v", err)
	}
	cpAfter, err := repo.GetOpenCheckpoint(ctx, "t1")
	if err != nil {
		t.Fatalf("GetOpenCheckpoint after resolve: %v", err)
	}
	if cpAfter != nil {
		t.Errorf("expected nil after resolve, got %v", cpAfter)
	}

	// Missing task: also returns (nil, nil).
	cp2, err := repo.GetOpenCheckpoint(ctx, "missing-task")
	if err != nil {
		t.Fatalf("GetOpenCheckpoint missing: %v", err)
	}
	if cp2 != nil {
		t.Error("expected nil for missing task")
	}

	// MarkCheckpointResolved guards.
	if err := repo.MarkCheckpointResolved(ctx, "", ""); err == nil {
		t.Error("MarkCheckpointResolved empty should error")
	}
}

// TestTradingOrderRepository_Count covers the Count helper which the
// existing contract suite doesn't exercise.
func TestTradingOrderRepository_Count(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTradingOrderRepository(db.DB)
	ctx := context.Background()
	for i, sym := range []string{"AAPL", "AAPL", "NVDA"} {
		id := "o" + string(rune('0'+i))
		boID := "bo" + string(rune('0'+i))
		if err := repo.Record(ctx, &persistence.TradingOrder{
			ID:             id,
			ProjectID:      "p",
			BrokerOrderID:  &boID,
			IdempotencyKey: "ik-" + id,
			Mode:           "paper",
			Symbol:         sym,
			Action:         "buy",
			OrderType:      "MKT",
			Qty:            1,
			Status:         "submitted",
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	n, err := repo.Count(ctx, persistence.TradingOrderFilter{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}

	sym := "AAPL"
	n, err = repo.Count(ctx, persistence.TradingOrderFilter{Symbol: &sym})
	if err != nil {
		t.Fatalf("Count(symbol): %v", err)
	}
	if n != 2 {
		t.Errorf("Count(symbol=AAPL) = %d, want 2", n)
	}
}

// TestTaskLLMUsageRepository_TopTasks — TopTasks aggregator not covered
// by other tests.
func TestTaskLLMUsageRepository_TopTasks(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)
	ctx := context.Background()
	taskA, taskB, taskC := "t-a", "t-b", "t-c"
	rows := []persistence.TaskLLMUsage{
		{ID: "u1", ProjectID: "p1", TaskID: &taskA, StepID: "s", Role: "w", Model: "m", CostUSD: 3.0},
		{ID: "u2", ProjectID: "p1", TaskID: &taskB, StepID: "s", Role: "w", Model: "m", CostUSD: 1.0},
		{ID: "u3", ProjectID: "p1", TaskID: &taskC, StepID: "s", Role: "w", Model: "m", CostUSD: 5.0},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	out, err := repo.TopTasks(ctx, time.Time{}, time.Time{}, 10, "p1")
	if err != nil {
		t.Fatalf("TopTasks: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("TopTasks len = %d", len(out))
	}
	// Sorted by cost desc.
	if out[0].TaskID != taskC {
		t.Errorf("top task = %q, want %q", out[0].TaskID, taskC)
	}
}
