package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestAdminAuditRepository_Insert pins the column order + the
// JSONB / INET / NULL handling. A future schema change that
// renames a column would surface here first.
func TestAdminAuditRepository_Insert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)

	ts := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC)
	entry := &persistence.AdminAuditEntry{
		ID:        "admaud-1",
		Timestamp: ts,
		Principal: "sk-admin",
		Source:    "ui",
		Action:    "mcp.refresh",
		Target:    "proj-a",
		After:     `{"result":"ok"}`,
		IP:        "10.0.0.1",
		UserAgent: "test/1.0",
	}
	// Argument list: id, ts, principal, source, action, target,
	// before_state (NULL), after_state (JSONB string), ip (TEXT),
	// user_agent.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO admin_audit")).
		WithArgs("admaud-1", ts, "sk-admin", "ui", "mcp.refresh", "proj-a",
			sql.NullString{}, `{"result":"ok"}`,
			"10.0.0.1", "test/1.0").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), entry); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAdminAuditRepository_Insert_NullBeforeAfter — empty Before /
// After / IP collapse to NULL.
func TestAdminAuditRepository_Insert_NullBeforeAfter(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)

	ts := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC)
	entry := &persistence.AdminAuditEntry{
		ID:        "admaud-2",
		Timestamp: ts,
		Principal: "p", Source: "ui", Action: "a",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO admin_audit")).
		WithArgs("admaud-2", ts, "p", "ui", "a", "",
			sql.NullString{}, sql.NullString{},
			sql.NullString{}, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), entry); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAdminAuditRepository_Insert_AutogenID — empty ID gets filled
// in by GenerateID before INSERT.
func TestAdminAuditRepository_Insert_AutogenID(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)

	entry := &persistence.AdminAuditEntry{
		Principal: "p", Source: "ui", Action: "a",
	}
	// Match any args — we just want to confirm Insert doesn't bomb
	// out on a zero ID and that the row's ID is populated after.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO admin_audit")).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			"p", "ui", "a", "",
			sql.NullString{}, sql.NullString{}, sql.NullString{}, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Insert(context.Background(), entry); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if entry.ID == "" {
		t.Fatal("Insert should populate entry.ID when caller leaves it blank")
	}
}

// TestAdminAuditRepository_NilEntry — defensive.
func TestAdminAuditRepository_NilEntry(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)
	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("Insert(nil) should return an error")
	}
}

// TestAdminAuditRepository_PageSizeRequired — repo refuses
// unbounded scans.
func TestAdminAuditRepository_PageSizeRequired(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)
	_, err := repo.List(context.Background(), persistence.AdminAuditFilter{})
	if err == nil {
		t.Fatal("List with PageSize=0 should error")
	}
}

// TestAdminAuditRepository_List_AllFilters — exercises every
// optional filter axis to cover the WHERE-clause assembly + the
// LIKE-escape path for target prefix.
func TestAdminAuditRepository_List_AllFilters(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)

	rows := sqlmock.NewRows([]string{
		"id", "ts", "principal", "source", "action", "target",
		"before_state", "after_state", "ip", "user_agent",
	})

	mock.ExpectQuery(regexp.QuoteMeta("FROM admin_audit WHERE 1=1")).
		WithArgs(
			"mcp.refresh",
			"sk-admin",
			`100\%`+"%", // escaped + suffix
			time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
			10, 5,
		).
		WillReturnRows(rows)

	_, err := repo.List(context.Background(), persistence.AdminAuditFilter{
		Action:       "mcp.refresh",
		Principal:    "sk-admin",
		TargetPrefix: "100%",
		Since:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		Until:        time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
		PageSize:     10,
		Offset:       5,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

// TestAdminAuditRepository_List_QueryError — error path returns
// mapped error.
func TestAdminAuditRepository_List_QueryError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM admin_audit WHERE 1=1")).
		WithArgs(10).
		WillReturnError(errStub("db down"))

	_, err := repo.List(context.Background(), persistence.AdminAuditFilter{PageSize: 10})
	if err == nil {
		t.Fatal("List should return the underlying error")
	}
}

// errStub is a tiny error implementation for the negative-path test.
type errStub string

func (e errStub) Error() string { return string(e) }

// TestEscapeLikePrefix covers the wildcard-escape helper.
func TestEscapeLikePrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc", "abc"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
	}
	for _, tc := range cases {
		if got := escapeLikePrefix(tc.in); got != tc.want {
			t.Errorf("escapeLikePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAdminAuditRepository_List_FilterAction — exact-match filter
// pins the WHERE clause.
func TestAdminAuditRepository_List_FilterAction(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAdminAuditRepository(db)

	rows := sqlmock.NewRows([]string{
		"id", "ts", "principal", "source", "action", "target",
		"before_state", "after_state", "ip", "user_agent",
	}).AddRow("admaud-1", time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC),
		"sk-admin", "ui", "mcp.refresh", "p-a", "", "", "", "")

	mock.ExpectQuery(regexp.QuoteMeta("FROM admin_audit WHERE 1=1 AND action =")).
		WithArgs("mcp.refresh", 10).
		WillReturnRows(rows)

	got, err := repo.List(context.Background(), persistence.AdminAuditFilter{
		Action: "mcp.refresh", PageSize: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Action != "mcp.refresh" {
		t.Fatalf("List: got %+v", got)
	}
}
