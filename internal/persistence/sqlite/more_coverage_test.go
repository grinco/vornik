package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestExecutionStepOutcome_FinalizeAndList covers Finalize / List /
// SupersedeAfter paths plus FinalizePending success branch.
func TestExecutionStepOutcome_FinalizeAndList(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewExecutionStepOutcomeRepository(db.DB)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	rows := []persistence.ExecutionStepOutcome{
		{ID: "o1", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s1", Role: "w", Model: "m", Outcome: "pending_validation", RecordedAt: base.Add(-3 * time.Hour)},
		{ID: "o2", ProjectID: "p", TaskID: "t", ExecutionID: "e1", StepID: "s2", Role: "w", Model: "m", Outcome: "pending_validation", RecordedAt: base.Add(-2 * time.Hour)},
		{ID: "o3", ProjectID: "p2", TaskID: "t2", ExecutionID: "e2", StepID: "s1", Role: "judge", Model: "m", Outcome: "ok", RecordedAt: base.Add(-time.Hour)},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	// Finalize o1 to ok.
	if err := repo.Finalize(ctx, "o1", "ok", "", "", nil); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	// Finalize missing id → ErrNotFound.
	if err := repo.Finalize(ctx, "missing", "ok", "", "", nil); err != persistence.ErrNotFound {
		t.Errorf("Finalize missing err = %v, want ErrNotFound", err)
	}

	// FinalizePending picks the most-recent pending for the step and
	// returns (role, model, err).
	role, model, finErr := repo.FinalizePending(ctx, "e1", "s2", "failed", "tool_error", "boom", nil)
	if finErr != nil {
		t.Fatalf("FinalizePending: %v", finErr)
	}
	if role != "w" || model != "m" {
		t.Errorf("FinalizePending role/model = %q/%q", role, model)
	}
	// FinalizePending with no pending → ErrNotFound.
	if _, _, err := repo.FinalizePending(ctx, "e1", "s2", "failed", "", "", nil); err != persistence.ErrNotFound {
		t.Errorf("FinalizePending second = %v, want ErrNotFound", err)
	}

	// List with various filters.
	proj := "p"
	taskID := "t"
	execID := "e1"
	stepID := "s1"
	roleFilter := "w"
	modelFilter := "m"
	outcome := "ok"
	listed, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ProjectID:   &proj,
		TaskID:      &taskID,
		ExecutionID: &execID,
		StepID:      &stepID,
		Role:        &roleFilter,
		Model:       &modelFilter,
		Outcome:     &outcome,
		PageSize:    10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "o1" {
		t.Errorf("List = %+v", listed)
	}

	// List with Since/Until + offset path.
	since := base.Add(-4 * time.Hour)
	until := base
	listed2, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		Since: &since, Until: &until, PageSize: 1, Offset: 1,
	})
	if err != nil {
		t.Fatalf("List paging: %v", err)
	}
	if len(listed2) != 1 {
		t.Errorf("paged list len = %d", len(listed2))
	}

	// SupersedeAfter.
	n, err := repo.SupersedeAfter(ctx, "e1", base.Add(-2*time.Hour-time.Minute))
	if err != nil {
		t.Fatalf("SupersedeAfter: %v", err)
	}
	if n < 1 {
		t.Errorf("SupersedeAfter affected = %d, want ≥1", n)
	}
}

