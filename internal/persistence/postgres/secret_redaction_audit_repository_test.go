package postgres

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestSecretRedactionAuditRepository_Record pins the multi-row INSERT
// column order and the empty-batch short-circuit. Backlog item 2.
func TestSecretRedactionAuditRepository_Record(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewSecretRedactionAuditRepository(db)

	// Empty batch: no SQL issued.
	if err := repo.Record(context.Background(), nil); err != nil {
		t.Fatalf("Record(nil): %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO secret_redaction_audit")).
		WithArgs(
			"secred-1", "p1", "task-x", sqlmock.AnyArg(),
			"result_json", "openai_key", 2, "live", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Record(context.Background(), []persistence.SecretRedactionEvent{
		{ID: "secred-1", ProjectID: "p1", TaskID: "task-x", Checkpoint: "result_json", FindingType: "openai_key", Count: 2, Source: "live"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSecretRedactionAuditRepository_CountByTask pins the badge query
// and the empty-taskID short-circuit.
func TestSecretRedactionAuditRepository_CountByTask(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewSecretRedactionAuditRepository(db)

	// Empty taskID: no SQL.
	if _, total, err := repo.CountByTask(context.Background(), ""); err != nil || total != 0 {
		t.Fatalf("empty taskID = (%d, %v), want (0, nil)", total, err)
	}

	rows := sqlmock.NewRows([]string{"finding_type", "sum"}).
		AddRow("openai_key", 5).
		AddRow("entropy", 1)
	mock.ExpectQuery(regexp.QuoteMeta("FROM secret_redaction_audit")).
		WithArgs("task-x").
		WillReturnRows(rows)

	byType, total, err := repo.CountByTask(context.Background(), "task-x")
	if err != nil {
		t.Fatalf("CountByTask: %v", err)
	}
	if byType["openai_key"] != 5 || byType["entropy"] != 1 {
		t.Errorf("byType wrong: %+v", byType)
	}
	if total != 6 {
		t.Errorf("total = %d, want 6", total)
	}
}
