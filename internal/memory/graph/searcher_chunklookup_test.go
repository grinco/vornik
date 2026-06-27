package graph

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSQLChunkLookup_ProjectAndLifecycleFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// No repo_scope → query must pin project + published + non-refuted
	// and NOT include the repo_scope clause.
	rows := sqlmock.NewRows([]string{"id", "project_id", "source_name", "content", "repo_scope"}).
		AddRow("c1", "projA", "research.md", "body", "")
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("projA", "c1").
		WillReturnRows(rows)

	l := NewSQLChunkLookup(db)
	got, err := l.LookupChunks(context.Background(), "projA", []string{"c1"}, "")
	if err != nil {
		t.Fatalf("LookupChunks: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "c1" || got[0].ProjectID != "projA" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLChunkLookup_RepoScopeClause(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// With repo_scope the query gains the scope clause and a 3rd arg.
	rows := sqlmock.NewRows([]string{"id", "project_id", "source_name", "content", "repo_scope"}).
		AddRow("c1", "p", "s.md", "x", "repoX")
	mock.ExpectQuery(regexp.QuoteMeta("repo_scope = $3 OR repo_scope = '*' OR repo_scope IS NULL")).
		WithArgs("p", "c1", "repoX").
		WillReturnRows(rows)

	l := NewSQLChunkLookup(db)
	got, err := l.LookupChunks(context.Background(), "p", []string{"c1"}, "repoX")
	if err != nil {
		t.Fatalf("LookupChunks: %v", err)
	}
	if len(got) != 1 || got[0].RepoScope != "repoX" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSQLChunkLookup_EmptyAndGuards(t *testing.T) {
	l := NewSQLChunkLookup(nil)
	if _, err := l.LookupChunks(context.Background(), "p", []string{"c1"}, ""); err == nil {
		t.Fatal("expected error for nil db")
	}
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	l2 := NewSQLChunkLookup(db)
	if _, err := l2.LookupChunks(context.Background(), "", []string{"c1"}, ""); err == nil {
		t.Fatal("expected error for empty project")
	}
	got, err := l2.LookupChunks(context.Background(), "p", nil, "")
	if err != nil || got != nil {
		t.Fatalf("empty ids should be no-op, got %v %+v", err, got)
	}
}

func TestSQLChunkLookup_DropsForeignProjectRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Defensive: even if a row with a foreign project_id somehow comes
	// back, the scan-time guard drops it.
	rows := sqlmock.NewRows([]string{"id", "project_id", "source_name", "content", "repo_scope"}).
		AddRow("c1", "projA", "s", "ok", "").
		AddRow("c2", "projB", "s", "leak", "")
	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("projA", "c1", "c2").
		WillReturnRows(rows)

	l := NewSQLChunkLookup(db)
	got, err := l.LookupChunks(context.Background(), "projA", []string{"c1", "c2"}, "")
	if err != nil {
		t.Fatalf("LookupChunks: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("foreign project row leaked: %+v", got)
	}
}
