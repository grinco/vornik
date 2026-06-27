package persistence

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectMigrationAdvisoryLock seeds the sqlmock expectation for
// the pg_advisory_lock Run takes at the top of its body. Tests
// that call Run should call this first, then declare the rest
// of their expectations, then call expectMigrationAdvisoryUnlock
// last (the unlock fires from a defer, so it's the final query).
func expectMigrationAdvisoryLock(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_lock($1)")).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectMigrationAdvisoryUnlock seeds the matching unlock
// expectation. Must be the LAST expectation declared in a
// Run-calling test (production code defers the unlock).
func expectMigrationAdvisoryUnlock(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

// TestMigrationRunner_Run_AcquiresAdvisoryLock asserts the first
// query Run issues is `SELECT pg_advisory_lock($1)` with the
// fixed migration key, and the last query is the matching
// `pg_advisory_unlock`. Without this lock two daemon processes
// during a rolling deploy can both read currentVersion = N and
// both try to apply migration N+1 — a race that has historically
// corrupted schema on production rollouts.
//
// Reversion sentinel: deleting the lock from Run() fails this
// test loud.
func TestMigrationRunner_Run_AcquiresAdvisoryLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Expectation order:
	//   1. lock
	//   2. ensureMigrationsTable
	//   3. syncBootstrapSchema check (initial-version exists?)
	//   4. getCurrentVersion (no pending migrations)
	//   5. unlock (deferred)
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_lock($1)")).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "initial", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}

// TestMigrationRunner_Run_LockFailureAborts asserts that if the
// advisory lock can't be acquired (e.g. another pod holds it and
// our context times out), Run returns the error and does NOT
// attempt any migration work. Crucial — taking the lock is the
// gate that prevents concurrent application.
func TestMigrationRunner_Run_LockFailureAborts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_lock($1)")).
		WithArgs(migrationLockKey).
		WillReturnError(errors.New("lock acquire timeout"))
	// NO further expectations — if Run continues past the lock
	// failure, sqlmock will fail with "no expectation".

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "x", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected error on lock failure, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}
