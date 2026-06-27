package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingSafetyEventRepository persists broker-side safety decisions
// (kill-switch toggles, breaker trips, cap refusals). Append-only;
// duplicate ID inserts no-op via INSERT OR IGNORE.
type TradingSafetyEventRepository struct {
	db DBTX
}

func NewTradingSafetyEventRepository(db DBTX) *TradingSafetyEventRepository {
	return &TradingSafetyEventRepository{db: db}
}

// Record inserts one safety event row, idempotent on (id).
func (r *TradingSafetyEventRepository) Record(ctx context.Context, event *persistence.TradingSafetyEvent) error {
	if event == nil {
		return fmt.Errorf("nil safety event")
	}
	if event.ID == "" {
		return fmt.Errorf("safety event ID required")
	}
	if event.ProjectID == "" {
		return fmt.Errorf("project ID required")
	}
	if event.Kind == "" {
		return fmt.Errorf("kind required")
	}
	recordedAt := event.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	severity := event.Severity
	if severity == "" {
		severity = "info"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO trading_safety_events (
			id, project_id, recorded_at, kind, severity, symbol, detail
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.ProjectID, sqliteTime(recordedAt),
		event.Kind, severity, event.Symbol, nullableBlob(event.Detail),
	)
	return err
}

// List returns rows matching filter, newest-first.
func (r *TradingSafetyEventRepository) List(ctx context.Context, filter persistence.TradingSafetyEventFilter) ([]*persistence.TradingSafetyEvent, error) {
	q, args := buildSafetyEventQuerySqlite(filter, false)
	q += " ORDER BY recorded_at DESC"
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 1000 {
		filter.PageSize = 1000
	}
	q += " LIMIT ?"
	args = append(args, filter.PageSize)
	if filter.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, filter.Offset)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TradingSafetyEvent
	for rows.Next() {
		var (
			e          persistence.TradingSafetyEvent
			recordedAt sqlTime
			symbol     sql.NullString
			detail     sql.NullString
		)
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &recordedAt, &e.Kind, &e.Severity,
			&symbol, &detail,
		); err != nil {
			return nil, err
		}
		if symbol.Valid {
			e.Symbol = &symbol.String
		}
		if detail.Valid {
			e.Detail = []byte(detail.String)
		}
		e.RecordedAt = recordedAt.Time
		out = append(out, &e)
	}
	return out, rows.Err()
}

// Count returns the row count under filter.
func (r *TradingSafetyEventRepository) Count(ctx context.Context, filter persistence.TradingSafetyEventFilter) (int64, error) {
	q, args := buildSafetyEventQuerySqlite(filter, true)
	var n int64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func buildSafetyEventQuerySqlite(filter persistence.TradingSafetyEventFilter, countOnly bool) (string, []any) {
	var b strings.Builder
	if countOnly {
		b.WriteString("SELECT COUNT(*) FROM trading_safety_events WHERE 1=1")
	} else {
		b.WriteString(`
			SELECT id, project_id, recorded_at, kind, severity, symbol, detail
			FROM trading_safety_events WHERE 1=1`)
	}
	args := make([]any, 0, 5)
	if filter.ProjectID != nil && *filter.ProjectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Kind != nil && *filter.Kind != "" {
		b.WriteString(" AND kind = ?")
		args = append(args, *filter.Kind)
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		b.WriteString(" AND symbol = ?")
		args = append(args, *filter.Symbol)
	}
	if filter.Since != nil {
		b.WriteString(" AND recorded_at >= ?")
		args = append(args, sqliteTime(*filter.Since))
	}
	if filter.Until != nil {
		b.WriteString(" AND recorded_at < ?")
		args = append(args, sqliteTime(*filter.Until))
	}
	return b.String(), args
}
