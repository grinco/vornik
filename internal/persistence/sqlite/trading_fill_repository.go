package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Ensure TradingFillRepository satisfies the interface at compile time.
var _ persistence.TradingFillRepository = (*TradingFillRepository)(nil)

// TradingFillRepository persists fill events streamed from the
// broker MCP poll loop. INSERT-only — fills are append-only history.
type TradingFillRepository struct {
	db DBTX
}

func NewTradingFillRepository(db DBTX) *TradingFillRepository {
	return &TradingFillRepository{db: db}
}

// Record inserts one fill row; ON CONFLICT(id) DO NOTHING so the
// broker writer's retry-with-deterministic-id pattern stays
// idempotent.
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
	filledAt := fill.FilledAt
	if filledAt.IsZero() {
		filledAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO trading_fills (
			id, order_id, project_id, symbol,
			qty, price, commission_usd, filled_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fill.ID, fill.OrderID, fill.ProjectID, fill.Symbol,
		fill.Qty, fill.Price, fill.CommissionUSD, sqliteTime(filledAt),
	)
	return err
}

// List returns fills matching filter, newest-first.
func (r *TradingFillRepository) List(ctx context.Context, filter persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, order_id, project_id, symbol,
		       qty, price, commission_usd, filled_at
		FROM trading_fills WHERE 1=1`)
	args := buildTradingFillFilter(&b, filter)

	b.WriteString(" ORDER BY filled_at DESC")
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 5000 {
		filter.PageSize = 5000
	}
	b.WriteString(" LIMIT ?")
	args = append(args, filter.PageSize)
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TradingFill
	for rows.Next() {
		var (
			f          persistence.TradingFill
			commission sql.NullFloat64
			filledAt   sqlTime
		)
		if err := rows.Scan(
			&f.ID, &f.OrderID, &f.ProjectID, &f.Symbol,
			&f.Qty, &f.Price, &commission, &filledAt,
		); err != nil {
			return nil, err
		}
		if commission.Valid {
			v := commission.Float64
			f.CommissionUSD = &v
		}
		f.FilledAt = filledAt.Time
		out = append(out, &f)
	}
	return out, rows.Err()
}

// SumVolume returns SUM(qty * price) over fills matching filter.
func (r *TradingFillRepository) SumVolume(ctx context.Context, filter persistence.TradingFillFilter) (float64, error) {
	var b strings.Builder
	b.WriteString(`SELECT COALESCE(SUM(qty * price), 0) FROM trading_fills WHERE 1=1`)
	args := buildTradingFillFilter(&b, filter)
	var sum float64
	if err := r.db.QueryRowContext(ctx, b.String(), args...).Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

func buildTradingFillFilter(b *strings.Builder, filter persistence.TradingFillFilter) []any {
	args := []any{}
	if filter.ProjectID != nil && *filter.ProjectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.OrderID != nil && *filter.OrderID != "" {
		b.WriteString(" AND order_id = ?")
		args = append(args, *filter.OrderID)
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		b.WriteString(" AND symbol = ?")
		args = append(args, *filter.Symbol)
	}
	if filter.Since != nil {
		b.WriteString(" AND filled_at >= ?")
		args = append(args, sqliteTime(*filter.Since))
	}
	if filter.Until != nil {
		b.WriteString(" AND filled_at < ?")
		args = append(args, sqliteTime(*filter.Until))
	}
	return args
}

// MaxFilledAt returns the newest filled_at for a project (the exec-reconcile
// cursor seed). Returns the Unix epoch when the project has no fills yet.
func (r *TradingFillRepository) MaxFilledAt(ctx context.Context, projectID string) (time.Time, error) {
	epoch := time.Unix(0, 0).UTC()
	var raw sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT MAX(filled_at) FROM trading_fills WHERE project_id = ?`, projectID).Scan(&raw)
	if err != nil {
		return epoch, err
	}
	if !raw.Valid {
		return epoch, nil
	}
	return raw.Time.UTC(), nil
}

// PatchCommission fills commission_usd only when still NULL — so a late
// commissionReportEvent can't overwrite a sweep-populated 0 with a stale value.
func (r *TradingFillRepository) PatchCommission(ctx context.Context, id string, commissionUSD float64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE trading_fills SET commission_usd = ? WHERE id = ? AND commission_usd IS NULL`,
		commissionUSD, id)
	return err
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
	var execID, accountID, sourceDetail *string
	if fill.ExecID != nil {
		execID = fill.ExecID
	}
	if fill.AccountID != nil {
		accountID = fill.AccountID
	}
	if fill.SourceDetail != nil {
		sourceDetail = fill.SourceDetail
	}
	src := fill.Source
	if src == "" {
		src = "reconcile"
	}
	// recorded_at is NOT NULL with no column default on sqlite (Postgres
	// uses DEFAULT NOW()), so the sqlite layer must bind it explicitly —
	// mirroring how Record() binds filled_at. Omitting it caused a NOT NULL
	// violation at runtime in sqlite shadow mode.
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO trading_fills_shadow (
		    id, order_id, project_id, symbol, qty, price, commission_usd,
		    exec_id, account_id, source, source_detail, filled_at, recorded_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		fill.ID, fill.OrderID, fill.ProjectID, fill.Symbol, fill.Qty, fill.Price,
		ptrFloatOrNil(fill.CommissionUSD), execID, accountID, src, sourceDetail,
		sqliteTime(filledAt), sqliteTime(time.Now().UTC()))
	return err
}

// ListNullCommission returns fills whose commission_usd IS NULL and whose
// filled_at is older than olderThan — the commission backfill sweep's input.
// Returns id, exec_id, account_id, project_id, symbol, filled_at.
func (r *TradingFillRepository) ListNullCommission(ctx context.Context, olderThan time.Time) ([]*persistence.TradingFill, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, COALESCE(exec_id, ''), COALESCE(account_id, ''),
		       project_id, symbol, filled_at
		FROM trading_fills
		WHERE commission_usd IS NULL AND filled_at < ?`,
		sqliteTime(olderThan))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TradingFill
	for rows.Next() {
		var f persistence.TradingFill
		var execID, accountID string
		var filledAt sqlTime
		if err := rows.Scan(&f.ID, &execID, &accountID, &f.ProjectID, &f.Symbol, &filledAt); err != nil {
			return nil, err
		}
		if execID != "" {
			f.ExecID = &execID
		}
		if accountID != "" {
			f.AccountID = &accountID
		}
		f.FilledAt = filledAt.Time
		out = append(out, &f)
	}
	return out, rows.Err()
}

var _ = fmt.Errorf // keep imports tidy when fmt is only used for errors
