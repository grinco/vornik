package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

func newClassifyBackfiller(t *testing.T, replies []titlerReply) (*ClassifyBackfiller, sqlmock.Sqlmock, func()) {
	t.Helper()
	r, mock, cleanup := newRepo(t)
	fp := newClassifyProvider(replies...)
	cl := NewClassifier(fp, "")
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

	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "researcher", "first body").
			AddRow("c2", "p", "x.md", "", "ambiguous").
			AddRow("c3", "p", "y.md", "writer", "third body"))

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
		WithArgs(5).
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
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-a", "alpha.md", "researcher", "alpha").
			AddRow("c2", "p-b", "beta.md", "writer", "beta").
			AddRow("c3", "p-c", "gamma.md", "analyst", "gamma").
			AddRow("c4", "p-d", "delta.md", "analyst", "delta"))
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
