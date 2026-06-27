package postgres

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TradingSnapshotRepository implements
// persistence.TradingPositionsSnapshotRepository on PostgreSQL.
//
// The schema (migration 19) carries one row per sample with
// equity, cash, unrealised P/L, today's realised P/L, and the
// raw positions array as JSONB. The soak panel only needs the
// scalar fields for Sharpe/drawdown; the positions blob is
// kept on the row so a future per-symbol P&L panel doesn't
// need a second table.
type TradingSnapshotRepository struct {
	db DBTX
}

// NewTradingSnapshotRepository constructs a new repo over db.
func NewTradingSnapshotRepository(db DBTX) *TradingSnapshotRepository {
	return &TradingSnapshotRepository{db: db}
}

// Record inserts one snapshot row. ID is required (caller-
// generated to keep the repo simple). RecordedAt defaults to
// NOW() server-side when the caller leaves it zero — the
// sampler relies on that to keep its own clock out of the
// time series.
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

	// Default RecordedAt to NOW() at the DB so the sampler's
	// own clock skew doesn't drift the series. Pass NULL via
	// COALESCE when zero so the column default fires.
	var recordedAt any
	if snap.RecordedAt.IsZero() {
		recordedAt = nil
	} else {
		recordedAt = snap.RecordedAt.UTC()
	}

	// positions_json is NOT NULL on the schema; default to an
	// empty array when the caller didn't carry one so the
	// INSERT doesn't 23502 on a thin sample.
	positions := snap.PositionsJSON
	if len(positions) == 0 {
		positions = []byte(`[]`)
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO trading_positions_snapshots (
		    id, project_id, recorded_at, cash_usd, equity_usd,
		    unrealised_pl_usd, realised_pl_day_usd, positions_json
		) VALUES ($1, $2, COALESCE($3, NOW()), $4, $5, $6, $7, $8)`,
		snap.ID, snap.ProjectID, recordedAt,
		snap.CashUSD, snap.EquityUSD, snap.UnrealisedPLUSD,
		snap.RealisedPLDayUSD, positions,
	)
	return mapDBError(err)
}

// ListSince returns snapshots oldest-first so callers can
// iterate the time series without re-sorting. The cap defaults
// to 10000 — at 5-min cadence that's ~34 days, enough for the
// soak window. Callers wanting a longer history pass an
// explicit limit; passing < 0 raises an error rather than
// returning unbounded data.
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
		WHERE project_id = $1 AND recorded_at >= $2
		ORDER BY recorded_at ASC
		LIMIT $3`,
		projectID, since.UTC(), limit,
	)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TradingPositionsSnapshot
	for rows.Next() {
		s := &persistence.TradingPositionsSnapshot{}
		var positions []byte
		if err := rows.Scan(
			&s.ID, &s.ProjectID, &s.RecordedAt,
			&s.CashUSD, &s.EquityUSD, &s.UnrealisedPLUSD,
			&s.RealisedPLDayUSD, &positions,
		); err != nil {
			return nil, mapDBError(err)
		}
		if len(positions) > 0 {
			s.PositionsJSON = positions
		}
		out = append(out, s)
	}
	return out, mapDBError(rows.Err())
}