// TestKnowledgeEntityRepository_List drives the List filter combinations
// (lifecycle, types, NameLike), guard, and pagination.
func TestKnowledgeEntityRepository_List(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewKnowledgeEntityRepository(db.DB)
	ctx := context.Background()

	for _, e := range []*persistence.KnowledgeEntity{
		{ProjectID: "p1", Type: "PERSON", CanonicalName: "alice"},
		{ProjectID: "p1", Type: "PERSON", CanonicalName: "alpha"},
		{ProjectID: "p1", Type: "ORG", CanonicalName: "acme"},
		{ProjectID: "p2", Type: "PERSON", CanonicalName: "bob"},
	} {
		if err := repo.Insert(ctx, e); err != nil {
			t.Fatalf("Insert %s: %v", e.CanonicalName, err)
		}
	}

	// Project filter alone.
	out, err := repo.List(ctx, persistence.KnowledgeEntityFilter{ProjectID: "p1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("project filter len = %d, want 3", len(out))
	}

	// Type filter.
	out, err = repo.List(ctx, persistence.KnowledgeEntityFilter{ProjectID: "p1", Types: []string{"PERSON"}})
	if err != nil {
		t.Fatalf("List type: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("type filter len = %d, want 2", len(out))
	}

	// NameLike.
	out, err = repo.List(ctx, persistence.KnowledgeEntityFilter{ProjectID: "p1", NameLike: "al"})
	if err != nil {
		t.Fatalf("List name: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("name filter len = %d, want 2", len(out))
	}

	// Guard.
	if _, err := repo.List(ctx, persistence.KnowledgeEntityFilter{}); err == nil {
		t.Error("List empty project should error")
	}

	// SimilarByEmbedding returns ErrUnimplemented.
	if _, err := repo.SimilarByEmbedding(ctx, "p1", "PERSON", []float32{1, 2, 3}, 5); err == nil {
		t.Error("SimilarByEmbedding should be unimplemented")
	}
}

// TestSqliteDB_Ping_Migrate covers the DB-level helpers.
func TestSqliteDB_Ping_Migrate(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Before Migrate the sentinel table is absent → IsReady errors.
	if err := db.IsReady(ctx); err == nil {
		t.Error("IsReady should fail before Migrate")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Idempotent Migrate.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate idempotent: %v", err)
	}
	// After Migrate IsReady succeeds.
	if err := db.IsReady(ctx); err != nil {
		t.Errorf("IsReady after Migrate = %v", err)
	}
}

// TestTaskRepository_LeaseRenewRelease exercises LeaseTask plus
// the lease-renewal / release helpers — they share the same lease_id /
// status state-machine and rounding them out lifts the LeaseTask line
// coverage past 58%.
func TestTaskRepository_LeaseRenewRelease(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	if err := repo.Create(ctx, &persistence.Task{ID: "task-lease", ProjectID: "p1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:            "p1",
		LeaseHolder:          "worker-1",
		LeaseDurationSeconds: 60,
	})
	if err != nil {
		t.Fatalf("LeaseTask: %v", err)
	}
	if got == nil || got.ID != "task-lease" {
		t.Fatalf("LeaseTask returned %v", got)
	}
	leaseID := got.LeaseID

	// RenewLease keeps the lease but extends the expiry.
	if err := repo.RenewLease(ctx, got.ID, *leaseID, 120); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	// RenewLease with bogus lease returns ErrLeaseNotFound.
	if err := repo.RenewLease(ctx, got.ID, "not-the-lease", 30); err != persistence.ErrLeaseNotFound {
		t.Errorf("RenewLease bogus = %v, want ErrLeaseNotFound", err)
	}

	// ReleaseLease back to QUEUED.
	if err := repo.ReleaseLease(ctx, got.ID, *leaseID, persistence.TaskStatusQueued,
		persistence.ReleaseOptions{Attempt: 2, MaxAttempts: 3}); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	// Guard: empty leaseID.
	if err := repo.ReleaseLease(ctx, got.ID, "", persistence.TaskStatusQueued, persistence.ReleaseOptions{}); err == nil {
		t.Error("ReleaseLease empty should error")
	}

	// LeaseTask with no available tasks → ErrNoTasksAvailable.
	got2, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID: "p-empty", LeaseHolder: "w", LeaseDurationSeconds: 60,
	})
	if err == nil || got2 != nil {
		t.Errorf("expected ErrNoTasksAvailable, got %v / %v", got2, err)
	}
}

// TestTaskRepository_RequeueAndTransitionVariants exercises remaining
// branches in RequeueTerminalTask and TransitionConditional opts.
func TestTaskRepository_RequeueAndTransitionVariants(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	if err := repo.Create(ctx, &persistence.Task{ID: "tx1", ProjectID: "p1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Move it to FAILED via TransitionConditional.
	ok, err := repo.TransitionConditional(ctx, "tx1",
		[]persistence.TaskStatus{persistence.TaskStatusQueued},
		persistence.TaskStatusFailed, persistence.TransitionOpts{})
	if err != nil || !ok {
		t.Fatalf("transition queued→failed: %v ok=%v", err, ok)
	}

	// RequeueTerminalTask returns true.
	ok, err = repo.RequeueTerminalTask(ctx, "tx1", 1, 5)
	if err != nil {
		t.Fatalf("RequeueTerminalTask: %v", err)
	}
	if !ok {
		t.Error("requeue should succeed for FAILED task")
	}
	// Re-requeue on now-QUEUED row is a no-op.
	ok, _ = repo.RequeueTerminalTask(ctx, "tx1", 1, 5)
	if ok {
		t.Error("re-requeue of QUEUED should be no-op")
	}

	// TransitionToCancelled happy path.
	cancelled, err := repo.TransitionToCancelled(ctx, "tx1")
	if err != nil || !cancelled {
		t.Fatalf("TransitionToCancelled: %v cancelled=%v", err, cancelled)
	}
	// On already-cancelled task, returns false.
	cancelled, err = repo.TransitionToCancelled(ctx, "tx1")
	if err != nil || cancelled {
		t.Errorf("double-cancel: err=%v cancelled=%v", err, cancelled)
	}
}

// TestIngestQueueRepository_QueueDepthAndMarkFailed plugs the
// MarkFailed/QueueDepth branches.
func TestIngestQueueRepository_QueueDepthAndMarkFailed(t *testing.T) {
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

	depth, err := repo.QueueDepth(ctx, "p1")
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("QueueDepth = %d, want 1", depth)
	}

	claimed, err := repo.ClaimBatch(ctx, "p1", 5)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d, want 1", len(claimed))
	}

	// MarkFailed with maxAttempts=1 marks it failed terminally.
	terminal, err := repo.MarkFailed(ctx, claimed[0].ID, 1, "boom")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !terminal {
		t.Error("expected terminal=true when attempts >= max")
	}
	// MarkFailed for missing id → not terminal, no error.
	terminal, err = repo.MarkFailed(ctx, "missing", 3, "")
	if err != nil {
		t.Fatalf("MarkFailed missing: %v", err)
	}
	if terminal {
		t.Error("missing row should not be terminal")
	}
	// Guards.
	if _, err := repo.MarkFailed(ctx, "", 3, ""); err == nil {
		t.Error("empty id should error")
	}
	if err := repo.MarkDone(ctx, ""); err == nil {
		t.Error("MarkDone empty id should error")
	}
	if err := repo.Enqueue(ctx, nil); err == nil {
		t.Error("Enqueue(nil) should error")
	}
	if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{}); err == nil {
		t.Error("Enqueue empty fields should error")
	}
	if _, err := repo.ClaimBatch(ctx, "", 0); err == nil {
		t.Error("ClaimBatch empty project should error")
	}
}

