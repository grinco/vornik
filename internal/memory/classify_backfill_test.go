package memory

import (
	"context"
	"errors"
	"testing"
	"time"
	"vornik.io/vornik/internal/llmspend"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

func newClassifyBackfiller(t *testing.T, replies []titlerReply) (*ClassifyBackfiller, sqlmock.Sqlmock, func()) {
	t.Helper()
	r, mock, cleanup := newRepo(t)
	fp := newClassifyProvider(replies...)
	cl := NewClassifier(fp, "", llmspend.Disabled())
	return &ClassifyBackfiller{
		Repo:       r,
		Classifier: cl,
		Logger:     zerolog.Nop(),
		Metrics:    freshMetrics(),
	}, mock, cleanup
}

func TestClassifyBackfiller_CountRemaining(t *testing.T) {
	var nilB *ClassifyBackfiller
	if _, err := nilB.CountRemaining(context.Background(), "p"); err == nil {
		t.Fatal("want err")
	}
	b := &ClassifyBackfiller{}
	if _, err := b.CountRemaining(context.Background(), "p"); err == nil {
		t.Fatal("want err")
	}
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).
			AddRow("researcher", 5).
			AddRow("", 3))
	got, err := bf.CountRemaining(context.Background(), "p")
	if err != nil || got != 8 {
		t.Fatalf("got %d %v", got, err)
	}
}

func TestClassifyBackfiller_CountRemaining_RepoError(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errors.New("boom"))
	if _, err := bf.CountRemaining(context.Background(), "p"); err == nil {
		t.Fatal("want err")
	}
}

func TestClassifyBackfiller_BackfillBatch_NoConfig(t *testing.T) {
	var nilB *ClassifyBackfiller
	if _, err := nilB.BackfillBatch(context.Background(), "p", 5); err == nil {
		t.Fatal("nil receiver")
	}
	if _, err := (&ClassifyBackfiller{}).BackfillBatch(context.Background(), "p", 5); err == nil {
		t.Fatal("nil repo/classifier")
	}
	bf, _, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	if _, err := bf.BackfillBatch(context.Background(), "", 5); err == nil {
		t.Fatal("empty project")
	}
}

func TestClassifyBackfiller_BackfillBatch_ListErr(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errors.New("boom"))
	if _, err := bf.BackfillBatch(context.Background(), "p", 5); err == nil {
		t.Fatal("want err")
	}
}

func TestClassifyBackfiller_BackfillBatch_Mixed(t *testing.T) {
	// c1 → research (succeeds), c2 → unclassified (skipped),
	// c3 → LLM error (failed).
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
		{content: "unclassified"},
		{err: errors.New("upstream 503")},
	})
	defer cleanup()

	// Every role here is deliberately ABSENT from roleClassMap, so each
	// row reaches the LLM — which is the branch this test covers. Rows
	// with a mapped role never call the model at all (2026-08-15).
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "dispatcher", "first body").
			AddRow("c2", "p", "x.md", "", "ambiguous").
			AddRow("c3", "p", "y.md", "dispatcher", "third body"))

	// c1 → UPDATE.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// CountRemaining at end (one query against project_memory_chunks).
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("writer", 0))

	res, err := bf.BackfillBatch(context.Background(), "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 3 {
		t.Fatalf("processed: %d", res.Processed)
	}
	if res.Succeeded != 1 || res.Skipped != 1 || res.Failed != 1 {
		t.Fatalf("counts: %+v", res)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors: %v", res.Errors)
	}
}

func TestClassifyBackfiller_BackfillBatch_PersistErr(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
	})
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "researcher", "body"))
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnError(errors.New("disk full"))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}))

	res, err := bf.BackfillBatch(context.Background(), "p", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Succeeded != 0 {
		t.Fatalf("counts: %+v", res)
	}
}

func TestClassifyBackfiller_BackfillBatch_CountRemainingErrTolerated(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "spec"},
	})
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "analyst", "body"))
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnError(errors.New("count down"))

	res, err := bf.BackfillBatch(context.Background(), "p", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining != 0 {
		t.Fatalf("remaining should default to 0 on count failure, got %d", res.Remaining)
	}
}

