package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingOrderRepository persists broker-side order events streamed
// via the broker→daemon audit channel.
//
// Identity check on (project_id, idempotency_key): if a row already
// exists with that pair but the (symbol, action, qty, limit_price)
// tuple differs, return persistence.ErrOrderIdentityMismatch —
// matches the Postgres safeguard that protects against the NVDA
// idempotency-key collision class.
type TradingOrderRepository struct {
	db DBTX
}

func NewTradingOrderRepository(db DBTX) *TradingOrderRepository {
	return &TradingOrderRepository{db: db}
}

// Record upserts one order row. Re-records with the same
// (project_id, idempotency_key) merge into the existing row when
// the identity tuple matches; mismatches return
// ErrOrderIdentityMismatch.
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, idempotency_key) DO UPDATE SET
			status              = excluded.status,
			last_status_reason  = excluded.last_status_reason,
			stop_price          = excluded.stop_price,
			terminal_at         = excluded.terminal_at,
			broker_order_id     = COALESCE(excluded.broker_order_id, trading_orders.broker_order_id),
			filled_qty          = excluded.filled_qty`,
		order.ID, order.ProjectID, order.TaskID, order.ExecutionID, order.BrokerOrderID,
		order.IdempotencyKey, order.Mode, order.Symbol, order.Action, order.OrderType,
		order.Qty, order.LimitPrice, order.StopPrice, order.TimeInForce,
		order.Status, order.LastStatusReason, sqliteTime(submittedAt), sqliteTimePtr(order.TerminalAt),
		order.FilledQty,
	)
	return err
}

func (r *TradingOrderRepository) checkIdentityMatch(ctx context.Context, order *persistence.TradingOrder) error {
	const q = `
		SELECT symbol, action, qty, COALESCE(limit_price, 0)
		FROM trading_orders
		WHERE project_id = ? AND idempotency_key = ?`
	var (
		existingSym    string
		existingAction string
		existingQty    float64
		existingLimit  float64
	)
	err := r.db.QueryRowContext(ctx, q, order.ProjectID, order.IdempotencyKey).Scan(
		&existingSym, &existingAction, &existingQty, &existingLimit,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
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

// List returns trading_orders matching filter, newest-first.
func (r *TradingOrderRepository) List(ctx context.Context, filter persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	q, args := buildTradingOrderQuerySqlite(filter, false)
	q += " ORDER BY submitted_at DESC"
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 5000 {
		filter.PageSize = 5000
	}
	args = append(args, filter.PageSize)
	q += " LIMIT ?"
	if filter.Offset > 0 {
		args = append(args, filter.Offset)
		q += " OFFSET ?"
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TradingOrder
	for rows.Next() {
		o, err := scanTradingOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Count returns the row count under filter.
func (r *TradingOrderRepository) Count(ctx context.Context, filter persistence.TradingOrderFilter) (int64, error) {
	q, args := buildTradingOrderQuerySqlite(filter, true)
	var n int64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func buildTradingOrderQuerySqlite(filter persistence.TradingOrderFilter, countOnly bool) (string, []any) {
	var b strings.Builder
	if countOnly {
		b.WriteString(`SELECT COUNT(*) FROM trading_orders WHERE 1=1`)
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
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Status != nil && *filter.Status != "" {
		b.WriteString(" AND status = ?")
		args = append(args, *filter.Status)
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		b.WriteString(" AND symbol = ?")
		args = append(args, *filter.Symbol)
	}
	if filter.Since != nil {
		b.WriteString(" AND submitted_at >= ?")
		args = append(args, sqliteTime(*filter.Since))
	}
	if filter.Until != nil {
		b.WriteString(" AND submitted_at < ?")
		args = append(args, sqliteTime(*filter.Until))
	}
	return b.String(), args
}

func scanTradingOrder(scanner interface{ Scan(dest ...any) error }) (*persistence.TradingOrder, error) {
	o := &persistence.TradingOrder{}
	var (
		taskID, execID, brokerID sql.NullString
		limitPx, stopPx          sql.NullFloat64
		submittedAt              sqlTime
		terminalAt               sqlNullTime
	)
	err := scanner.Scan(
		&o.ID, &o.ProjectID, &taskID, &execID, &brokerID,
		&o.IdempotencyKey, &o.Mode, &o.Symbol, &o.Action, &o.OrderType,
		&o.Qty, &limitPx, &stopPx, &o.TimeInForce,
		&o.Status, &o.LastStatusReason, &submittedAt, &terminalAt,
	)
	if err != nil {
		return nil, err
	}
	if taskID.Valid {
		o.TaskID = &taskID.String
	}
	if execID.Valid {
		o.ExecutionID = &execID.String
	}
	if brokerID.Valid {
		o.BrokerOrderID = &brokerID.String
	}
	if limitPx.Valid {
		v := limitPx.Float64
		o.LimitPrice = &v
	}
	if stopPx.Valid {
		v := stopPx.Float64
		o.StopPrice = &v
	}
	o.SubmittedAt = submittedAt.Time
	if terminalAt.Valid {
		t := terminalAt.Time
		o.TerminalAt = &t
	}
	return o, nil
}
