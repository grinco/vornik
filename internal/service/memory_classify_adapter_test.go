package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory"
)

// stubChatNoop is a minimal chat.Provider that never returns a
// response — the adapter tests never trigger an LLM call (the
// classify path bails before calling because the stub repo returns
// no rows), so the stub doesn't need to be functional.
type stubChatNoop struct{}

func (stubChatNoop) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (stubChatNoop) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (stubChatNoop) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (stubChatNoop) Model() string            { return "stub" }
func (stubChatNoop) SetMetrics(*chat.Metrics) {}

// classifyAdapterTestFixture wires the minimal pieces needed to
// exercise memoryClassifyBackfillAdapter. The Repository is backed
// by sqlmock so CountRemaining + BackfillBatch can run against
// scripted DB responses; the Classifier never fires in these tests
// because the stub returns no chunks.
type classifyAdapterTestFixture struct {
	db      *sql.DB
	mock    sqlmock.Sqlmock
	repo    *memory.Repository
	adapter api.MemoryClassifyBackfiller
}

func newClassifyAdapterFixture(t *testing.T) *classifyAdapterTestFixture {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	repo := memory.NewRepository(db)
	classifier := memory.NewClassifier(stubChatNoop{}, "")
	bf := &memory.ClassifyBackfiller{
		Repo:       repo,
		Classifier: classifier,
		Logger:     zerolog.Nop(),
	}
	adapter := newMemoryClassifyBackfillAdapter(bf)
	return &classifyAdapterTestFixture{
		db:      db,
		mock:    mock,
		repo:    repo,
		adapter: adapter,
	}
}

func TestMemoryClassifyBackfillAdapter_CountRemaining_ForwardsProject(t *testing.T) {
	f := newClassifyAdapterFixture(t)
	defer func() { _ = f.db.Close() }()

	f.mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("assistant").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).
			AddRow("researcher", 5).
			AddRow("", 2))

	got, err := f.adapter.CountRemaining(context.Background(), "assistant")
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("CountRemaining: %d, want 7", got)
	}
}

func TestMemoryClassifyBackfillAdapter_CountRemaining_PropagatesError(t *testing.T) {
	f := newClassifyAdapterFixture(t)
	defer func() { _ = f.db.Close() }()
	f.mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnError(errors.New("db down"))
	if _, err := f.adapter.CountRemaining(context.Background(), "assistant"); err == nil {
		t.Fatal("want err")
	}
}

func TestMemoryClassifyBackfillAdapter_BackfillBatch_ZeroChunks(t *testing.T) {
	f := newClassifyAdapterFixture(t)
	defer func() { _ = f.db.Close() }()

	// No unclassified rows → backfiller short-circuits before any
	// classifier call. CountRemaining gets called at the end of
	// BackfillBatch to refresh the progress count.
	f.mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 5).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	f.mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}))

	res, err := f.adapter.BackfillBatch(context.Background(), "p", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	if res.Processed != 0 || res.Succeeded != 0 || res.Failed != 0 {
		t.Fatalf("counts not zero: %+v", res)
	}
}

func TestMemoryClassifyBackfillAdapter_BackfillBatch_PropagatesError(t *testing.T) {
	f := newClassifyAdapterFixture(t)
	defer func() { _ = f.db.Close() }()
	f.mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnError(errors.New("list down"))
	if _, err := f.adapter.BackfillBatch(context.Background(), "p", 5); err == nil {
		t.Fatal("want err")
	}
}

func TestMemoryClassifyBackfillAdapter_BackfillBatch_FieldMapping(t *testing.T) {
	// Verify field-by-field that the adapter copies memory.ClassifyBackfillResult
	// into api.MemoryClassifyBackfillResult without dropping a field. Empty
	// chunk list keeps Processed=0; we exercise Remaining specifically.
	f := newClassifyAdapterFixture(t)
	defer func() { _ = f.db.Close() }()
	f.mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	f.mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("", 17))

	res, err := f.adapter.BackfillBatch(context.Background(), "p", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining != 17 {
		t.Fatalf("Remaining not forwarded: %+v", res)
	}
}
