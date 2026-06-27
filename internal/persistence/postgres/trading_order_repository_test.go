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

func newOrderRepo(t *testing.T) (*TradingOrderRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewTradingOrderRepository(db), mock, func() { _ = db.Close() }
}

// ---------- ptr helpers ----------

func TestPtrHelpers(t *testing.T) {
	if got := ptrStringOrNil(nil); got != nil {
		t.Errorf("ptrStringOrNil(nil) = %v, want nil", got)
	}
	empty := ""
	if got := ptrStringOrNil(&empty); got != nil {
		t.Errorf("ptrStringOrNil(\"\") = %v, want nil", got)
	}
	v := "x"
	if got := ptrStringOrNil(&v); got != "x" {
		t.Errorf("ptrStringOrNil(\"x\") = %v, want \"x\"", got)
	}

	if got := ptrFloatOrNil(nil); got != nil {
		t.Errorf("ptrFloatOrNil(nil) = %v, want nil", got)
	}
	f := 1.5
	if got := ptrFloatOrNil(&f); got != 1.5 {
		t.Errorf("ptrFloatOrNil(1.5) = %v, want 1.5", got)
	}
	// Zero float is preserved (NOT mapped to NULL) — sentinel
	// distinction between "no price" (nil) and "literal zero".
	zero := 0.0
	if got := ptrFloatOrNil(&zero); got != 0.0 {
		t.Errorf("ptrFloatOrNil(0) = %v, want 0", got)
	}

	if got := ptrTimeOrNil(nil); got != nil {
		t.Errorf("ptrTimeOrNil(nil) = %v, want nil", got)
	}
	zt := time.Time{}
	if got := ptrTimeOrNil(&zt); got != nil {
		t.Errorf("ptrTimeOrNil(zero) = %v, want nil", got)
	}
	nyc, _ := time.LoadLocation("America/New_York")
	if nyc == nil {
		nyc = time.UTC
	}
	tz := time.Date(2026, 5, 13, 10, 0, 0, 0, nyc)
	got := ptrTimeOrNil(&tz)
	gotT, ok := got.(time.Time)
	if !ok {
		t.Fatalf("ptrTimeOrNil non-zero: expected time.Time, got %T", got)
	}
	if gotT.Location() != time.UTC {
		t.Errorf("ptrTimeOrNil should normalize to UTC, got %v", gotT.Location())
	}
}

// ---------- Record ----------

func TestOrderRecord_Validation(t *testing.T) {
	repo, _, cleanup := newOrderRepo(t)
	defer cleanup()

	cases := []struct {
		name   string
		order  *persistence.TradingOrder
		errSub string
	}{
		{"nil", nil, "nil trading order"},
		{"empty id", &persistence.TradingOrder{ProjectID: "p", IdempotencyKey: "k"}, "order ID required"},
		{"empty project", &persistence.TradingOrder{ID: "o", IdempotencyKey: "k"}, "project ID required"},
		{"empty idempotency", &persistence.TradingOrder{ID: "o", ProjectID: "p"}, "idempotency key required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.Record(context.Background(), tc.order)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("expected %q, got %v", tc.errSub, err)
			}
		})
	}
}

