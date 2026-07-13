package memory

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestListChunkIDsByFailedProducer_JoinsThroughExecution pins the retro-clean
// query (LLD 2026-07-12-rag-ingest-producer-success-gate §5): candidate chunks
// are those whose producing EXECUTION ended unsuccessfully. The join is
// chunks.artifact_id → artifacts.execution_id → executions.status, so a
// task's FAILED-exec-1 chunks are selected while its COMPLETED-exec-2 (retry)
// chunks are not. Companion-note / uploaded-doc chunks (empty task_id) are
// never selected.
func TestListChunkIDsByFailedProducer_JoinsThroughExecution(t *testing.T) {
	repo, mock, done := newRepo(t)
	defer done()

	rows := sqlmock.NewRows([]string{"id"}).AddRow("c1").AddRow("c2")
	// The query must join executions and filter on FAILED/CANCELLED, scoped
	// by project, excluding empty task_id chunks.
	mock.ExpectQuery("FROM project_memory_chunks.*JOIN artifacts.*JOIN executions.*status IN").
		WithArgs("proj-1").
		WillReturnRows(rows)

	got, err := repo.ListChunkIDsByFailedProducer(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("ListChunkIDsByFailedProducer: %v", err)
	}
	if len(got) != 2 || got[0] != "c1" || got[1] != "c2" {
		t.Fatalf("got %v, want [c1 c2]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestListChunkIDsByFailedProducer_EmptyProject rejects an empty project id
// (the query is always project-scoped — the IDOR guard).
func TestListChunkIDsByFailedProducer_EmptyProject(t *testing.T) {
	repo, _, done := newRepo(t)
	defer done()
	if _, err := repo.ListChunkIDsByFailedProducer(context.Background(), ""); err == nil {
		t.Fatal("expected an error for empty project id")
	}
}

// TestListChunkIDsByFailedProducer_NoRows returns an empty slice (not an
// error) when nothing matches — a clean project.
func TestListChunkIDsByFailedProducer_NoRows(t *testing.T) {
	repo, mock, done := newRepo(t)
	defer done()
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("proj-clean").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	got, err := repo.ListChunkIDsByFailedProducer(context.Background(), "proj-clean")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no chunk ids, got %v", got)
	}
}