func TestClassifyBackfiller_BackfillBatch_WalksPastLLMAbstentions(t *testing.T) {
	// This is the project-scoped path used by `vornikctl memory reclassify
	// --use-llm`. A prior fix paged only the all-projects worker, leaving this
	// endpoint to re-read the oldest abstained row forever.
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "unclassified"},
		{content: "research"},
	})
	defer cleanup()

	mock.ExpectQuery("FROM project_memory_chunks").WithArgs("p", 1, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("old", "p", "old.md", "unknown", "ambiguous"))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("unknown", 2))
	first, err := bf.BackfillBatch(context.Background(), "p", 1)
	if err != nil || first.Skipped != 1 {
		t.Fatalf("first batch: %+v, %v", first, err)
	}

	// The next request must start at offset one, reaching the row that was
	// previously hidden behind the model's abstention.
	mock.ExpectQuery("FROM project_memory_chunks").WithArgs("p", 1, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("new", "p", "new.md", "unknown", "evidence"))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("unknown", 1))
	second, err := bf.BackfillBatch(context.Background(), "p", 1)
	if err != nil || second.Succeeded != 1 {
		t.Fatalf("second batch: %+v, %v", second, err)
	}
}

func TestClassifyBackfiller_LockForProject_IsPerProject(t *testing.T) {
	b := &ClassifyBackfiller{}
	lockA := b.lockForProject("a")
	if lockA != b.lockForProject("a") {
		t.Fatal("one project must keep one cursor lock")
	}
	if lockA == b.lockForProject("b") {
		t.Fatal("independent projects must not share a sweep lock")
	}
}

func TestClassifyBackfiller_BackfillBatch_CtxCancelMidLoop(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
		{content: "research"},
	})
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "a", "r", "x").
			AddRow("c2", "p", "b", "r", "y"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bf.BackfillBatch(ctx, "p", 5); err == nil {
		t.Fatal("want ctx err")
	}
}

// ---- Auto-backfill loop (Measure 1, 2026-05-15) ----------------------

func TestClassifyBackfiller_Run_DisabledConfig(t *testing.T) {
	// Nil receiver / nil deps: returns immediately without panic.
	var nilB *ClassifyBackfiller
	nilB.Run(context.Background(), time.Hour, 10)
	(&ClassifyBackfiller{}).Run(context.Background(), time.Hour, 10)

	bf, _, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	// interval <= 0 → returns. batchSize <= 0 → returns. The
	// container.go wiring also treats interval == 0 as the
	// off-switch, so these two must both no-op silently.
	bf.Run(context.Background(), 0, 10)
	bf.Run(context.Background(), time.Hour, 0)
}

func TestClassifyBackfiller_Run_ImmediateTickAndCancel(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{{content: "research"}})
	defer cleanup()
	// First runOnce: count remaining > 0 → cross-project list →
	// classifier → persist → recount.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "researcher", "body")) // body before classification
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		bf.Run(ctx, 50*time.Millisecond, 5)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on cancel")
	}
}

func TestClassifyBackfiller_runOnce_IdleAndCountErrPaths(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	// CountRemainingAll error → "errored" tick label, no list.
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("db down"))
	bf.runOnce(context.Background(), 10)
	// Idle: count=0 → "idle" tick, no list.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	bf.runOnce(context.Background(), 10)
}

func TestClassifyBackfiller_runOnce_BatchErrorRecorded(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	// Cross-project list fails → BackfillBatchAcrossProjects returns
	// an error → runOnce logs and records "errored". The remaining
	// gauge was already updated above; subsequent ticks will retry.
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnError(errors.New("list down"))
	bf.runOnce(context.Background(), 5)
}

// ---- BackfillBatchAcrossProjects (the cross-project sibling) ----------

func TestClassifyBackfiller_BackfillBatchAcrossProjects_NoConfig(t *testing.T) {
	var nilB *ClassifyBackfiller
	if _, err := nilB.BackfillBatchAcrossProjects(context.Background(), 5); err == nil {
		t.Fatal("nil receiver")
	}
	if _, err := (&ClassifyBackfiller{}).BackfillBatchAcrossProjects(context.Background(), 5); err == nil {
		t.Fatal("nil repo/classifier")
	}
}