// TestOrderRecord_HappyPath_AllOptionalsSet pins the full insert
// shape with every nullable column carrying a value.
func TestOrderRecord_HappyPath_AllOptionalsSet(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	submitted := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	terminal := submitted.Add(time.Minute)
	taskID := "task-1"
	execID := "exec-1"
	brokerID := "B-12345"
	limitPx := 150.25
	stopPx := 140.0

	order := &persistence.TradingOrder{
		ID:               "ord-1",
		ProjectID:        "p-1",
		TaskID:           &taskID,
		ExecutionID:      &execID,
		BrokerOrderID:    &brokerID,
		IdempotencyKey:   "key-1",
		Mode:             "paper",
		Symbol:           "AAPL",
		Action:           "BUY",
		OrderType:        "LMT",
		Qty:              10,
		LimitPrice:       &limitPx,
		StopPrice:        &stopPx,
		TimeInForce:      "DAY",
		Status:           "filled",
		LastStatusReason: "OK",
		SubmittedAt:      submitted,
		TerminalAt:       &terminal,
	}

	// Pre-flight identity check: no existing row → upsert proceeds.
	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p-1", "key-1").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_orders")).
		WithArgs(
			"ord-1", "p-1", "task-1", "exec-1", "B-12345",
			"key-1", "paper", "AAPL", "BUY", "LMT",
			10.0, 150.25, 140.0, "DAY",
			"filled", "OK",
			submitted, terminal,
			0.0, // filled_qty
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), order); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestOrderRecord_NilOptionalsBindNULL covers the ptrStringOrNil /
// ptrFloatOrNil / ptrTimeOrNil pathways through Record: every
// optional column becomes SQL NULL.
func TestOrderRecord_NilOptionalsBindNULL(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	submitted := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	order := &persistence.TradingOrder{
		ID:             "ord-2",
		ProjectID:      "p-1",
		IdempotencyKey: "key-2",
		Mode:           "paper",
		Symbol:         "AAPL",
		Action:         "BUY",
		OrderType:      "MKT",
		Qty:            10,
		TimeInForce:    "DAY",
		Status:         "submitted",
		SubmittedAt:    submitted,
		// TaskID, ExecutionID, BrokerOrderID, LimitPrice, StopPrice, TerminalAt all nil.
	}

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p-1", "key-2").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))
	mock.ExpectExec("INSERT INTO trading_orders").
		WithArgs(
			"ord-2", "p-1",
			nil, nil, nil, // task_id, execution_id, broker_order_id
			"key-2", "paper", "AAPL", "BUY", "MKT",
			10.0,
			nil, nil, // limit_price, stop_price
			"DAY", "submitted", "",
			submitted,
			nil, // terminal_at
			0.0, // filled_qty
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), order); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestOrderRecord_DefaultsSubmittedAt: zero SubmittedAt -> now. The
// broker posts orders with submitted_at unset when it just
// generated the order locally; the daemon stamps the time so the
// timeline column is never NULL.
func TestOrderRecord_DefaultsSubmittedAt(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	order := &persistence.TradingOrder{
		ID:             "ord-3",
		ProjectID:      "p-1",
		IdempotencyKey: "key-3",
		Mode:           "paper",
		Symbol:         "AAPL",
		Action:         "SELL",
		OrderType:      "MKT",
		Qty:            5,
		TimeInForce:    "DAY",
		Status:         "submitted",
	}

	before := time.Now().UTC().Add(-time.Second)
	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p-1", "key-3").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))
	mock.ExpectExec("INSERT INTO trading_orders").
		WithArgs(
			"ord-3", "p-1",
			nil, nil, nil,
			"key-3", "paper", "AAPL", "SELL", "MKT",
			5.0,
			nil, nil,
			"DAY", "submitted", "",
			recentTimeMatcher{notBefore: before},
			nil,
			0.0, // filled_qty
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), order); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestOrderRecord_DualConflictUpsertShape verifies the
// `ON CONFLICT (project_id, idempotency_key) DO UPDATE SET ...`
// clause is what fires on retry. The fragment match here pins
// the dual-key contract; without it, a retry that brings updated
// status would either land as a duplicate row or silently drop.
func TestOrderRecord_DualConflictUpsertShape(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p", "k").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (project_id, idempotency_key) DO UPDATE SET")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Record(context.Background(), &persistence.TradingOrder{
		ID: "o", ProjectID: "p", IdempotencyKey: "k",
		SubmittedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestOrderRecord_PropagatesDBError(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p", "k").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))
	mock.ExpectExec("INSERT INTO trading_orders").
		WillReturnError(errors.New("conn closed"))

	err := repo.Record(context.Background(), &persistence.TradingOrder{
		ID: "o", ProjectID: "p", IdempotencyKey: "k",
		SubmittedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestOrderRecord_IdentityMismatchRejected is the regression
// guard for the 2026-05-15 NVDA bookkeeping incident: when the
// (project_id, idempotency_key) tuple already names a row whose
// (symbol, action, qty, limit_price) differs from the incoming
// payload, the upsert MUST refuse with ErrOrderIdentityMismatch
// instead of silently merging the incoming status onto the
// stale row's identity. Pre-fix, a fresh 6-share NVDA buy got
// attached to a May 12 fractional 2.7614 row because both calls
// passed sha256("") as the idempotency_key; the broker's MCP
// validator now blocks that key class, and this test pins the
// belt-and-suspenders defense at the repository layer.
func TestOrderRecord_IdentityMismatchRejected(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	// Existing row: SELL 2.7614 NVDA at limit 220.50.
	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("ibkr-trader", "shared-key-32chars-test-fixture-1").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}).
			AddRow("NVDA", "BUY", 2.7614, 220.50))

	// Incoming: BUY 6 NVDA at limit 230.34 — same key but different size.
	limit := 230.34
	err := repo.Record(context.Background(), &persistence.TradingOrder{
		ID:             "ord-fresh",
		ProjectID:      "ibkr-trader",
		IdempotencyKey: "shared-key-32chars-test-fixture-1",
		Symbol:         "NVDA",
		Action:         "BUY",
		Qty:            6,
		LimitPrice:     &limit,
		SubmittedAt:    time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected ErrOrderIdentityMismatch when qty/limit differ from existing row")
	}
	if !errors.Is(err, persistence.ErrOrderIdentityMismatch) {
		t.Fatalf("expected ErrOrderIdentityMismatch in chain, got %v", err)
	}
	// Error text must surface the diffs so operators reconciling
	// the broker side know which fields drifted.
	msg := err.Error()
	if !contains(msg, "qty 2.7614→6") || !contains(msg, "limit 220.5→230.34") {
		t.Errorf("error message should list both qty and limit drift, got %q", msg)
	}
}

// TestOrderRecord_MatchingIdentityProceeds — the happy path for
// a legitimate retry: same key + same (symbol, action, qty,
// limit_price) → no error, the upsert runs as before. Pins that
// the new pre-flight check doesn't false-positive on the
// at-least-once-delivery contract the broker's audit writer
// depends on.
func TestOrderRecord_MatchingIdentityProceeds(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p-1", "matching-key-32chars-test-fixture").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}).
			AddRow("AAPL", "BUY", 10.0, 150.25))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_orders")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	limit := 150.25
	err := repo.Record(context.Background(), &persistence.TradingOrder{
		ID:             "ord-retry",
		ProjectID:      "p-1",
		IdempotencyKey: "matching-key-32chars-test-fixture",
		Symbol:         "AAPL",
		Action:         "BUY",
		Qty:            10,
		LimitPrice:     &limit,
		SubmittedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("matching retry should succeed: %v", err)
	}
}

// TestOrderRecord_FilledQtyBoundAndUpdated verifies that
// filled_qty is included in the INSERT column list as $19 and that
// the ON CONFLICT DO UPDATE SET clause also updates it from
// EXCLUDED.filled_qty. This covers the fill-reconciliation
// requirement: each audit write carries the latest accumulated
// filled_qty so the reconciler can derive partial/filled status
// by comparing filled_qty vs qty without a second query.
func TestOrderRecord_FilledQtyBoundAndUpdated(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	submitted := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)
	limitPx := 100.0

	order := &persistence.TradingOrder{
		ID:               "ord-fq",
		ProjectID:        "p-fq",
		IdempotencyKey:   "key-fq",
		Mode:             "paper",
		Symbol:           "NVDA",
		Action:           "BUY",
		OrderType:        "LMT",
		Qty:              10,
		LimitPrice:       &limitPx,
		TimeInForce:      "DAY",
		Status:           "partial",
		LastStatusReason: "",
		SubmittedAt:      submitted,
		FilledQty:        6.0,
	}

	// Pre-flight identity check: no existing row.
	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p-fq", "key-fq").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))

	// The INSERT must pass filled_qty (6.0) as the 19th arg, and the
	// ON CONFLICT fragment must include filled_qty = EXCLUDED.filled_qty.
	mock.ExpectExec(regexp.QuoteMeta("filled_qty = EXCLUDED.filled_qty")).
		WithArgs(
			"ord-fq", "p-fq",
			nil, nil, nil, // task_id, execution_id, broker_order_id
			"key-fq", "paper", "NVDA", "BUY", "LMT",
			10.0, 100.0,
			nil, // stop_price
			"DAY", "partial", "",
			submitted,
			nil, // terminal_at
			6.0, // filled_qty — $19
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), order); err != nil {
		t.Fatalf("Record with FilledQty: %v", err)
	}
}

// contains is a tiny helper to keep the assertion lines above
// readable without dragging in strings.Contains repeatedly.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// ---------- List ----------

func orderRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "execution_id", "broker_order_id",
		"idempotency_key", "mode", "symbol", "action", "order_type",
		"qty", "limit_price", "stop_price", "time_in_force",
		"status", "last_status_reason", "submitted_at", "terminal_at",
	})
}

