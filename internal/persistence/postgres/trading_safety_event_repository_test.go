package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func newSafetyEventRepo(t *testing.T) (*TradingSafetyEventRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewTradingSafetyEventRepository(db), mock, func() { _ = db.Close() }
}

// ---------- Record validation ----------

func TestSafetyEventRecord_Validation(t *testing.T) {
	repo, _, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	cases := []struct {
		name   string
		event  *persistence.TradingSafetyEvent
		errSub string
	}{
		{"nil", nil, "nil safety event"},
		{"empty id", &persistence.TradingSafetyEvent{ProjectID: "p", Kind: "halt"}, "ID required"},
		{"empty project", &persistence.TradingSafetyEvent{ID: "e-1", Kind: "halt"}, "project ID required"},
		{"empty kind", &persistence.TradingSafetyEvent{ID: "e-1", ProjectID: "p"}, "kind required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.Record(context.Background(), tc.event)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("expected error containing %q, got %v", tc.errSub, err)
			}
		})
	}
}

// TestSafetyEventRecord_HappyPath pins the full INSERT shape with
// every column the schema cares about. Symbol/Detail are populated
// here so the pointer-and-blob binding is exercised end-to-end.
func TestSafetyEventRecord_HappyPath(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	sym := "AAPL"
	detail := []byte(`{"reason":"daily_loss_cap"}`)
	event := &persistence.TradingSafetyEvent{
		ID:         "evt-1",
		ProjectID:  "p-1",
		RecordedAt: recorded,
		Kind:       "halt_trading",
		Severity:   "critical",
		Symbol:     &sym,
		Detail:     detail,
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_safety_events")).
		WithArgs(
			"evt-1", "p-1", recorded, "halt_trading", "critical",
			"AAPL", detail,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), event); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestSafetyEventRecord_DefaultsRecordedAtAndSeverity pins the
// defaulting block: empty Severity -> "info", zero RecordedAt -> now.
// The kind+severity defaulting is what keeps the audit trail
// readable when callers post sparse events.
func TestSafetyEventRecord_DefaultsRecordedAtAndSeverity(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	event := &persistence.TradingSafetyEvent{
		ID:        "evt-2",
		ProjectID: "p-1",
		Kind:      "submitted",
		// Severity and RecordedAt deliberately omitted.
	}

	before := time.Now().UTC().Add(-time.Second)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_safety_events")).
		WithArgs(
			"evt-2", "p-1",
			recentTimeMatcher{notBefore: before}, // recorded_at defaulted to now
			"submitted",
			"info",      // severity defaulted
			nil,         // symbol: untyped nil (ptrStringOrNil)
			[]byte(nil), // detail: typed-nil []byte (the source sets `detail = nil`
			// on a []byte local, which boxes as ([]byte)(nil), not
			// untyped nil — lib/pq still translates to SQL NULL)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), event); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestSafetyEventRecord_EmptyDetailBindsAsNull is load-bearing:
// the schema's detail column is JSONB NULLABLE, and an empty []byte
// would bind as `”::bytea`, blowing up the JSONB cast. The
// `detail = nil` branch in the source guards this — pin it.
func TestSafetyEventRecord_EmptyDetailBindsAsNull(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	event := &persistence.TradingSafetyEvent{
		ID:         "evt-3",
		ProjectID:  "p-1",
		RecordedAt: time.Now().UTC(),
		Kind:       "ack",
		Severity:   "info",
		Detail:     []byte{}, // empty — must bind as NULL
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_safety_events")).
		WithArgs(
			"evt-3", "p-1", sqlmock.AnyArg(), "ack", "info",
			nil,         // symbol untyped nil
			[]byte(nil), // detail typed-nil []byte (NOT empty bytes)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), event); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestSafetyEventRecord_ConflictDoNothingIsSilent covers the
// audit-trail contract: a retried POST under transient outage must
// be a silent no-op (RowsAffected = 0, err = nil). Any change that
// surfaces a duplicate-key error breaks the broker's at-least-once
// retry path.
func TestSafetyEventRecord_ConflictDoNothingIsSilent(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	event := &persistence.TradingSafetyEvent{
		ID:         "evt-dup",
		ProjectID:  "p-1",
		RecordedAt: time.Now().UTC(),
		Kind:       "halt",
		Severity:   "critical",
	}

	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (id) DO NOTHING")).
		WillReturnResult(sqlmock.NewResult(0, 0)) // simulated dup: 0 rows

	if err := repo.Record(context.Background(), event); err != nil {
		t.Fatalf("dup retry should be silent, got %v", err)
	}
}

func TestSafetyEventRecord_PropagatesDBError(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	event := &persistence.TradingSafetyEvent{
		ID: "e", ProjectID: "p", RecordedAt: time.Now().UTC(),
		Kind: "halt", Severity: "critical",
	}
	mock.ExpectExec("INSERT INTO trading_safety_events").
		WillReturnError(errors.New("conn closed"))

	if err := repo.Record(context.Background(), event); err == nil {
		t.Fatal("expected DB error to propagate")
	}
}

// ---------- List ----------

func safetyEventRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "recorded_at", "kind", "severity", "symbol", "detail",
	})
}

// TestSafetyEventList_DefaultPageSize asserts that PageSize=0
// defaults to 100 (not 0, which would return zero rows) and that
// no OFFSET clause is appended when Offset=0.
func TestSafetyEventList_DefaultPageSize(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	rows := safetyEventRows().
		AddRow("e-1", "p-1", recorded, "halt", "critical", "AAPL", []byte(`{}`)).
		AddRow("e-2", "p-1", recorded, "warn", "info", nil, nil)

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_safety_events WHERE 1=1")).
		WithArgs(100). // default
		WillReturnRows(rows)

	out, err := repo.List(context.Background(), persistence.TradingSafetyEventFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].Symbol == nil || *out[0].Symbol != "AAPL" {
		t.Errorf("row 0 symbol roundtrip: %+v", out[0].Symbol)
	}
	if out[1].Symbol != nil {
		t.Errorf("row 1 symbol should be nil, got %v", *out[1].Symbol)
	}
	if string(out[0].Detail) != "{}" {
		t.Errorf("row 0 detail roundtrip: %s", out[0].Detail)
	}
	if out[1].Detail != nil {
		t.Errorf("row 1 detail should be nil, got %v", out[1].Detail)
	}
}

// TestSafetyEventList_CapsPageSize verifies that PageSize > 1000
// is clamped to 1000 — the full-table-scan guard documented in the
// source comment.
func TestSafetyEventList_CapsPageSize(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM trading_safety_events").
		WithArgs(1000). // clamped from 5000
		WillReturnRows(safetyEventRows())

	if _, err := repo.List(context.Background(), persistence.TradingSafetyEventFilter{
		PageSize: 5000,
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
}

// TestSafetyEventList_AllFilters drives every WHERE branch + OFFSET.
func TestSafetyEventList_AllFilters(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	pid, kind, sym := "p-1", "halt", "AAPL"
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_safety_events")).
		WithArgs("p-1", "halt", "AAPL", since, until, 50, 10).
		WillReturnRows(safetyEventRows())

	_, err := repo.List(context.Background(), persistence.TradingSafetyEventFilter{
		ProjectID: &pid, Kind: &kind, Symbol: &sym,
		Since: &since, Until: &until,
		PageSize: 50, Offset: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestSafetyEventList_EmptyStringFiltersAreIgnored guards the
// "*filter.X != \"\"" branch — passing &"" must NOT add a
// WHERE clause (otherwise the UI's "all kinds" tab would return
// zero rows on every project that ever had an event).
func TestSafetyEventList_EmptyStringFiltersAreIgnored(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	empty := ""
	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_safety_events WHERE 1=1")).
		WithArgs(100). // only the default page size
		WillReturnRows(safetyEventRows())

	_, err := repo.List(context.Background(), persistence.TradingSafetyEventFilter{
		ProjectID: &empty, Kind: &empty, Symbol: &empty,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestSafetyEventList_QueryError(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM trading_safety_events").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.List(context.Background(), persistence.TradingSafetyEventFilter{}); err == nil {
		t.Fatal("expected error")
	}
}

// ---------- Count ----------

func TestSafetyEventCount_NoFilter(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM trading_safety_events WHERE 1=1")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))

	got, err := repo.Count(context.Background(), persistence.TradingSafetyEventFilter{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 7 {
		t.Errorf("expected 7, got %d", got)
	}
}

func TestSafetyEventCount_WithFilters(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	pid, kind := "p-1", "halt"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM trading_safety_events")).
		WithArgs("p-1", "halt").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	got, err := repo.Count(context.Background(), persistence.TradingSafetyEventFilter{
		ProjectID: &pid, Kind: &kind,
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestSafetyEventCount_QueryError(t *testing.T) {
	repo, mock, cleanup := newSafetyEventRepo(t)
	defer cleanup()

	mock.ExpectQuery("SELECT COUNT").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.Count(context.Background(), persistence.TradingSafetyEventFilter{}); err == nil {
		t.Fatal("expected error")
	}
}
