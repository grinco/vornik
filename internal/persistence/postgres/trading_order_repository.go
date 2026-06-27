package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingOrderRepository implements
// persistence.TradingOrderRepository on PostgreSQL.
//
// Record uses ON CONFLICT (project_id, idempotency_key) DO
// NOTHING + ON CONFLICT (id) DO NOTHING so a retried POST from
// the broker's audit writer (after a transient daemon outage)
// is a silent no-op. The broker generates the idempotency key
// once per place_order call; retries reuse it. The result: at
// least once delivery from the broker, exactly once landing in
// the DB, no duplicate rows under any failure mode.
type TradingOrderRepository struct {
	db DBTX
}

// NewTradingOrderRepository constructs a new repo over db.
func NewTradingOrderRepository(db DBTX) *TradingOrderRepository {
	return &TradingOrderRepository{db: db}
}

// Record upserts one order row. The dual ON CONFLICT shape
// covers both the natural-key uniqueness (project + idempotency)
// and the surrogate id PRIMARY KEY — either collision returns
// silently. Status / last_status_reason / terminal_at columns
// DO update on conflict so a later POST that reports "this
// order has now filled" replaces the older "submitted" status
// in place. Idempotency_key is deliberately NOT updated on
// conflict (it's the natural key).
//
// Pre-flight identity check (added 2026-05-15): when a row with
// the same (project_id, idempotency_key) already exists, refuse
// the upsert when its (symbol, action, qty, limit_price) differs
// from the incoming payload. The idempotency key MUST describe
// the same logical order across all retries; a mismatch means an
// upstream caller reused a key for a different decision (the
// 2026-05-15 NVDA incident: sha256("") collapsed onto May 12's
// 2.7614 fractional row and merged a fresh 6-share buy in
// silently). Returning ErrOrderIdentityMismatch lets the broker
// audit writer surface the divergence to operators instead of
// quietly corrupting the books — the broker has already placed
// the order at IBKR by this point, so the row stays in its old
// shape and the operator's reconcile path takes over.
func (r *TradingOrderRepository) Record(ctx context.Context, order *persistence.TradingOrder) error {
	if order == nil {
		return fmt.Errorf("nil trading order")
	}
	if order.ID == "" {
		return fmt.Errorf("trading order ID required")
	}
	if order.ProjectID == "" {
		return fmt.Errorf("project ID required")
	}
	if order.IdempotencyKey == "" {
		return fmt.Errorf("idempotency key required")
	}
	if err := r.checkIdentityMatch(ctx, order); err != nil {
		return err
	}
	submittedAt := order.SubmittedAt
	if submittedAt.IsZero() {
		submittedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_orders (
		    id, project_id, task_id, execution_id, broker_order_id,
		    idempotency_key, mode, symbol, action, order_type,
		    qty, limit_price, stop_price, time_in_force,
		    status, last_status_reason, submitted_at, terminal_at,
		    filled_qty
		) VALUES (
		    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		    $11, $12, $13, $14, $15, $16, $17, $18,
		    $19
		)
		ON CONFLICT (project_id, idempotency_key) DO UPDATE SET
		    broker_order_id    = COALESCE(EXCLUDED.broker_order_id, trading_orders.broker_order_id),
		    status             = EXCLUDED.status,
		    last_status_reason = EXCLUDED.last_status_reason,
		    terminal_at        = COALESCE(EXCLUDED.terminal_at, trading_orders.terminal_at),
		    filled_qty         = EXCLUDED.filled_qty`,
		order.ID, order.ProjectID, ptrStringOrNil(order.TaskID),
		ptrStringOrNil(order.ExecutionID), ptrStringOrNil(order.BrokerOrderID),
		order.IdempotencyKey, order.Mode, order.Symbol, order.Action, order.OrderType,
		order.Qty, ptrFloatOrNil(order.LimitPrice), ptrFloatOrNil(order.StopPrice),
		order.TimeInForce, order.Status, order.LastStatusReason,
		submittedAt, ptrTimeOrNil(order.TerminalAt),
		order.FilledQty,
	)
	return mapDBError(err)
}

// checkIdentityMatch verifies that an existing trading_orders row
// with the same (project_id, idempotency_key) describes the same
// logical order as the incoming payload. Returns nil when no row
// exists (the common path — a fresh insert) or when the existing
// row's identity matches. Returns ErrOrderIdentityMismatch
// (wrapped with context) when the tuple
// (symbol, action, qty, limit_price) differs.
//
// Limit price comparison uses an explicit COALESCE so a missing
// limit (MKT orders) matches a missing limit, and float equality
// is treated as exact — the broker writes round-trip Decimal
// values so the floats are stable. Stop price is intentionally
// excluded: brackets may legitimately revise the protective stop
// without the order's identity changing.
func (r *TradingOrderRepository) checkIdentityMatch(ctx context.Context, order *persistence.TradingOrder) error {
	const q = `
		SELECT symbol, action, qty, COALESCE(limit_price, 0)
		FROM trading_orders
		WHERE project_id = $1 AND idempotency_key = $2`
	var (
		existingSym    string
		existingAction string
		existingQty    float64
		existingLimit  float64
	)
	row := r.db.QueryRowContext(ctx, q, order.ProjectID, order.IdempotencyKey)
	switch err := row.Scan(&existingSym, &existingAction, &existingQty, &existingLimit); {
	case err != nil:
		// sql.ErrNoRows lands here too — no existing row means no
		// identity to clash with; the upsert proceeds normally.
		// Other errors get mapped (connection drops, etc.) so the
		// caller distinguishes "DB problem" from "data problem".
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return mapDBError(err)
	}
	incomingLimit := 0.0
	if order.LimitPrice != nil {
		incomingLimit = *order.LimitPrice
	}
	mismatches := []string{}
	if existingSym != order.Symbol {
		mismatches = append(mismatches, fmt.Sprintf("symbol %q→%q", existingSym, order.Symbol))
	}
	if existingAction != order.Action {
		mismatches = append(mismatches, fmt.Sprintf("action %q→%q", existingAction, order.Action))
	}
	if existingQty != order.Qty {
		mismatches = append(mismatches, fmt.Sprintf("qty %g→%g", existingQty, order.Qty))
	}
	if existingLimit != incomingLimit {
		mismatches = append(mismatches, fmt.Sprintf("limit %g→%g", existingLimit, incomingLimit))
	}
	if len(mismatches) == 0 {
		return nil
	}
	return fmt.Errorf("%w: project=%s idempotency_key=%s differs in %s",
		persistence.ErrOrderIdentityMismatch, order.ProjectID, order.IdempotencyKey, strings.Join(mismatches, ", "))
}

// List returns trading_orders rows matching the filter,
// newest-first. Default page size 100; cap at 5000 so a
// caller passing PageSize=0 doesn't accidentally trigger a
// full-table scan on a busy project.
func (r *TradingOrderRepository) List(ctx context.Context, filter persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	q, args := buildTradingOrderQuery(filter, false)
	q += " ORDER BY submitted_at DESC"
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 5000 {
		filter.PageSize = 5000
	}
	args = append(args, filter.PageSize)
	q += fmt.Sprintf(" LIMIT $%d", len(args))
	if filter.Offset > 0 {
		args = append(args, filter.Offset)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TradingOrder
	for rows.Next() {
		o := &persistence.TradingOrder{}
		var taskID, execID, brokerID *string
		var limitPx, stopPx *float64
		var terminalAt *time.Time
		if err := rows.Scan(
			&o.ID, &o.ProjectID, &taskID, &execID, &brokerID,
			&o.IdempotencyKey, &o.Mode, &o.Symbol, &o.Action, &o.OrderType,
			&o.Qty, &limitPx, &stopPx, &o.TimeInForce,
			&o.Status, &o.LastStatusReason, &o.SubmittedAt, &terminalAt,
		); err != nil {
			return nil, mapDBError(err)
		}
		o.TaskID = taskID
		o.ExecutionID = execID
		o.BrokerOrderID = brokerID
		o.LimitPrice = limitPx
		o.StopPrice = stopPx
		o.TerminalAt = terminalAt
		out = append(out, o)
	}
	return out, mapDBError(rows.Err())
}

// Count returns the total row count for the filter. Used by
// pagination + the headline soak-panel tiles ("orders today",
// "orders 7d") where we want a number without dragging the
// rows into memory.
func (r *TradingOrderRepository) Count(ctx context.Context, filter persistence.TradingOrderFilter) (int64, error) {
	q, args := buildTradingOrderQuery(filter, true)
	var n int64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}

// buildTradingOrderQuery is the shared query builder for List
// and Count. countOnly swaps the SELECT list for a COUNT(*)
// without re-implementing the WHERE clause.
func buildTradingOrderQuery(filter persistence.TradingOrderFilter, countOnly bool) (string, []any) {
	var b strings.Builder
	if countOnly {
		b.WriteString("SELECT COUNT(*) FROM trading_orders WHERE 1=1")
	} else {
		b.WriteString(`
			SELECT id, project_id, task_id, execution_id, broker_order_id,
			       idempotency_key, mode, symbol, action, order_type,
			       qty, limit_price, stop_price, time_in_force,
			       status, last_status_reason, submitted_at, terminal_at
			FROM trading_orders WHERE 1=1`)
	}
	args := make([]any, 0, 5)
	if filter.ProjectID != nil && *filter.ProjectID != "" {
		args = append(args, *filter.ProjectID)
		fmt.Fprintf(&b, " AND project_id = $%d", len(args))
	}
	if filter.Status != nil && *filter.Status != "" {
		args = append(args, *filter.Status)
		fmt.Fprintf(&b, " AND status = $%d", len(args))
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		args = append(args, *filter.Symbol)
		fmt.Fprintf(&b, " AND symbol = $%d", len(args))
	}
	if filter.Since != nil {
		args = append(args, filter.Since.UTC())
		fmt.Fprintf(&b, " AND submitted_at >= $%d", len(args))
	}
	if filter.Until != nil {
		args = append(args, filter.Until.UTC())
		fmt.Fprintf(&b, " AND submitted_at < $%d", len(args))
	}
	return b.String(), args
}

// ptrStringOrNil / ptrFloatOrNil / ptrTimeOrNil convert pointer-
// typed optionals to the `any` shape ExecContext expects, with
// nil pointers becoming SQL NULL. Distinct from the package's
// existing nullableString / nullableTime helpers (which return
// sql.NullString / sql.NullTime) — those are for sql.Null*
// scanning patterns elsewhere; ours flow into ExecContext args
// where untyped nil becomes NULL more cleanly.
func ptrStringOrNil(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func ptrFloatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func ptrTimeOrNil(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}