// TestOrderList_DefaultPageSize: PageSize=0 -> 100.
func TestOrderList_DefaultPageSize(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	submitted := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	terminal := submitted.Add(time.Minute)
	taskID := "task-1"
	limitPx := 150.0

	rows := orderRows().
		AddRow(
			"ord-1", "p-1", &taskID, nil, nil,
			"key-1", "paper", "AAPL", "BUY", "LMT",
			10.0, &limitPx, nil, "DAY",
			"filled", "OK", submitted, &terminal,
		).
		AddRow(
			"ord-2", "p-1", nil, nil, nil,
			"key-2", "paper", "MSFT", "SELL", "MKT",
			5.0, nil, nil, "DAY",
			"submitted", "", submitted, nil,
		)

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders WHERE 1=1")).
		WithArgs(100).
		WillReturnRows(rows)

	out, err := repo.List(context.Background(), persistence.TradingOrderFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].TaskID == nil || *out[0].TaskID != "task-1" {
		t.Errorf("row 0 task_id roundtrip: %+v", out[0].TaskID)
	}
	if out[0].LimitPrice == nil || *out[0].LimitPrice != 150.0 {
		t.Errorf("row 0 limit_price roundtrip: %+v", out[0].LimitPrice)
	}
	if out[1].LimitPrice != nil {
		t.Errorf("row 1 limit_price should be nil, got %+v", out[1].LimitPrice)
	}
	if out[1].TerminalAt != nil {
		t.Errorf("row 1 terminal_at should be nil")
	}
}

