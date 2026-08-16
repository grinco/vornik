package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CapabilityUsageRepository reports capability adoption per project.
//
// Shares persistence.CapabilitySignals with the Postgres implementation so the
// two cannot disagree about what a capability is or which table evidences it.
// Only the placeholder syntax and the missing-table tolerance differ.
type CapabilityUsageRepository struct{ db persistence.DBTX }

// NewCapabilityUsageRepository wires the reader.
func NewCapabilityUsageRepository(db persistence.DBTX) *CapabilityUsageRepository {
	return &CapabilityUsageRepository{db: db}
}

// Usage implements persistence.CapabilityUsageRepository.
//
// Unlike the Postgres path this queries each capability separately. A SQLite
// deployment may legitimately lack tables an enterprise schema has, and one
// absent table in a UNION fails the whole statement — which would report every
// capability as unused rather than the one that is missing. Per-capability
// queries let a missing table mean "not present here" without erasing the rest.
func (r *CapabilityUsageRepository) Usage(ctx context.Context, since time.Time) ([]persistence.CapabilityUsage, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("capability usage: no database")
	}
	var out []persistence.CapabilityUsage
	for _, c := range persistence.CapabilitySignals {
		proj, group := "''", ""
		if c.ProjectCol != "" {
			proj = c.ProjectCol
			group = " GROUP BY " + c.ProjectCol
		}
		where := c.TsCol + " >= ?"
		if c.Where != "" {
			where += " AND " + c.Where
		}
		q := fmt.Sprintf(
			"SELECT COALESCE(%s,'') AS p, count(*) AS n, max(%s) AS last FROM %s WHERE %s%s",
			proj, c.TsCol, c.Table, where, group)
		rows, err := r.db.QueryContext(ctx, q, since)
		if err != nil {
			// Absent table or column on this schema: report the capability as
			// unused rather than failing the whole report.
			out = append(out, persistence.CapabilityUsage{Key: c.Key})
			continue
		}
		sawRow := false
		for rows.Next() {
			var u persistence.CapabilityUsage
			var last sql.NullTime
			if err := rows.Scan(&u.ProjectID, &u.Count, &last); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("capability usage: scan %s: %w", c.Key, err)
			}
			u.Key = c.Key
			if last.Valid {
				t := last.Time
				u.LastUsed = &t
			}
			out = append(out, u)
			sawRow = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
		if !sawRow {
			// Every catalogued capability appears even with no rows: the unused
			// ones are the enablement list and must not be omitted.
			out = append(out, persistence.CapabilityUsage{Key: c.Key})
		}
	}
	return out, nil
}
