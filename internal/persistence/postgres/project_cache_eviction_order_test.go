package postgres

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

// The cache evictor must run INSIDE the wipe's transaction and BEFORE any
// DELETE. The keys it needs are derived from the chunks' own source_name and
// content, so after the DELETE there is nothing left to compute them from —
// an evictor that ran last would find no rows and evict nothing, silently.
//
// sqlmock's expectations are ORDERED, so the evictor's own statement standing
// first is the assertion.
//
// Covered here rather than only in the integration suite because
// `make test-integration` runs ./test/integration/, ./internal/persistence/
// postgres/ and ./internal/cli/ — not ./internal/memory/, where the end-to-end
// eviction tests live.
func TestDeleteProjectData_EvictsCacheBeforeAnyDelete(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()

	called := false
	evict := func(ctx context.Context, tx persistence.DBTX, projectID string) (int, error) {
		called = true
		_, err := tx.ExecContext(ctx, "DELETE FROM embedding_cache WHERE content_hash = $1", projectID)
		return 7, err
	}
	repo := NewProjectDataCleanupRepository(db, evict)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM embedding_cache")).
		WithArgs("p1").
		WillReturnResult(sqlmock.NewResult(0, 7))
	for range persistence.ProjectDataTables {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ")).
			WithArgs("p1").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	stats, err := repo.DeleteProjectData(context.Background(), "p1")
	if err != nil {
		t.Fatalf("DeleteProjectData: %v", err)
	}
	if !called {
		t.Fatal("the cache evictor was never invoked")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("eviction did not run first, inside the transaction: %v", err)
	}
	if !stats.CacheEvictionRan {
		t.Error("stats do not record that the eviction ran")
	}
	if stats.CachedEmbeddingsEvicted != 7 {
		t.Errorf("CachedEmbeddingsEvicted = %d, want 7", stats.CachedEmbeddingsEvicted)
	}
}

// A deployment with no evictor wired must SAY so rather than report a zero.
// A count that could mean "nothing to evict" or "never attempted" is the shape
// of control this codebase treats as a defect.
func TestDeleteProjectData_NoEvictorReportsNotAttempted(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProjectDataCleanupRepositoryWithoutCacheEviction(db)

	mock.ExpectBegin()
	for range persistence.ProjectDataTables {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ")).
			WithArgs("p1").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	stats, err := repo.DeleteProjectData(context.Background(), "p1")
	if err != nil {
		t.Fatalf("DeleteProjectData: %v", err)
	}
	if stats.CacheEvictionRan {
		t.Error("CacheEvictionRan is true with no evictor wired")
	}
	if stats.CachedEmbeddingsEvicted != 0 {
		t.Errorf("CachedEmbeddingsEvicted = %d, want 0", stats.CachedEmbeddingsEvicted)
	}
}

// A bare nil evictor is a WIRING MISTAKE, not a configuration. The wipe it
// produces deletes a project's chunks and keeps every vector derived from them,
// and the only signal is a false in an audit row somebody has to read later.
//
// Raised by the 2026-09-07 companion review: reporting the omission honestly is
// necessary and not sufficient, because it makes the gap visible only after a
// deletion has already happened. Constructing the deleter without an evictor is
// now something a caller has to SAY, so a reviewer sees it in the diff.
func TestNewProjectDataCleanupRepository_RefusesANilEvictor(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a nil evictor was accepted silently")
		}
		// The message must name the alternative, or the next person's fix is to
		// delete the check.
		if msg, _ := r.(string); !strings.Contains(msg, "NewProjectDataCleanupRepositoryWithoutCacheEviction") {
			t.Errorf("panic message does not name the explicit constructor: %v", r)
		}
	}()
	_ = NewProjectDataCleanupRepository(db, nil)
}

// The deliberate case still exists — some callers genuinely do not evict (the
// transaction-shape tests below), and forcing them through a named constructor
// is the whole point.
func TestNewProjectDataCleanupRepositoryWithoutCacheEviction_IsAllowed(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	if repo := NewProjectDataCleanupRepositoryWithoutCacheEviction(db); repo == nil {
		t.Fatal("explicit no-eviction constructor returned nil")
	}
}
