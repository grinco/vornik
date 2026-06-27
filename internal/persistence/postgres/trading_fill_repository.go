package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingFillRepository implements
// persistence.TradingFillRepository on PostgreSQL.
//
// Append-only log keyed by id. The broker's poll loop generates
// a deterministic id per (order_id, filled_at) so a retried
// post under transient daemon outage collides on the PRIMARY
// KEY and silently no-ops. Same trust + idempotency contract
// as the orders + safety-events ingestion endpoints.
type TradingFillRepository struct {
	db DBTX
}

// NewTradingFillRepository constructs a new repo over db.
func NewTradingFillRepository(db DBTX) *TradingFillRepository {
	return &TradingFillRepository{db: db}
}

// Record inserts one fill row. ON CONFLICT (id) DO NOTHING so
// the broker's writer can re-post under transient outages
// without double-counting volume.
func (r *TradingFillRepository) Record(ctx context.Context, fill *persistence.TradingFill) error {
	if fill == nil {
		return fmt.Errorf("nil trading fill")
	}
	if fill.ID == "" {
		return fmt.Errorf("trading fill ID required")
	}
	if fill.OrderID == "" {
		return fmt.Errorf("order ID required")
	}
	if fill.ProjectID == "" {
		return fmt.Errorf("project ID required")
	}
	if fill.Symbol == "" {
		return fmt.Errorf("symbol required")
	}

	filledAt := fill.FilledAt
	if filledAt.IsZero() {
		filledAt = time.Now().UTC()
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_fills (
		    id, order_id, project_id, symbol,
		    qty, price, commission_usd, filled_at,
		    exec_id, account_id, source, source_detail
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO NOTHING`,
		fill.ID, fill.OrderID, fill.ProjectID, fill.Symbol,
		fill.Qty, fill.Price, ptrFloatOrNil(fill.CommissionUSD), filledAt,
		ptrStringOrNil(fill.ExecID), ptrStringOrNil(fill.AccountID),
		defaultStr(fill.Source, "reconcile"), ptrStringOrNil(fill.SourceDetail),
	)
	return mapDBError(err)
}

// MaxFilledAt returns the newest filled_at for a project (the exec-reconcile
// cursor seed). Returns the Unix epoch when the project has no fills yet.
func (r *TradingFillRepository) MaxFilledAt(ctx context.Context, projectID string) (time.Time, error) {
	var t sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(filled_at), to_timestamp(0)) FROM trading_fills WHERE project_id = $1`,
		projectID).Scan(&t)
	if err != nil {
		return time.Time{}, mapDBError(err)
	}
	return t.Time.UTC(), nil
}

// PatchCommission fills commission_usd only when still NULL — so a late
// commissionReportEvent can't overwrite a sweep-populated 0 with a stale value.
func (r *TradingFillRepository) PatchCommission(ctx context.Context, id string, commissionUSD float64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE trading_fills SET commission_usd = $1 WHERE id = $2 AND commission_usd IS NULL`,
		commissionUSD, id)
	return mapDBError(err)
}

// RecordShadow inserts into trading_fills_shadow (shadow-mode comparison;
// no FK to trading_orders). Idempotent on id.
func (r *TradingFillRepository) RecordShadow(ctx context.Context, fill *persistence.TradingFill) error {
	if fill == nil || fill.ID == "" {
		return fmt.Errorf("nil/empty shadow fill")
	}
	filledAt := fill.FilledAt
	if filledAt.IsZero() {
		filledAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_fills_shadow (
		    id, order_id, project_id, symbol, qty, price, commission_usd,
		    exec_id, account_id, source, source_detail, filled_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO NOTHING`,
		fill.ID, fill.OrderID, fill.ProjectID, fill.Symbol, fill.Qty, fill.Price,
		ptrFloatOrNil(fill.CommissionUSD), ptrStringOrNil(fill.ExecID),
		ptrStringOrNil(fill.AccountID), defaultStr(fill.Source, "reconcile"),
		ptrStringOrNil(fill.SourceDetail), filledAt)
	return mapDBError(err)
}