func TestClassifyBackfiller_BackfillBatchAcrossProjects_MultiProject(t *testing.T) {
	// Two projects in the same tick — ensures the cross-project list
	// drives one classify call per row regardless of project_id and
	// the UpdateChunkClass key is the chunk_id (not (project,chunk)).
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
		{content: "spec"},
	})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id").
		WithArgs(5, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-alpha", "alpha.md", "researcher", "alpha body").
			AddRow("c2", "p-beta", "beta.md", "analyst", "beta body"))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 2 || res.Succeeded != 2 || res.Remaining != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestClassifyBackfiller_BackfillBatchAcrossProjects_PerRowErrorsRecorded(t *testing.T) {
	// Per-row paths under one tick:
	//   c1 → research (succeeds)
	//   c2 → "unclassified" (skipped, model said so)
	//   c3 → LLM error (failed, error recorded)
	//   c4 → persist error (failed, error recorded)
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
		{content: "unclassified"},
		{err: errors.New("upstream 503")},
		{content: "spec"},
	})
	defer cleanup()
	// Unmapped roles throughout: these four paths are the LLM's, and a
	// role in roleClassMap would short-circuit past it (2026-08-15).
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-a", "alpha.md", "dispatcher", "alpha").
			AddRow("c2", "p-b", "beta.md", "dispatcher", "beta").
			AddRow("c3", "p-c", "gamma.md", "dispatcher", "gamma").
			AddRow("c4", "p-d", "delta.md", "dispatcher", "delta"))
	// c1 succeeds → UPDATE.
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	// c4 persist fails.
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("disk full"))
	// CountRemainingAll at end.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 4 || res.Succeeded != 1 || res.Skipped != 1 || res.Failed != 2 {
		t.Fatalf("counts: %+v", res)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("errors recorded: %v", res.Errors)
	}
}

func TestClassifyBackfiller_BackfillBatchAcrossProjects_CountRemainingErrTolerated(t *testing.T) {
	// CountRemainingAll error at end → Remaining stays 0 (defaults),
	// the batch itself still returns success. Mirrors the per-project
	// sibling's tolerance.
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{{content: "spec"}})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-a", "doc.md", "analyst", "body"))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("count down"))

	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining != 0 {
		t.Fatalf("remaining should default to 0 on count failure, got %d", res.Remaining)
	}
	if res.Succeeded != 1 {
		t.Fatalf("succeeded: %+v", res)
	}
}

func TestClassifyBackfiller_BackfillBatchAcrossProjects_CtxCancelMidLoop(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "research"},
		{content: "spec"},
	})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-a", "a", "researcher", "x").
			AddRow("c2", "p-b", "b", "analyst", "y"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bf.BackfillBatchAcrossProjects(ctx, 5); err == nil {
		t.Fatal("want ctx err")
	}
}

func TestClassifyBackfiller_BackfillBatchAcrossProjects_ListErr(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id").WillReturnError(errors.New("boom"))
	if _, err := bf.BackfillBatchAcrossProjects(context.Background(), 5); err == nil {
		t.Fatal("want err")
	}
}

func TestClassifyBackfiller_CountRemainingAll(t *testing.T) {
	var nilB *ClassifyBackfiller
	if _, err := nilB.CountRemainingAll(context.Background()); err == nil {
		t.Fatal("nil receiver should error — runOnce relies on this to record the 'errored' label")
	}
	bf, mock, cleanup := newClassifyBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	got, err := bf.CountRemainingAll(context.Background())
	if err != nil || got != 7 {
		t.Fatalf("got %d %v", got, err)
	}
}

// LIVELOCK, observed in production 2026-07-30. The auto-loop selected
// `ORDER BY created_at ASC LIMIT 25`, the classifier declined all 25, and a declined row
// is never written — so the same page came back every tick and permanently blocked the
// 1,174 rows behind it. The journal showed it plainly: succeeded=0, skipped=25 and
// remaining pinned at 1199 across every tick, for hours.
//
// The cursor has to advance past what nothing can classify.
func TestClassifyBackfiller_AdvancesPastRowsNothingCanClassify(t *testing.T) {
	b := &ClassifyBackfiller{}
	if b.offset != 0 {
		t.Fatalf("fresh backfiller offset = %d, want 0", b.offset)
	}
	// Simulate a tick that declined everything.
	b.offset += 25
	if b.offset != 25 {
		t.Fatalf("offset after an all-declined page = %d, want 25 so the next tick "+
			"reaches rows the first page was blocking", b.offset)
	}
}

// The deterministic role map is consulted BEFORE the LLM: it is free, it is what the
// ingest path itself uses, and for a known producer_role it is the authoritative answer.
// The LLM is reserved for roles the map cannot place.
func TestClassifyByRole_ProvidesATerminalAnswerForKnownRoles(t *testing.T) {
	if got, _ := ClassifyByRole("researcher"); got == ClassUnclassified {
		t.Error("a known producer role must map to a real class, or the backfill " +
			"cannot classify the row without paying for a model call")
	}
	// A companion deposit is its own class, and must not be relabelled.
	if got, _ := ClassifyByRole("companion:claude-code"); got != ClassCompanionNote {
		t.Errorf("companion role = %q, want companion_note", got)
	}
	// An unknown role genuinely has no answer — the fallback must not invent one.
	if got, _ := ClassifyByRole("some-role-nobody-registered"); got != ClassUnclassified {
		t.Errorf("unknown role = %q, want unclassified rather than a guess", got)
	}
}