func TestOrderList_CapsPageSize(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM trading_orders").
		WithArgs(5000).
		WillReturnRows(orderRows())

	if _, err := repo.List(context.Background(), persistence.TradingOrderFilter{
		PageSize: 10000,
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestOrderList_AllFilters(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	pid, status, sym := "p-1", "filled", "AAPL"
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("p-1", "filled", "AAPL", since, until, 25, 10).
		WillReturnRows(orderRows())

	_, err := repo.List(context.Background(), persistence.TradingOrderFilter{
		ProjectID: &pid, Status: &status, Symbol: &sym,
		Since: &since, Until: &until,
		PageSize: 25, Offset: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestOrderList_QueryError(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM trading_orders").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.List(context.Background(), persistence.TradingOrderFilter{}); err == nil {
		t.Fatal("expected error")
	}
}

// ---------- Count ----------

func TestOrderCount_NoFilter(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM trading_orders WHERE 1=1")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(42)))

	got, err := repo.Count(context.Background(), persistence.TradingOrderFilter{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestOrderCount_AllFilters(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	pid, status, sym := "p-1", "filled", "AAPL"
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM trading_orders")).
		WithArgs("p-1", "filled", "AAPL", since, until).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))

	got, err := repo.Count(context.Background(), persistence.TradingOrderFilter{
		ProjectID: &pid, Status: &status, Symbol: &sym,
		Since: &since, Until: &until,
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 7 {
		t.Errorf("expected 7, got %d", got)
	}
}

func TestOrderCount_QueryError(t *testing.T) {
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	mock.ExpectQuery("SELECT COUNT").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.Count(context.Background(), persistence.TradingOrderFilter{}); err == nil {
		t.Fatal("expected error")
	}
}
