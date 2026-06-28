package persistence

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestMigrationRunner_Rollback_Happy covers the success path of Rollback,
// including the DELETE FROM migrations record removal.
func TestMigrationRunner_Rollback_Happy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(2))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE example")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM migrations WHERE version = $1")).
		WithArgs(2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{
		{Version: 1, Name: "one", Up: "SELECT 1", Down: "DROP TABLE one"},
		{Version: 2, Name: "two", Up: "SELECT 2", Down: "DROP TABLE example"},
	}

	if err := runner.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestMigrationRunner_Rollback_MissingVersion handles the case where the
// applied version is not in the in-memory migrations slice.
func TestMigrationRunner_Rollback_MissingVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(99))

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "one", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Rollback(context.Background()); err == nil {
		t.Fatal("expected error for missing migration")
	}
}

// TestMigrationRunner_Rollback_NoDownSQL covers the no-down-migration branch.
func TestMigrationRunner_Rollback_NoDownSQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "one", Up: "SELECT 1", Down: ""}}
	if err := runner.Rollback(context.Background()); err == nil {
		t.Fatal("expected no-down error")
	}
}

// TestMigrationRunner_Rollback_DownSQLFails covers the DownSQL failure path.
func TestMigrationRunner_Rollback_DownSQLFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE bad")).
		WillReturnError(errors.New("down boom"))
	mock.ExpectRollback()

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "one", Up: "SELECT 1", Down: "DROP TABLE bad"}}
	if err := runner.Rollback(context.Background()); err == nil {
		t.Fatal("expected down-failure error")
	}
}

// TestMigrationRunner_Rollback_RecordDeleteFails covers the row-delete error.
func TestMigrationRunner_Rollback_RecordDeleteFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DROP TABLE x")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM migrations WHERE version = $1")).
		WithArgs(1).
		WillReturnError(errors.New("delete boom"))
	mock.ExpectRollback()

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "one", Up: "SELECT 1", Down: "DROP TABLE x"}}
	if err := runner.Rollback(context.Background()); err == nil {
		t.Fatal("expected delete-record error")
	}
}

// TestMigrationRunner_SyncBootstrap_DBAlreadySynced covers the early-return
// when the initial-version row already exists.
func TestMigrationRunner_SyncBootstrap_DBAlreadySynced(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "initial", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestMigrationRunner_SyncBootstrap_BootstrapNotPresent covers the path where
// the legacy schema check returns false.
func TestMigrationRunner_SyncBootstrap_BootstrapNotPresent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("SELECT").
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT 1")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO migrations (version, name) VALUES ($1, $2)")).
		WithArgs(1, "initial_schema").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "initial_schema", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestMigrationRunner_SyncBootstrap_BootstrapPresentInsertsRow covers the
// legacy-schema bootstrap insertion path.
func TestMigrationRunner_SyncBootstrap_BootstrapPresentInsertsRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// bootstrap check returns TRUE
	mock.ExpectQuery("SELECT").
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	// bootstrap insertion
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO migrations (version, name)")).
		WithArgs(1, "initial_schema").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "initial_schema", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestMigrationRunner_ApplyMigration_BeginTxFails covers the failure to begin
// a transaction.
func TestMigrationRunner_ApplyMigration_BeginTxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))
	mock.ExpectBegin().WillReturnError(errors.New("begin boom"))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "x", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// TestMigrationRunner_ApplyMigration_RecordInsertFails covers the insert-into-
// migrations failure inside applyMigration.
func TestMigrationRunner_ApplyMigration_RecordInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT 1")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO migrations (version, name) VALUES ($1, $2)")).
		WithArgs(1, "x").
		WillReturnError(errors.New("insert boom"))
	mock.ExpectRollback()
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "x", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// TestMigrationRunner_Status_EnsureMigrationsTableFails covers the failure
// to ensure the migrations table when invoking Status.
func TestMigrationRunner_Status_EnsureMigrationsTableFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnError(errors.New("create boom"))

	runner := NewMigrationRunner(db)
	if _, err := runner.Status(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// TestMigrationRunner_Run_GetVersionFails covers the get-current-version
// failure path.
func TestMigrationRunner_Run_GetVersionFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnError(errors.New("query boom"))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = []Migration{{Version: 1, Name: "x", Up: "SELECT 1", Down: "SELECT 1"}}
	if err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// TestMigrationRunner_Run_EmptyMigrationsSet ensures the early return when
// the migration list is empty.
func TestMigrationRunner_Run_EmptyMigrationsSet(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectMigrationAdvisoryLock(mock)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(0))
	expectMigrationAdvisoryUnlock(mock)

	runner := NewMigrationRunner(db)
	runner.migrations = nil
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
