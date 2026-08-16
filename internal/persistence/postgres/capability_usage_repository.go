package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// CapabilityUsageRepository reports capability adoption per project.
type CapabilityUsageRepository struct{ db persistence.DBTX }

// NewCapabilityUsageRepository wires the reader.
func NewCapabilityUsageRepository(db persistence.DBTX) *CapabilityUsageRepository {
	return &CapabilityUsageRepository{db: db}
}

// Usage implements persistence.CapabilityUsageRepository.
func (r *CapabilityUsageRepository) Usage(ctx context.Context, since time.Time) ([]persistence.CapabilityUsage, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("capability usage: no database")
	}
	var parts []string
	for _, c := range persistence.CapabilitySignals {
		proj := "''"
		group := ""
		if c.ProjectCol != "" {
			proj = c.ProjectCol
			group = " GROUP BY " + c.ProjectCol
		}
		where := fmt.Sprintf("%s >= $1", c.TsCol)
		if c.Where != "" {
			where += " AND " + c.Where
		}
		// COALESCE on the project column: a NULL project is real data and must
		// not vanish from a count that claims to be complete.
		parts = append(parts, fmt.Sprintf(
			"SELECT '%s' AS k, COALESCE(%s,'') AS p, count(*) AS n, max(%s) AS last FROM %s WHERE %s%s",
			c.Key, proj, c.TsCol, c.Table, where, group))
	}
	rows, err := r.db.QueryContext(ctx, strings.Join(parts, " UNION ALL "), since)
	if err != nil {
		return nil, fmt.Errorf("capability usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}
	var out []persistence.CapabilityUsage
	for rows.Next() {
		var u persistence.CapabilityUsage
		var last sql.NullTime
		if err := rows.Scan(&u.Key, &u.ProjectID, &u.Count, &last); err != nil {
			return nil, fmt.Errorf("capability usage: scan: %w", err)
		}
		if last.Valid {
			t := last.Time
			u.LastUsed = &t
		}
		if u.Count > 0 {
			seen[u.Key] = true
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Every catalogued capability appears, used or not. The unused ones are the
	// enablement list and an inner join would silently drop exactly them.
	for _, c := range persistence.CapabilitySignals {
		if !seen[c.Key] {
			out = append(out, persistence.CapabilityUsage{Key: c.Key})
		}
	}
	return out, nil
}
