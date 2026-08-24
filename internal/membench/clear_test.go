package membench

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The clear this package promised and never performed.
//
// §5.8 says "a benchmark run writes and clears bulk memory", the flag is named
// --i-know-this-wipes, CheckDestructiveTarget's refusal text promises the run
// "will bulk-write and clear", and VornikSystem.Teardown's comment said cleanup
// happened "once per run at the database level". Four statements agreeing, and
// no code deleted anything. Measured cost over three runs of the same 120
// items: admitted deposits 426 -> 426 -> 209 as the store filled, two real
// items eventually losing their whole haystack to dedup_hash, and accuracy
// moving 0.692 -> 0.750 once the store was wiped by hand.

func TestClearBenchmarkStore_deletesTheWholeRetrievableSurface(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Order is load-bearing: mentions reference entities, edges and entities
	// are independent of the chunk cascade, and chunks come last so a failure
	// part-way leaves the cheapest thing to re-derive.
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WithArgs("bench").
		WillReturnResult(sqlmock.NewResult(0, 7539))
	mock.ExpectExec("DELETE FROM knowledge_edges").WithArgs("bench").
		WillReturnResult(sqlmock.NewResult(0, 2392))
	mock.ExpectExec("DELETE FROM knowledge_entities").WithArgs("bench").
		WillReturnResult(sqlmock.NewResult(0, 6781))
	mock.ExpectExec("DELETE FROM project_memory_chunks").WithArgs("bench", "membench/%").
		WillReturnResult(sqlmock.NewResult(0, 2090))
	mock.ExpectCommit()

	res, err := ClearBenchmarkStore(context.Background(), db, "bench", "membench/%")
	if err != nil {
		t.Fatalf("ClearBenchmarkStore: %v", err)
	}
	if res.Chunks != 2090 || res.Entities != 6781 || res.Edges != 2392 || res.Mentions != 7539 {
		t.Errorf("counts not reported back: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Chunks alone are NOT the retrievable surface. A chunk delete cascades
// entity_mentions by FK and stops; knowledge_entities and knowledge_edges
// survive, and query_expander.go seeds expansion from entities while
// graph/searcher.go searches entities, edges and mentions. Measured after a
// chunk-only wipe of the real bench database: 3,545 entities and 2,392 edges
// still present and still queryable.
func TestClearBenchmarkStore_doesNotStopAtChunks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WithArgs("bench").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM knowledge_edges").WithArgs("bench").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM knowledge_entities").WithArgs("bench").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM project_memory_chunks").WithArgs("bench", "membench/%").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if _, err := ClearBenchmarkStore(context.Background(), db, "bench", "membench/%"); err != nil {
		t.Fatalf("ClearBenchmarkStore: %v", err)
	}
	// ExpectationsWereMet fails if any of the three graph deletes was skipped,
	// which is the regression this pins.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the graph tables must be cleared, not just chunks: %v", err)
	}
}

// The chunk predicate is BOTH project and scope prefix. A bare prefix would
// clear a project someone named "membench-something"; a bare project id would
// clear rows this harness does not own.
func TestClearBenchmarkStore_chunkDeleteIsProjectAndScopeScoped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WithArgs("bench").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM knowledge_edges").WithArgs("bench").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM knowledge_entities").WithArgs("bench").WillReturnResult(sqlmock.NewResult(0, 0))
	// Two args = both predicates present.
	mock.ExpectExec(`project_id = \$1 AND repo_scope LIKE \$2`).
		WithArgs("bench", "membench/%").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if _, err := ClearBenchmarkStore(context.Background(), db, "bench", "membench/%"); err != nil {
		t.Fatalf("ClearBenchmarkStore: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("chunk delete must carry both predicates: %v", err)
	}
}

// A partial clear is worse than none: it leaves a store that looks reset and is
// not, which is the shape of the original defect. Any failure rolls back.
func TestClearBenchmarkStore_rollsBackOnFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WithArgs("bench").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("DELETE FROM knowledge_edges").WithArgs("bench").
		WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	_, err = ClearBenchmarkStore(context.Background(), db, "bench", "membench/%")
	if err == nil {
		t.Fatal("a failed clear must surface: a half-cleared store reads as reset and is not")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the underlying cause must survive: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected a rollback: %v", err)
	}
}

// Refuse rather than guess. An empty project would make every predicate match
// nothing (silent no-op) or everything, depending on the statement.
func TestClearBenchmarkStore_refusesEmptyProject(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := ClearBenchmarkStore(context.Background(), db, "  ", "membench/%"); err == nil {
		t.Fatal("an empty project id must be refused, not executed")
	}
}

// The scope prefix is optional — the agent harness clears its project WHOLESALE
// because the contamination is the project's own memory whatever scope it was
// written under. An empty prefix must therefore mean "no scope predicate",
// not "match nothing".
func TestClearBenchmarkStore_emptyPrefixClearsProjectWide(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WithArgs("agentbench").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM knowledge_edges").WithArgs("agentbench").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM knowledge_entities").WithArgs("agentbench").WillReturnResult(sqlmock.NewResult(0, 0))
	// One arg only: no repo_scope predicate.
	mock.ExpectExec("DELETE FROM project_memory_chunks").WithArgs("agentbench").
		WillReturnResult(sqlmock.NewResult(0, 886))
	mock.ExpectCommit()

	res, err := ClearBenchmarkStore(context.Background(), db, "agentbench", "")
	if err != nil {
		t.Fatalf("ClearBenchmarkStore: %v", err)
	}
	if res.Chunks != 886 {
		t.Errorf("Chunks = %d, want 886", res.Chunks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Total exists so a caller can print one number; a clear that reports nothing
// is how the absent one stayed invisible.
func TestClearResultTotal(t *testing.T) {
	r := ClearResult{Mentions: 7539, Edges: 2392, Entities: 6781, Chunks: 2090}
	if got := r.Total(); got != 18802 {
		t.Errorf("Total() = %d, want 18802", got)
	}
	if (ClearResult{}).Total() != 0 {
		t.Error("an empty result totals zero")
	}
}