// TestKnowledgeEdge_DropChunkFromSources covers DropChunkFromSources.
func TestKnowledgeEdge_DropChunkFromSources(t *testing.T) {
	db := newTestDB(t)
	entRepo := sqlite.NewKnowledgeEntityRepository(db.DB)
	edgeRepo := sqlite.NewKnowledgeEdgeRepository(db.DB)
	ctx := context.Background()

	for _, e := range []*persistence.KnowledgeEntity{
		{ProjectID: "p1", CanonicalName: "a", Type: "PERSON"},
		{ProjectID: "p1", CanonicalName: "b", Type: "PERSON"},
	} {
		if err := entRepo.Insert(ctx, e); err != nil {
			t.Fatalf("entity: %v", err)
		}
	}
	a, _ := entRepo.GetByCanonical(ctx, "p1", "PERSON", "a")
	b, _ := entRepo.GetByCanonical(ctx, "p1", "PERSON", "b")

	edge := &persistence.KnowledgeEdge{
		ProjectID: "p1", FromEntity: a.ID, ToEntity: b.ID, Predicate: "KNOWS",
		Confidence: 0.9, SourceChunks: []string{"c1", "c2"},
	}
	if err := edgeRepo.UpsertEdge(ctx, edge); err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}

	n, err := edgeRepo.DropChunkFromSources(ctx, "c1")
	if err != nil {
		t.Fatalf("DropChunkFromSources: %v", err)
	}
	if n != 1 {
		t.Errorf("DropChunkFromSources = %d, want 1", n)
	}
	// Empty id rejected.
	if _, err := edgeRepo.DropChunkFromSources(ctx, ""); err == nil {
		t.Error("empty chunk id should error")
	}

	// Drop the last source chunk → edge moves to quarantined lifecycle.
	if _, err := edgeRepo.DropChunkFromSources(ctx, "c2"); err != nil {
		t.Fatalf("DropChunkFromSources c2: %v", err)
	}
	got, _ := edgeRepo.Get(ctx, edge.ID)
	if got.LifecycleState != "quarantined" {
		t.Errorf("edge lifecycle = %q, want quarantined", got.LifecycleState)
	}
}

