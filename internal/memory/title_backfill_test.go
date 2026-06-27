package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

func newBackfiller(t *testing.T, replies []titlerReply) (*TitleBackfiller, sqlmock.Sqlmock, func()) {
	t.Helper()
	r, mock, cleanup := newRepo(t)
	fp := &titlerFakeProvider{replies: replies}
	tr := NewTitler(fp, "")
	return &TitleBackfiller{
		Repo:    r,
		Titler:  tr,
		Logger:  zerolog.Nop(),
		Metrics: freshMetrics(),
	}, mock, cleanup
}

func TestCountRemaining(t *testing.T) {
	var nilB *TitleBackfiller
	if _, err := nilB.CountRemaining(context.Background()); err == nil {
		t.Fatal("want err")
	}
	b := &TitleBackfiller{}
	if _, err := b.CountRemaining(context.Background()); err == nil {
		t.Fatal("want err")
	}
	bf, mock, cleanup := newBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	n, err := bf.CountRemaining(context.Background())
	if err != nil || n != 7 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestBackfillBatch_NoConfig(t *testing.T) {
	var nilB *TitleBackfiller
	if _, err := nilB.BackfillBatch(context.Background(), 5); err == nil {
		t.Fatal("want err")
	}
	if _, err := (&TitleBackfiller{}).BackfillBatch(context.Background(), 5); err == nil {
		t.Fatal("want err")
	}
}

func TestBackfillBatch_ListErr(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, nil)
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnError(errors.New("boom"))
	if _, err := bf.BackfillBatch(context.Background(), 5); err == nil {
		t.Fatal("want err")
	}
}

func TestBackfillBatch_HappySuccessAndSkipAndFail(t *testing.T) {
	// c2's content is whitespace-only → Titler.Title returns ("", nil) → Skipped.
	// c3 produces an LLM error → Failed.
	bf, mock, cleanup := newBackfiller(t, []titlerReply{
		{content: "Topic One"},        // c1 succeeds
		{err: errors.New("llm down")}, // c3 fails (c2 short-circuits before LLM call)
	})
	defer cleanup()

	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WithArgs(10). // default
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "alpha").
			AddRow("c2", "p", "s", "   ").
			AddRow("c3", "p", "s", "gamma"))

	// Succeed → UpdateContentTitle for c1.
	mock.ExpectExec("UPDATE project_memory_chunks SET content_title").
		WithArgs("Topic One", "c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// CountRemaining at end.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	res, err := bf.BackfillBatch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Processed != 3 || res.Succeeded != 1 || res.Skipped != 1 || res.Failed != 1 || res.Remaining != 2 {
		t.Fatalf("got %+v", res)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors: %v", res.Errors)
	}
}

func TestBackfillBatch_UpdateErrorRecorded(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{{content: "Topic"}})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "x"))
	mock.ExpectExec("UPDATE project_memory_chunks SET content_title").
		WillReturnError(errors.New("disk full"))
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	res, err := bf.BackfillBatch(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Succeeded != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestBackfillBatch_CountRemainingErrLeavesZero(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{{content: "X"}})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "x"))
	mock.ExpectExec("UPDATE project_memory_chunks SET content_title").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("x"))
	res, err := bf.BackfillBatch(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining != 0 {
		t.Fatalf("remaining: %d", res.Remaining)
	}
}

func TestBackfillBatch_CtxCancelMidLoop(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{{content: "X"}, {content: "Y"}})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "x").
			AddRow("c2", "p", "s", "y"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bf.BackfillBatch(ctx, 5); err == nil {
		t.Fatal("expected ctx err")
	}
}

func TestRun_DisabledConfig(t *testing.T) {
	// Nil receiver / nil repo: returns immediately.
	var nilB *TitleBackfiller
	nilB.Run(context.Background(), time.Hour, 10)
	(&TitleBackfiller{}).Run(context.Background(), time.Hour, 10)

	bf, _, cleanup := newBackfiller(t, nil)
	defer cleanup()
	// interval <= 0 → returns.
	bf.Run(context.Background(), 0, 10)
	// batchSize <= 0 → returns.
	bf.Run(context.Background(), time.Hour, 0)
}

func TestRun_ImmediateTickAndCancel(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{{content: "X"}})
	defer cleanup()
	// First runOnce: count remaining > 0 → backfill batch.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "x"))
	mock.ExpectExec("UPDATE project_memory_chunks SET content_title").
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

func TestRunOnce_IdleAndErrPaths(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, nil)
	defer cleanup()
	// CountRemaining error path.
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("x"))
	bf.runOnce(context.Background(), 10)

	// Idle: count=0.
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	bf.runOnce(context.Background(), 10)
}

func TestRunOnce_BatchError(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{{content: "X"}})
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WillReturnError(errors.New("list down"))
	bf.runOnce(context.Background(), 5)
}
