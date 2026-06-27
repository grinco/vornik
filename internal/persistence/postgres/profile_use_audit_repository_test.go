package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

func TestProfileUseAudit_Insert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	now := time.Date(2026, 5, 24, 22, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO profile_use_audit")).
		WithArgs("web:abc", "task-1", []byte(`["tone","verbosity"]`), true).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(42), now))

	row := &persistence.ProfileUseAudit{
		OperatorID: "web:abc",
		TaskID:     "task-1",
		UsedKeys:   []string{"tone", "verbosity"},
		UsedNotes:  true,
	}
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if row.ID != 42 {
		t.Errorf("ID: %d", row.ID)
	}
}

func TestProfileUseAudit_Insert_EmptyKeysBecomesEmptyArray(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	// nil UsedKeys should serialise as `[]` so the JSONB column
	// NOT NULL constraint holds.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO profile_use_audit")).
		WithArgs("web:abc", nil, []byte(`[]`), false).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))

	if err := repo.Insert(context.Background(), &persistence.ProfileUseAudit{
		OperatorID: "web:abc",
	}); err != nil {
		t.Errorf("nil-keys insert should succeed: %v", err)
	}
}

func TestProfileUseAudit_Insert_RequiresOperator(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	err := repo.Insert(context.Background(), &persistence.ProfileUseAudit{})
	if err == nil {
		t.Errorf("empty operator id must error")
	}
}

func TestProfileUseAudit_ListForOperator_DefaultLimit(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id, COALESCE(task_id, ''), used_keys, used_notes, created_at")).
		WithArgs("web:abc", 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operator_id", "task_id", "used_keys", "used_notes", "created_at"}).
			AddRow(int64(1), "web:abc", "task-1", []byte(`["tone"]`), true, time.Now()))

	rows, err := repo.ListForOperator(context.Background(), "web:abc", persistence.ProfileUseAuditQuery{})
	if err != nil {
		t.Fatalf("ListForOperator: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].UsedKeys[0] != "tone" {
		t.Errorf("used_keys round-trip: %#v", rows[0].UsedKeys)
	}
}

func TestProfileUseAudit_ListForOperator_WithSinceAndLimit(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE operator_id = $1 AND created_at >= $2")).
		WithArgs("web:abc", since, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operator_id", "task_id", "used_keys", "used_notes", "created_at"}))

	_, err := repo.ListForOperator(context.Background(), "web:abc", persistence.ProfileUseAuditQuery{Since: since, Limit: 100})
	if err != nil {
		t.Errorf("ListForOperator: %v", err)
	}
}

func TestProfileUseAudit_ListForOperator_LimitClamps(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	// Limit > 500 caps at 500.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, operator_id")).
		WithArgs("web:abc", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operator_id", "task_id", "used_keys", "used_notes", "created_at"}))

	if _, err := repo.ListForOperator(context.Background(), "web:abc", persistence.ProfileUseAuditQuery{Limit: 10000}); err != nil {
		t.Errorf("ListForOperator: %v", err)
	}
}

func TestProfileUseAudit_DeleteAllForOperator(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProfileUseAuditRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM profile_use_audit WHERE operator_id")).
		WithArgs("web:abc").
		WillReturnResult(sqlmock.NewResult(0, 7))

	if err := repo.DeleteAllForOperator(context.Background(), "web:abc"); err != nil {
		t.Errorf("DeleteAllForOperator: %v", err)
	}
}
