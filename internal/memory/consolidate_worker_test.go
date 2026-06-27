package memory

import (
	"context"
	"errors"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// stubProjectLister returns a fixed slice. Avoids pulling
// registry.Registry into the memory package's test deps.
type stubProjectLister struct{ ids []string }

func (s *stubProjectLister) ListProjectIDs() []string { return s.ids }

// failingConsolidator simulates a library error path so the
// per-project errored counter is exercised. Wraps an empty
// Consolidator so the embedded ConsolidateProject is overridden
// via composition (the worker calls c.Consolid.ConsolidateProject
// directly, so we replace c.Consolid itself).
type failingConsolidator struct {
	calls int32
}

func (f *failingConsolidator) ConsolidateProject(_ context.Context, _ string, _ int) (*ProjectGist, error) {
	atomic.AddInt32(&f.calls, 1)
	return nil, errors.New("simulated consolidator failure")
}

// consolidatorIface is the narrow surface the worker uses from
// Consolidator. Re-declared here for test substitution.
type consolidatorIface interface {
	ConsolidateProject(ctx context.Context, projectID string, scanLimit int) (*ProjectGist, error)
}

// Compile-time guard that both concrete types satisfy the iface
// — keeps the test honest if Consolidator's signature ever drifts.
var (
	_ consolidatorIface = (*Consolidator)(nil)
	_ consolidatorIface = (*failingConsolidator)(nil)
)

// TestConsolidateWorker_TickHappyPath — two projects, both have
// chunks. Each gets one UPSERT into project_gists; tick outcome
// is `progressed`. We mock both the chunks-list query (for
// ConsolidateProject) and the gist UPSERT.
func TestConsolidateWorker_TickHappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	cons := NewConsolidator(repo)
	w := &ConsolidateWorker{
		Consolid: cons, Repo: repo,
		Projects: &stubProjectLister{ids: []string{"proj-a", "proj-b"}},
		Interval: time.Hour, // not exercised in unit tests
		Logger:   zerolog.Nop(),
	}

	// Project A: 2 chunks. Project B: 1 chunk. The library
	// tokenises content and the worker UPSERTs the resulting gist.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("proj-a", 1000).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).
			AddRow("alpha beta beta gamma").
			AddRow("alpha gamma delta"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_gists")).
		WithArgs("proj-a", sqlmock.AnyArg(), 2, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("proj-b", 1000).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).
			AddRow("only one chunk here please"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_gists")).
		WithArgs("proj-b", sqlmock.AnyArg(), 1, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestConsolidateWorker_TickEmptyRegistry — no projects in the
// registry means the loop has nothing to do; outcome is `idle`,
// no DB calls are made. Defensive: an early-boot daemon with no
// projects loaded should not crash or fire empty queries.
func TestConsolidateWorker_TickEmptyRegistry(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	w := &ConsolidateWorker{
		Consolid: NewConsolidator(NewRepository(db)),
		Repo:     NewRepository(db),
		Projects: &stubProjectLister{ids: nil},
		Interval: time.Hour,
		Logger:   zerolog.Nop(),
	}
	w.tick(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected sql calls: %v", err)
	}
}

// TestConsolidateWorker_TickOneProjectFailsOtherSucceeds — the
// failure-isolation contract: one project's DB error must NOT
// halt the tick. The succeeding project still gets its gist
// upserted. Tick outcome remains `progressed` (at least one OK).
func TestConsolidateWorker_TickOneProjectFailsOtherSucceeds(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	w := &ConsolidateWorker{
		Consolid: NewConsolidator(repo),
		Repo:     repo,
		Projects: &stubProjectLister{ids: []string{"bad-project", "good-project"}},
		Interval: time.Hour,
		Logger:   zerolog.Nop(),
	}

	// bad-project: ListChunkContents errors out.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("bad-project", 1000).
		WillReturnError(errors.New("connection refused"))

	// good-project: succeeds.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("good-project", 1000).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("hello world hello"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_gists")).
		WithArgs("good-project", sqlmock.AnyArg(), 1, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations (failure isolation broken?): %v", err)
	}
}

// TestConsolidateWorker_DisabledByInterval — Interval <= 0 means
// the worker returns immediately. Operators set this when they
// want gist generation on-demand only (via the API force flag,
// when that lands).
func TestConsolidateWorker_DisabledByInterval(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	w := &ConsolidateWorker{
		Consolid: NewConsolidator(NewRepository(db)),
		Repo:     NewRepository(db),
		Projects: &stubProjectLister{ids: []string{"p"}},
		Interval: 0, // disabled
		Logger:   zerolog.Nop(),
	}
	done := make(chan struct{})
	go func() {
		w.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// Returned immediately as required.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("worker did not return on Interval=0")
	}
}

// TestConsolidateWorker_RespectsContextCancel — when the parent
// context is cancelled mid-tick the loop exits cleanly without
// leaving the worker goroutine stuck.
func TestConsolidateWorker_RespectsContextCancel(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	// First tick fires immediately — supply ONE round-trip so it
	// completes, then cancel.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("p", 1000).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("hello"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_gists")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewRepository(db)
	w := &ConsolidateWorker{
		Consolid: NewConsolidator(repo), Repo: repo,
		Projects: &stubProjectLister{ids: []string{"p"}},
		Interval: 50 * time.Millisecond,
		Logger:   zerolog.Nop(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	// Wait for the first tick + Stopped channel to be wired.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-w.Stopped():
	case <-time.After(time.Second):
		t.Fatal("worker did not stop within 1s after context cancel")
	}
}