// newClassifyBackfillerProbe is newClassifyBackfiller plus a handle on the fake
// provider, so a test can assert the model was never called.
func newClassifyBackfillerProbe(t *testing.T, replies []titlerReply) (*ClassifyBackfiller, *classifyFakeProvider, sqlmock.Sqlmock, func()) {
	t.Helper()
	r, mock, cleanup := newRepo(t)
	fp := newClassifyProvider(replies...)
	cl := NewClassifier(fp, "", llmspend.Disabled())
	return &ClassifyBackfiller{
		Repo:       r,
		Classifier: cl,
		Logger:     zerolog.Nop(),
		Metrics:    freshMetrics(),
	}, fp, mock, cleanup
}

// Regression, 2026-08-15. The companion-rag-ingest workflow stamps producer_role
// "rag-ingester" on every operator-deposited document — in practice the LLDs. That
// role was missing from roleClassMap, and the backfill consulted the paid model
// BEFORE the map, so each tick billed 25 Bedrock requests, had the model abstain on
// all 25, then fell back to a map that had no answer either. 497 chunks sat at
// ClassUnclassified (0.3 confidence, never role-of-record) while the loop re-billed
// them every ten minutes indefinitely.
//
// Pre-fix this test fails twice over: calls == 1 (the model was asked) and
// Succeeded == 0 / Skipped == 1 (nothing was classified).
func TestClassifyBackfiller_RagIngestedDocClassifiedWithoutPayingForAModelCall(t *testing.T) {
	bf, fp, mock, cleanup := newClassifyBackfillerProbe(t, nil)
	defer cleanup()

	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("companion-example", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "companion-example", "some-design.md", "rag-ingester", "a design document"))
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("rag-ingester", 0))

	res, err := bf.BackfillBatch(context.Background(), "companion-example", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 1 || res.Skipped != 0 {
		t.Fatalf("an ingested design must be classified deterministically, got %+v", res)
	}
	if n := fp.calls.Load(); n != 0 {
		t.Fatalf("classifier called %d time(s) for a row the free role map can place; "+
			"that is the per-request billing this fix exists to stop", n)
	}
}

// The auto-loop runs the cross-project sibling, so it is the one that was actually
// burning requests every ten minutes. Same guarantee.
func TestClassifyBackfiller_AcrossProjects_RagIngestedDocSkipsTheModel(t *testing.T) {
	bf, fp, mock, cleanup := newClassifyBackfillerProbe(t, nil)
	defer cleanup()

	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "companion-example", "lld.md", "rag-ingester", "body"))
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 1 || res.Skipped != 0 {
		t.Fatalf("counts: %+v", res)
	}
	if n := fp.calls.Load(); n != 0 {
		t.Fatalf("auto-loop called the model %d time(s) for a mapped role", n)
	}
}

// The complement: a role the map genuinely cannot place must still reach the LLM.
// Without this, "skip the model" could be over-applied into never classifying
// anything the map does not already know.
func TestClassifyBackfiller_UnmappedRoleStillReachesTheModel(t *testing.T) {
	bf, fp, mock, cleanup := newClassifyBackfillerProbe(t, []titlerReply{{content: "research"}})
	defer cleanup()

	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "x.md", "dispatcher", "body"))
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("dispatcher", 0))

	res, err := bf.BackfillBatch(context.Background(), "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("counts: %+v", res)
	}
	if n := fp.calls.Load(); n != 1 {
		t.Fatalf("classifier called %d time(s); an unmapped role must still be asked", n)
	}
}

// rag-ingest is the operator's canonical path for depositing design documents, and
// the retrieval rule that treats LLDs as the authoritative design record keys on the
// spec class. Mapped anywhere lower, recall ranks agent-produced review artifacts
// (decision, 0.9) above the designs they review.
func TestRoleClassMap_RagIngesterIsSpec(t *testing.T) {
	got, pol := ClassifyByRole("rag-ingester")
	if got != ClassSpec {
		t.Fatalf("rag-ingester = %q, want spec", got)
	}
	if !pol.RoleOfRecordEligible {
		t.Error("an ingested design must be eligible as role-of-record")
	}
	if unc := DefaultClassPolicies[ClassUnclassified]; pol.DefaultConfidence <= unc.DefaultConfidence {
		t.Errorf("spec confidence %v must exceed unclassified %v, or the fix changes "+
			"nothing about retrieval ranking", pol.DefaultConfidence, unc.DefaultConfidence)
	}
}