// defaultStr returns s if non-empty, otherwise def.
func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// List returns fills matching the filter, newest-first by
// filled_at. Default page size 100; cap at 5000 so a caller
// passing PageSize=0 doesn't trigger a full-table scan on a
// busy project.
func (r *TradingFillRepository) List(ctx context.Context, filter persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	var b strings.Builder
	args := []any{}
	b.WriteString(`SELECT id, order_id, project_id, symbol,
	                       qty, price, commission_usd, filled_at
	                FROM trading_fills WHERE 1=1`)

	if filter.ProjectID != nil && *filter.ProjectID != "" {
		args = append(args, *filter.ProjectID)
		fmt.Fprintf(&b, " AND project_id = $%d", len(args))
	}
	if filter.OrderID != nil && *filter.OrderID != "" {
		args = append(args, *filter.OrderID)
		fmt.Fprintf(&b, " AND order_id = $%d", len(args))
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		args = append(args, *filter.Symbol)
		fmt.Fprintf(&b, " AND symbol = $%d", len(args))
	}
	if filter.Since != nil {
		args = append(args, *filter.Since)
		fmt.Fprintf(&b, " AND filled_at >= $%d", len(args))
	}
	if filter.Until != nil {
		args = append(args, *filter.Until)
		fmt.Fprintf(&b, " AND filled_at < $%d", len(args))
	}
	b.WriteString(" ORDER BY filled_at DESC")
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 5000 {
		filter.PageSize = 5000
	}
	args = append(args, filter.PageSize)
	fmt.Fprintf(&b, " LIMIT $%d", len(args))
	if filter.Offset > 0 {
		args = append(args, filter.Offset)
		fmt.Fprintf(&b, " OFFSET $%d", len(args))
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TradingFill
	for rows.Next() {
		var f persistence.TradingFill
		var commission *float64
		if err := rows.Scan(&f.ID, &f.OrderID, &f.ProjectID, &f.Symbol,
			&f.Qty, &f.Price, &commission, &f.FilledAt); err != nil {
			return nil, mapDBError(err)
		}
		f.CommissionUSD = commission
		out = append(out, &f)
	}
	return out, mapDBError(rows.Err())
}

// ListNullCommission returns fills whose commission_usd IS NULL and whose
// filled_at is older than olderThan. The sweep queries this to find fills
// that never received a commission from the broker — either because the
// commissionReportEvent arrived out of order and was dropped, or because
// the broker never emitted one (IBKR can omit commission on partial fills).
//
// Returns id, exec_id, account_id, project_id, symbol, filled_at — enough
// to match each fill back to a broker execution via exec_id.
func (r *TradingFillRepository) ListNullCommission(ctx context.Context, olderThan time.Time) ([]*persistence.TradingFill, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, COALESCE(exec_id, ''), COALESCE(account_id, ''),
		       project_id, symbol, filled_at
		FROM trading_fills
		WHERE commission_usd IS NULL AND filled_at < $1`,
		olderThan)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TradingFill
	for rows.Next() {
		var f persistence.TradingFill
		var execID, accountID string
		if err := rows.Scan(&f.ID, &execID, &accountID, &f.ProjectID, &f.Symbol, &f.FilledAt); err != nil {
			return nil, mapDBError(err)
		}
		if execID != "" {
			f.ExecID = &execID
		}
		if accountID != "" {
			f.AccountID = &accountID
		}
		f.FilledAt = f.FilledAt.UTC()
		out = append(out, &f)
	}
	return out, mapDBError(rows.Err())
}

// SumVolume returns SUM(qty × price) over fills matching the
// filter. Used by the soak-panel volume tile — precise realised
// volume that excludes cancelled/refused flow (which the
// trading_orders-based estimate historically counted because
// orders inflate via limit_price the moment they're submitted).
//
// Filter respects the same project / order / symbol / time
// constraints as List; PageSize and Offset are ignored — SUM is
// inherently an aggregate over the full match set.
func (r *TradingFillRepository) SumVolume(ctx context.Context, filter persistence.TradingFillFilter) (float64, error) {
	var b strings.Builder
	args := []any{}
	b.WriteString(`SELECT COALESCE(SUM(qty * price), 0) FROM trading_fills WHERE 1=1`)

	if filter.ProjectID != nil && *filter.ProjectID != "" {
		args = append(args, *filter.ProjectID)
		fmt.Fprintf(&b, " AND project_id = $%d", len(args))
	}
	if filter.OrderID != nil && *filter.OrderID != "" {
		args = append(args, *filter.OrderID)
		fmt.Fprintf(&b, " AND order_id = $%d", len(args))
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		args = append(args, *filter.Symbol)
		fmt.Fprintf(&b, " AND symbol = $%d", len(args))
	}
	if filter.Since != nil {
		args = append(args, *filter.Since)
		fmt.Fprintf(&b, " AND filled_at >= $%d", len(args))
	}
	if filter.Until != nil {
		args = append(args, *filter.Until)
		fmt.Fprintf(&b, " AND filled_at < $%d", len(args))
	}

	var sum float64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&sum); err != nil {
		return 0, mapDBError(err)
	}
	return sum, nil
}
