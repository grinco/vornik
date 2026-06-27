package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingSnapshotRepository persists the equity/cash/P&L time series.
type TradingSnapshotRepository struct {
	db DBTX
}

func NewTradingSnapshotRepository(db DBTX) *TradingSnapshotRepository {
	return &TradingSnapshotRepository{db: db}
}

// Record inserts one snapshot row.
func (r *TradingSnapshotRepository) Record(ctx context.Context, snap *persistence.TradingPositionsSnapshot) error {
	if snap == nil {
		return fmt.Errorf("nil snapshot")
	}
	if snap.ID == "" {
		return fmt.Errorf("snapshot ID required")
	}
	if snap.ProjectID == "" {
		return fmt.Errorf("project ID required")
	}
	recordedAt := snap.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	positions := snap.PositionsJSON
	if len(positions) == 0 {
		positions = []byte(`[]`)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_positions_snapshots (
			id, project_id, recorded_at, cash_usd, equity_usd,
			unrealised_pl_usd, realised_pl_day_usd, positions_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.ProjectID, sqliteTime(recordedAt),
		snap.CashUSD, snap.EquityUSD, snap.UnrealisedPLUSD,
		snap.RealisedPLDayUSD, positions,
	)
	return err
}

// ListSince returns snapshots oldest-first within [since, now].
func (r *TradingSnapshotRepository) ListSince(ctx context.Context, projectID string, since time.Time, limit int) ([]*persistence.TradingPositionsSnapshot, error) {
	if projectID == "" {
		return nil, fmt.Errorf("project ID required")
	}
	if limit < 0 {
		return nil, fmt.Errorf("limit must be >= 0")
	}
	if limit == 0 {
		limit = 10000
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, recorded_at, cash_usd, equity_usd,
		       unrealised_pl_usd, realised_pl_day_usd, positions_json
		FROM trading_positions_snapshots
		WHERE project_id = ? AND recorded_at >= ?
		ORDER BY recorded_at ASC
		LIMIT ?`,
		projectID, sqliteTime(since), limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TradingPositionsSnapshot
	for rows.Next() {
		var (
			s          persistence.TradingPositionsSnapshot
			recordedAt sqlTime
			positions  sql.NullString
		)
		if err := rows.Scan(
			&s.ID, &s.ProjectID, &recordedAt,
			&s.CashUSD, &s.EquityUSD, &s.UnrealisedPLUSD,
			&s.RealisedPLDayUSD, &positions,
		); err != nil {
			return nil, err
		}
		s.RecordedAt = recordedAt.Time
		if positions.Valid {
			s.PositionsJSON = []byte(positions.String)
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}