// TestKnowledgeEntity_AliasAndLifecycle — AddAlias + UpdateLifecycle
// branches, including the "already present" no-op and not-found case.
func TestKnowledgeEntity_AliasAndLifecycle(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewKnowledgeEntityRepository(db.DB)
	ctx := context.Background()
	if err := repo.Insert(ctx, &persistence.KnowledgeEntity{
		ProjectID: "p1", Type: "PERSON", CanonicalName: "alice",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, _ := repo.GetByCanonical(ctx, "p1", "PERSON", "alice")

	if err := repo.AddAlias(ctx, got.ID, "Ally"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	// Same alias → no-op, no error.
	if err := repo.AddAlias(ctx, got.ID, "Ally"); err != nil {
		t.Errorf("AddAlias duplicate should be no-op, got %v", err)
	}
	if err := repo.AddAlias(ctx, "missing", "x"); err != persistence.ErrNotFound {
		t.Errorf("AddAlias missing = %v, want ErrNotFound", err)
	}
	if err := repo.AddAlias(ctx, "", ""); err == nil {
		t.Error("AddAlias empty args should error")
	}

	if err := repo.UpdateLifecycle(ctx, got.ID, "deprecated"); err != nil {
		t.Fatalf("UpdateLifecycle: %v", err)
	}
}

// TestArtifactRepository_HashAndScan covers GetByHash + edge cases
// for scanSqliteArtifact (mime, hash).
func TestArtifactRepository_HashAndScan(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewArtifactRepository(db.DB)
	ctx := context.Background()
	hash := "deadbeef"
	mime := "application/json"
	a := &persistence.Artifact{
		ID:                "a1",
		ProjectID:         "p1",
		Name:              "out.json",
		ArtifactClass:     persistence.ArtifactClassOutput,
		StoragePath:       "/o.json",
		ContentHashSHA256: &hash,
		MimeType:          &mime,
		SizeBytes:         int64Ptr(123),
	}
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.GetByHash(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.MimeType == nil || *got.MimeType != "application/json" {
		t.Errorf("MimeType = %v", got.MimeType)
	}
	if got.SizeBytes == nil || *got.SizeBytes != 123 {
		t.Errorf("SizeBytes = %v", got.SizeBytes)
	}
	if _, err := repo.GetByHash(ctx, "nope"); err != persistence.ErrNotFound {
		t.Errorf("GetByHash missing = %v, want ErrNotFound", err)
	}
}

// TestTaskMessage_ScanMetadata exercises the scanTaskMessage metadata +
// pointer columns branches.
func TestTaskMessage_ScanMetadata(t *testing.T) {
	db := newTestDB(t)
	taskRepo := sqlite.NewTaskRepository(db.DB)
	repo := sqlite.NewTaskMessageRepository(db.DB)
	ctx := context.Background()

	if err := taskRepo.Create(ctx, &persistence.Task{ID: "t1", ProjectID: "p1"}); err != nil {
		t.Fatalf("task: %v", err)
	}
	execID := "exec-1"
	parentID := "parent-msg"
	authorID := "u-99"
	msg := &persistence.TaskMessage{
		TaskID:      "t1",
		ExecutionID: &execID,
		ParentID:    &parentID,
		AuthorKind:  "user",
		AuthorID:    &authorID,
		MessageKind: "user",
		Content:     "hello",
		Metadata:    []byte(`{"k":"v"}`),
	}
	if err := repo.Insert(ctx, msg); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	out, err := repo.List(ctx, persistence.TaskMessageFilter{TaskID: "t1", Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("List len = %d", len(out))
	}
	got := out[0]
	if got.ExecutionID == nil || *got.ExecutionID != execID {
		t.Errorf("ExecutionID = %v", got.ExecutionID)
	}
	if got.ParentID == nil || *got.ParentID != parentID {
		t.Errorf("ParentID = %v", got.ParentID)
	}
	if got.AuthorID == nil || *got.AuthorID != authorID {
		t.Errorf("AuthorID = %v", got.AuthorID)
	}
	if string(got.Metadata) != `{"k":"v"}` {
		t.Errorf("Metadata = %q", got.Metadata)
	}

	// Guard: nil-message Insert.
	if err := repo.Insert(ctx, nil); err == nil {
		t.Error("Insert(nil) should error")
	}
	if err := repo.Insert(ctx, &persistence.TaskMessage{}); err == nil {
		t.Error("Insert empty fields should error")
	}
}

// TestTradingOrderRepository_IdentityMismatchAndList — Record refuses
// to merge when symbol/action/qty mismatch; List with all filters.
func TestTradingOrderRepository_IdentityMismatchAndList(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTradingOrderRepository(db.DB)
	ctx := context.Background()
	o := &persistence.TradingOrder{
		ID: "o1", ProjectID: "p", IdempotencyKey: "ik-1", Mode: "paper",
		Symbol: "AAPL", Action: "buy", OrderType: "MKT", Qty: 1, Status: "submitted",
	}
	if err := repo.Record(ctx, o); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Re-record with mismatched symbol → error.
	bad := *o
	bad.Symbol = "NVDA"
	if err := repo.Record(ctx, &bad); err == nil {
		t.Error("expected identity-mismatch error")
	}
	// Guards.
	if err := repo.Record(ctx, nil); err == nil {
		t.Error("nil order should error")
	}
	if err := repo.Record(ctx, &persistence.TradingOrder{}); err == nil {
		t.Error("empty order should error")
	}

	// List with all filters.
	pid := "p"
	status := "submitted"
	sym := "AAPL"
	since := time.Now().UTC().Add(-time.Hour)
	until := time.Now().UTC().Add(time.Hour)
	out, err := repo.List(ctx, persistence.TradingOrderFilter{
		ProjectID: &pid, Status: &status, Symbol: &sym, Since: &since, Until: &until,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("List len = %d", len(out))
	}
}

// TestTradingFillRepository_FilterAndSumVolume exercises the
// buildTradingFillFilter branches via List.
func TestTradingFillRepository_FilterAndSumVolume(t *testing.T) {
	db := newTestDB(t)
	orderRepo := sqlite.NewTradingOrderRepository(db.DB)
	fillRepo := sqlite.NewTradingFillRepository(db.DB)
	ctx := context.Background()

	o := &persistence.TradingOrder{
		ID: "o1", ProjectID: "p", IdempotencyKey: "ik-fill", Mode: "paper",
		Symbol: "AAPL", Action: "buy", OrderType: "MKT", Qty: 5, Status: "submitted",
	}
	if err := orderRepo.Record(ctx, o); err != nil {
		t.Fatalf("Record order: %v", err)
	}
	base := time.Now().UTC()
	for i, qty := range []float64{2, 3} {
		if err := fillRepo.Record(ctx, &persistence.TradingFill{
			ID:        "f" + string(rune('0'+i)),
			OrderID:   "o1",
			ProjectID: "p",
			Symbol:    "AAPL",
			Qty:       qty,
			Price:     150.0,
			FilledAt:  base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}

	// List filters: project + symbol + since + until.
	pid := "p"
	sym := "AAPL"
	orderID := "o1"
	since := base.Add(-time.Minute)
	until := base.Add(time.Minute)
	out, err := fillRepo.List(ctx, persistence.TradingFillFilter{
		ProjectID: &pid, OrderID: &orderID, Symbol: &sym,
		Since: &since, Until: &until, PageSize: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("List len = %d", len(out))
	}

	vol, err := fillRepo.SumVolume(ctx, persistence.TradingFillFilter{
		ProjectID: &pid, Symbol: &sym, Since: &since, Until: &until,
	})
	if err != nil {
		t.Fatalf("SumVolume: %v", err)
	}
	// SUM(qty*price) = (2+3)*150 = 750
	if vol != 750 {
		t.Errorf("SumVolume = %v, want 750", vol)
	}
}

// TestTaskRepository_DelegatedTaskShapePassesSqliteChecks guards the
// SQLite bootstrap CHECK constraints against the live enums. The
// schema previously froze creation_source at ('USER','AGENT','SYSTEM')
// and delegation_mode at the dead ('NEW_CONTEXT','EXTEND',
// 'INTERROGATE') set, so a delegated task — which the executor writes
// with creation_source='DELEGATION' and one of SEQUENTIAL/PARALLEL/
// FAN_OUT — was rejected on SQLite (`make dev` defaults to SQLite, so
// this was a real dev runtime gap, not just a test artifact). The
// accepted sets now mirror the Postgres delegation_mode enum and
// task_creation_source enum.
func TestTaskRepository_DelegatedTaskShapePassesSqliteChecks(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()

	for _, m := range []persistence.DelegationMode{
		persistence.DelegationModeSequential,
		persistence.DelegationModeParallel,
		persistence.DelegationModeFanOut,
	} {
		mode := m
		id := "task-deleg-" + string(m)
		if err := repo.Create(ctx, &persistence.Task{
			ID:             id,
			ProjectID:      "p1",
			CreationSource: persistence.TaskCreationSourceDelegation,
			DelegationMode: &mode,
		}); err != nil {
			t.Fatalf("Create delegated task (creation_source=DELEGATION, delegation_mode=%q) rejected by SQLite CHECK: %v", m, err)
		}
		got, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.DelegationMode == nil || *got.DelegationMode != m {
			t.Fatalf("round-trip delegation_mode = %v, want %q", got.DelegationMode, m)
		}
		if got.CreationSource != persistence.TaskCreationSourceDelegation {
			t.Fatalf("round-trip creation_source = %q, want DELEGATION", got.CreationSource)
		}
	}

	// The other live creation_source values the schema must accept.
	for _, cs := range []persistence.TaskCreationSource{
		persistence.TaskCreationSourceAutonomous,
		persistence.TaskCreationSourceRoute,
		persistence.TaskCreationSourceA2A,
		persistence.TaskCreationSourceCompanion,
	} {
		id := "task-cs-" + string(cs)
		if err := repo.Create(ctx, &persistence.Task{
			ID: id, ProjectID: "p1", CreationSource: cs,
		}); err != nil {
			t.Fatalf("Create with creation_source=%q rejected by SQLite CHECK: %v", cs, err)
		}
	}

	// A dead/legacy delegation_mode must now be rejected by the CHECK.
	legacy := persistence.DelegationMode("NEW_CONTEXT")
	if err := repo.Create(ctx, &persistence.Task{
		ID: "task-deleg-legacy", ProjectID: "p1", DelegationMode: &legacy,
	}); err == nil {
		t.Error("Create with dead delegation_mode 'NEW_CONTEXT' should be rejected by the CHECK")
	}
}
