package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingSafetyEventRepository implements
// persistence.TradingSafetyEventRepository on PostgreSQL.
//
// Append-only — Record uses ON CONFLICT (id) DO NOTHING so a
// retried POST from the broker's audit writer (after a
// transient daemon outage) is a silent no-op. Safety events
// are immutable history; updates after the fact would erase
// the cross-component audit trail this table exists for.
type TradingSafetyEventRepository struct {
	db DBTX
}

// NewTradingSafetyEventRepository constructs a new repo over db.
func NewTradingSafetyEventRepository(db DBTX) *TradingSafetyEventRepository {
	return &TradingSafetyEventRepository{db: db}
}

// Record inserts one safety event row. The retry path through
// the broker's AuditWriter sends the same id repeatedly under
// transient outages; ON CONFLICT keeps the table clean.
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

	detail := event.Detail
	if len(detail) == 0 {
		detail = nil // SQL NULL — schema column is JSONB nullable
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_safety_events (
		    id, project_id, recorded_at, kind, severity, symbol, detail
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		event.ID, event.ProjectID, recordedAt, event.Kind, severity,
		ptrStringOrNil(event.Symbol), detail,
	)
	return mapDBError(err)
}

// List returns trading_safety_events rows matching the filter,
// newest-first. Default page size 100; cap at 1000 so a
// caller passing PageSize=0 doesn't trigger a full-table scan.
func (r *TradingSafetyEventRepository) List(ctx context.Context, filter persistence.TradingSafetyEventFilter) ([]*persistence.TradingSafetyEvent, error) {
	q, args := buildSafetyEventQuery(filter, false)
	q += " ORDER BY recorded_at DESC"
	if filter.PageSize <= 0 {
		filter.PageSize = 100
	}
	if filter.PageSize > 1000 {
		filter.PageSize = 1000
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

	var out []*persistence.TradingSafetyEvent
	for rows.Next() {
		e := &persistence.TradingSafetyEvent{}
		var symbol *string
		var detail []byte
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.RecordedAt, &e.Kind, &e.Severity,
			&symbol, &detail,
		); err != nil {
			return nil, mapDBError(err)
		}
		e.Symbol = symbol
		if len(detail) > 0 {
			e.Detail = detail
		}
		out = append(out, e)
	}
	return out, mapDBError(rows.Err())
}

// Count returns the total row count for the filter.
func (r *TradingSafetyEventRepository) Count(ctx context.Context, filter persistence.TradingSafetyEventFilter) (int64, error) {
	q, args := buildSafetyEventQuery(filter, true)
	var n int64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}

// buildSafetyEventQuery is the shared query builder for List
// and Count. countOnly swaps the SELECT list for a COUNT(*).
func buildSafetyEventQuery(filter persistence.TradingSafetyEventFilter, countOnly bool) (string, []any) {
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
		args = append(args, *filter.ProjectID)
		fmt.Fprintf(&b, " AND project_id = $%d", len(args))
	}
	if filter.Kind != nil && *filter.Kind != "" {
		args = append(args, *filter.Kind)
		fmt.Fprintf(&b, " AND kind = $%d", len(args))
	}
	if filter.Symbol != nil && *filter.Symbol != "" {
		args = append(args, *filter.Symbol)
		fmt.Fprintf(&b, " AND symbol = $%d", len(args))
	}
	if filter.Since != nil {
		args = append(args, filter.Since.UTC())
		fmt.Fprintf(&b, " AND recorded_at >= $%d", len(args))
	}
	if filter.Until != nil {
		args = append(args, filter.Until.UTC())
		fmt.Fprintf(&b, " AND recorded_at < $%d", len(args))
	}
	return b.String(), args
}
