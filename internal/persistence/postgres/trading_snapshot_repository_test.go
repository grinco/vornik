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

func newSnapshotRepo(t *testing.T) (*TradingSnapshotRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewTradingSnapshotRepository(db), mock, func() { _ = db.Close() }
}

// ---------- Record ----------

func TestSnapshotRecord_Validation(t *testing.T) {
	repo, _, cleanup := newSnapshotRepo(t)
	defer cleanup()

	cases := []struct {
		name   string
		snap   *persistence.TradingPositionsSnapshot
		errSub string
	}{
		{"nil", nil, "nil snapshot"},
		{"empty id", &persistence.TradingPositionsSnapshot{ProjectID: "p"}, "snapshot ID required"},
		{"empty project", &persistence.TradingPositionsSnapshot{ID: "s-1"}, "project ID required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.Record(context.Background(), tc.snap)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("expected error containing %q, got %v", tc.errSub, err)
			}
		})
	}
}

// TestSnapshotRecord_HappyPath pins all eight columns with the
// caller carrying an explicit RecordedAt and a non-empty
// positions_json blob.
func TestSnapshotRecord_HappyPath(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	positions := []byte(`[{"symbol":"AAPL","qty":10}]`)
	snap := &persistence.TradingPositionsSnapshot{
		ID:               "s-1",
		ProjectID:        "p-1",
		RecordedAt:       recorded,
		CashUSD:          1000.0,
		EquityUSD:        2500.0,
		UnrealisedPLUSD:  -50.0,
		RealisedPLDayUSD: 25.0,
		PositionsJSON:    positions,
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_positions_snapshots")).
		WithArgs(
			"s-1", "p-1", recorded,
			1000.0, 2500.0, -50.0, 25.0,
			positions,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), snap); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestSnapshotRecord_DefaultsRecordedAtToNULL covers the COALESCE
// trick: when RecordedAt is zero, the repo passes nil as $3 so the
// schema's `DEFAULT NOW()` fires. The sampler relies on this so
// its own wall clock doesn't drift the time series.
func TestSnapshotRecord_DefaultsRecordedAtToNULL(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	snap := &persistence.TradingPositionsSnapshot{
		ID:               "s-2",
		ProjectID:        "p-1",
		CashUSD:          0,
		EquityUSD:        0,
		UnrealisedPLUSD:  0,
		RealisedPLDayUSD: 0,
		PositionsJSON:    []byte(`[]`),
	}

	mock.ExpectExec(regexp.QuoteMeta("COALESCE($3, NOW())")).
		WithArgs(
			"s-2", "p-1",
			nil, // recorded_at -> NULL so COALESCE falls back to NOW()
			0.0, 0.0, 0.0, 0.0,
			[]byte(`[]`),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), snap); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestSnapshotRecord_DefaultsPositionsToEmptyArray is the
// NOT-NULL guard: positions_json is NOT NULL on the schema, so an
// empty caller-side blob must default to `[]` to avoid 23502.
func TestSnapshotRecord_DefaultsPositionsToEmptyArray(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	snap := &persistence.TradingPositionsSnapshot{
		ID:         "s-3",
		ProjectID:  "p-1",
		RecordedAt: recorded,
		// PositionsJSON deliberately nil/empty.
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_positions_snapshots")).
		WithArgs(
			"s-3", "p-1", recorded,
			0.0, 0.0, 0.0, 0.0,
			[]byte(`[]`), // defaulted, NOT empty bytes
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), snap); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestSnapshotRecord_PropagatesDBError(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO trading_positions_snapshots").
		WillReturnError(errors.New("conn closed"))

	err := repo.Record(context.Background(), &persistence.TradingPositionsSnapshot{
		ID: "s", ProjectID: "p", RecordedAt: time.Now().UTC(),
		PositionsJSON: []byte(`[]`),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------- ListSince ----------

func TestSnapshotListSince_RequiresProjectID(t *testing.T) {
	repo, _, cleanup := newSnapshotRepo(t)
	defer cleanup()
	if _, err := repo.ListSince(context.Background(), "", time.Time{}, 100); err == nil {
		t.Fatal("expected error for empty projectID")
	}
}

func TestSnapshotListSince_RejectsNegativeLimit(t *testing.T) {
	repo, _, cleanup := newSnapshotRepo(t)
	defer cleanup()
	if _, err := repo.ListSince(context.Background(), "p-1", time.Time{}, -1); err == nil {
		t.Fatal("expected error for negative limit")
	}
}

// TestSnapshotListSince_HappyPath_DefaultLimit pins the
// limit=0 -> 10000 default and the ORDER BY recorded_at ASC
// contract (callers expect oldest-first so they can iterate).
func TestSnapshotListSince_HappyPath_DefaultLimit(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	r1 := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	r2 := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	rows := sqlmock.NewRows([]string{
		"id", "project_id", "recorded_at", "cash_usd", "equity_usd",
		"unrealised_pl_usd", "realised_pl_day_usd", "positions_json",
	}).
		AddRow("s-1", "p-1", r1, 1000.0, 2500.0, -50.0, 25.0, []byte(`[{}]`)).
		AddRow("s-2", "p-1", r2, 1100.0, 2600.0, -25.0, 30.0, nil)

	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY recorded_at ASC")).
		WithArgs("p-1", since, 10000). // limit defaulted from 0
		WillReturnRows(rows)

	out, err := repo.ListSince(context.Background(), "p-1", since, 0)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].ID != "s-1" || out[1].ID != "s-2" {
		t.Errorf("rows out of expected order: %+v", out)
	}
	if string(out[0].PositionsJSON) != "[{}]" {
		t.Errorf("row 0 positions roundtrip: %s", out[0].PositionsJSON)
	}
	if out[1].PositionsJSON != nil {
		t.Errorf("row 1 positions should be nil, got %s", out[1].PositionsJSON)
	}
}

// TestSnapshotListSince_ExplicitLimit verifies caller-supplied
// limits are passed through verbatim (no clamp on the positive
// side — only the zero/negative branches are policy).
func TestSnapshotListSince_ExplicitLimit(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	since := time.Now().UTC()
	mock.ExpectQuery("trading_positions_snapshots").
		WithArgs("p-1", since, 250).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "recorded_at", "cash_usd", "equity_usd",
			"unrealised_pl_usd", "realised_pl_day_usd", "positions_json",
		}))

	if _, err := repo.ListSince(context.Background(), "p-1", since, 250); err != nil {
		t.Fatalf("ListSince: %v", err)
	}
}

func TestSnapshotListSince_QueryError(t *testing.T) {
	repo, mock, cleanup := newSnapshotRepo(t)
	defer cleanup()

	mock.ExpectQuery("trading_positions_snapshots").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.ListSince(context.Background(), "p-1", time.Now().UTC(), 100); err == nil {
		t.Fatal("expected error")
	}
}
