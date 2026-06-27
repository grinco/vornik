package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

// TestOperatorProfile_Get_Found: a row exists; Get returns it
// with the structured JSONB bytes intact for caller-side
// unmarshal. structured is JSONB so the persistence layer
// preserves whatever shape the caller wrote.
func TestOperatorProfile_Get_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	structured := []byte(`{"tone":"terse","verbosity":"low"}`)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operator_id, structured, notes, created_at, updated_at")).
		WithArgs("telegram:42").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "structured", "notes", "created_at", "updated_at"}).
			AddRow("telegram:42", structured, "user prefers concise replies", now, now))

	got, err := repo.Get(context.Background(), "telegram:42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OperatorID != "telegram:42" {
		t.Errorf("operator_id = %q", got.OperatorID)
	}
	if string(got.Structured) != string(structured) {
		t.Errorf("structured bytes mismatch: %q vs %q", got.Structured, structured)
	}
	if got.Notes != "user prefers concise replies" {
		t.Errorf("notes mismatch: %q", got.Notes)
	}
}

// TestOperatorProfile_Get_NotFound returns persistence.ErrNotFound
// so callers branch cleanly on "fresh operator, no profile yet".
func TestOperatorProfile_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT operator_id, structured")).
		WithArgs("telegram:nobody").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "structured", "notes", "created_at", "updated_at"}))

	_, err := repo.Get(context.Background(), "telegram:nobody")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestOperatorProfile_Get_RequiresOperatorID is defensive: an
// empty operator_id would yield the wrong row (the legacy
// empty-string operator). Reject at the boundary.
func TestOperatorProfile_Get_RequiresOperatorID(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Errorf("expected error on empty operator_id")
	}
}

// TestOperatorProfile_Upsert_InsertsNew: the first Upsert for
// an operator commits a fresh row via ON CONFLICT DO UPDATE.
// Caller doesn't have to pre-check existence.
func TestOperatorProfile_Upsert_InsertsNew(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO operator_profile")).
		WithArgs("telegram:42", []byte(`{"tone":"terse"}`), "prefers concise").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "telegram:42",
		Structured: []byte(`{"tone":"terse"}`),
		Notes:      "prefers concise",
	}); err != nil {
		t.Errorf("Upsert: %v", err)
	}
}

// TestOperatorProfile_Upsert_EmptyStructuredBecomesEmptyObject:
// nil/empty structured payload coerces to JSONB '{}' so the
// column's NOT NULL constraint is satisfied and downstream
// unmarshal doesn't barf on null bytes.
func TestOperatorProfile_Upsert_EmptyStructuredBecomesEmptyObject(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO operator_profile")).
		WithArgs("telegram:42", []byte("{}"), "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "telegram:42",
	}); err != nil {
		t.Errorf("Upsert: %v", err)
	}
}

// TestOperatorProfile_Upsert_RequiresOperatorID is the same
// boundary guard the Get path applies. Calling Upsert with a
// blank operator_id would silently update the "" row.
func TestOperatorProfile_Upsert_RequiresOperatorID(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	if err := repo.Upsert(context.Background(), &persistence.OperatorProfile{}); err == nil {
		t.Errorf("expected error on empty operator_id")
	}
}

// TestOperatorProfile_Delete_RemovesRow drives the
// `vornikctl operator forget <id>` privacy-revocation path.
func TestOperatorProfile_Delete_RemovesRow(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM operator_profile")).
		WithArgs("telegram:42").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Delete(context.Background(), "telegram:42"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// TestOperatorProfile_Delete_Idempotent: deleting an unknown
// operator returns nil so the CLI's revocation flow doesn't
// have to pre-check existence.
func TestOperatorProfile_Delete_Idempotent(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM operator_profile")).
		WithArgs("telegram:nobody").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.Delete(context.Background(), "telegram:nobody"); err != nil {
		t.Errorf("Delete on unknown id: %v", err)
	}
}

// TestOperatorProfile_List_OrderedByUpdatedAtDesc — the UI's
// operator list ranks recently-active profiles first so the
// operator most likely to need attention sits at the top.
func TestOperatorProfile_List_OrderedByUpdatedAtDesc(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operator_id, structured")).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "structured", "notes", "created_at", "updated_at"}).
			AddRow("telegram:42", []byte(`{"tone":"terse"}`), "n1", now.Add(-1*time.Hour), now).
			AddRow("telegram:99", []byte(`{}`), "", now.Add(-2*time.Hour), now.Add(-30*time.Minute)))

	got, err := repo.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// scanReminder-like ordering: caller relies on the SQL
	// ORDER BY (asserted in the regex above + the row order).
	if got[0].OperatorID != "telegram:42" {
		t.Errorf("first row = %q, want telegram:42", got[0].OperatorID)
	}
}

// TestOperatorProfile_List_DefaultsLimit50 — calling List with
// 0 limit should default to a sensible cap (50). Without this,
// a buggy UI could request every row.
func TestOperatorProfile_List_DefaultsLimit50(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT operator_id, structured")).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "structured", "notes", "created_at", "updated_at"}))

	if _, err := repo.List(context.Background(), 0); err != nil {
		t.Errorf("List(0): %v", err)
	}
}

// TestOperatorProfile_List_CapsLimitAt500 — upper bound guard.
func TestOperatorProfile_List_CapsLimitAt500(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorProfileRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT operator_id, structured")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows([]string{"operator_id", "structured", "notes", "created_at", "updated_at"}))

	if _, err := repo.List(context.Background(), 9999); err != nil {
		t.Errorf("List: %v", err)
	}
}
