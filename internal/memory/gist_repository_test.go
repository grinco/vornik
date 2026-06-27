package memory

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestUpsertGist_NewRow — the worker's happy path: a project's
// first tick lands a fresh row.
func TestUpsertGist_NewRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	generated := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	g := &PersistedGist{
		ProjectID:     "assistant",
		Terms:         []TermFrequency{{Term: "ibkr", Count: 12}, {Term: "trading", Count: 7}},
		ChunksScanned: 42,
		GeneratedAt:   generated,
		DurationMs:    17,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_gists")).
		WithArgs("assistant", sqlmock.AnyArg(), 42, generated, 17).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpsertGist(context.Background(), g); err != nil {
		t.Fatalf("UpsertGist: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestUpsertGist_RejectsEmptyProject — the table's PK is
// project_id; an empty string would silently land an orphan row.
// Validate up front instead.
func TestUpsertGist_RejectsEmptyProject(t *testing.T) {
	repo := NewRepository(nil)
	err := repo.UpsertGist(context.Background(), &PersistedGist{})
	if err == nil {
		t.Fatal("expected error on empty project id; got nil")
	}
}

// TestGetGist_NotFoundMapsToSentinel — sql.ErrNoRows must surface
// as ErrGistNotFound so the API layer can render an empty-state
// without bubbling 500s.
func TestGetGist_NotFoundMapsToSentinel(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM project_gists")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.GetGist(context.Background(), "missing")
	if !errors.Is(err, ErrGistNotFound) {
		t.Errorf("err = %v, want ErrGistNotFound", err)
	}
}

// TestGetGist_RoundTripsTerms — the JSONB column must round-trip
// the []TermFrequency slice. We supply the raw JSON the worker
// would have written and assert GetGist parses it back to the
// same slice.
func TestGetGist_RoundTripsTerms(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	termsJSON := []byte(`[{"Term":"ibkr","Count":12},{"Term":"trading","Count":7}]`)
	generated := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_gists")).
		WithArgs("assistant").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at", "duration_ms",
			"narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("assistant", termsJSON, 42, generated, 17, nil, nil, nil))

	got, err := repo.GetGist(context.Background(), "assistant")
	if err != nil {
		t.Fatalf("GetGist: %v", err)
	}
	if got.ProjectID != "assistant" || got.ChunksScanned != 42 || got.DurationMs != 17 {
		t.Errorf("metadata wrong: %+v", got)
	}
	if len(got.Terms) != 2 || got.Terms[0].Term != "ibkr" || got.Terms[1].Count != 7 {
		t.Errorf("terms wrong: %+v", got.Terms)
	}
	if got.Narrative != "" || got.NarrativeModel != "" {
		t.Errorf("expected zero narrative fields when columns are NULL: %+v", got)
	}
}

// ---- slice-3 LLM-tier narrative ----

func TestGetGist_PopulatesNarrativeWhenPresent(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	termsJSON := []byte(`[{"Term":"x","Count":1}]`)
	generated := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	narGen := time.Date(2026, 5, 19, 12, 5, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_gists")).
		WithArgs("p").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at", "duration_ms",
			"narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("p", termsJSON, 5, generated, 1, "the summary", "gpt-oss-20b", narGen))

	got, err := repo.GetGist(context.Background(), "p")
	if err != nil {
		t.Fatalf("GetGist: %v", err)
	}
	if got.Narrative != "the summary" {
		t.Errorf("Narrative = %q", got.Narrative)
	}
	if got.NarrativeModel != "gpt-oss-20b" {
		t.Errorf("NarrativeModel = %q", got.NarrativeModel)
	}
	if !got.NarrativeGeneratedAt.Equal(narGen) {
		t.Errorf("NarrativeGeneratedAt = %v, want %v", got.NarrativeGeneratedAt, narGen)
	}
}

func TestUpsertNarrative_UpdatesNarrativeColumns(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	at := time.Now().UTC()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_gists")).
		WithArgs("p", "summary text", "gpt-oss-20b", at).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpsertNarrative(context.Background(), "p", "summary text", "gpt-oss-20b", at); err != nil {
		t.Fatalf("UpsertNarrative: %v", err)
	}
}

func TestUpsertNarrative_NotFoundWhenNoRow(t *testing.T) {
	// Affected rows = 0 means the LLM-free tier hasn't UPSERTed
	// the row yet. Surfaces as ErrGistNotFound so the worker can
	// skip silently instead of treating it as a real failure.
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_gists")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.UpsertNarrative(context.Background(), "p", "x", "m", time.Now())
	if !errors.Is(err, ErrGistNotFound) {
		t.Errorf("err = %v, want ErrGistNotFound", err)
	}
}

func TestUpsertNarrative_RejectsEmptyProject(t *testing.T) {
	repo := NewRepository(nil)
	if err := repo.UpsertNarrative(context.Background(), "", "x", "m", time.Now()); err == nil {
		t.Fatal("expected error on empty project id")
	}
}
