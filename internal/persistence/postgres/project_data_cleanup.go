package postgres

import (
	"context"
	"fmt"
	"regexp"

	"vornik.io/vornik/internal/persistence"
)

// ProjectDataCleanupRepository implements persistence.ProjectDataDeleter
// against PostgreSQL. The deleter runs every DELETE in
// persistence.ProjectDataTables under one transaction so a mid-way
// failure rolls back cleanly — the archive sweeper retries on its
// next tick rather than fighting a partially-deleted state.
//
// Used by the archive-grace-period sweeper. Not wired anywhere else:
// the existing per-table repos handle row-level lifecycle for
// running projects; this deleter is specifically the
// "project-ID-wide hard wipe" path.
type ProjectDataCleanupRepository struct {
	db DBTX
}

// NewProjectDataCleanupRepository constructs the deleter over db.
// Pass a *sql.DB (so the deleter can begin its own transaction).
func NewProjectDataCleanupRepository(db DBTX) *ProjectDataCleanupRepository {
	return &ProjectDataCleanupRepository{db: db}
}

// safeTableName matches plain SQL identifiers. The table names in
// ProjectDataTables are hard-coded source-controlled constants but
// we still validate before string-concatenating into DDL so an
// accidental future entry with a quote / semicolon can't blow up
// the DELETE.
var safeTableName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// DeleteProjectData wipes every project-scoped row for projectID.
// Returns counts so the sweeper can write an audit row with
// "deleted N rows across M tables".
//
// Empty projectID returns an error — a blank ID would otherwise
// DELETE every row from every project table. Same defensive shape
// every other multi-row deleter in the codebase uses.
func (r *ProjectDataCleanupRepository) DeleteProjectData(ctx context.Context, projectID string) (persistence.ProjectDataStats, error) {
	if projectID == "" {
		return persistence.ProjectDataStats{}, fmt.Errorf("project_cleanup: empty projectID would wipe all projects")
	}

	stats := persistence.ProjectDataStats{}

	// persistence.BeginTx recognises both the raw *sql.DB pool and the
	// daemon's *DBWithMetrics wrapper. The previous inline assertion
	// only matched *sql.DB, so wiring this deleter through the metrics
	// wrapper would silently drop the all-or-nothing transaction and
	// run per-table autocommit DELETEs (bug sweep 2026-06-04).
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return stats, fmt.Errorf("project_cleanup: begin tx: %w", err)
	}
	if !ok {
		// DBTX is already a *sql.Tx — caller owns commit/rollback.
		// Run the DELETEs directly without nesting.
		for _, table := range persistence.ProjectDataTables {
			n, err := deleteForProject(ctx, r.db, table, projectID)
			if err != nil {
				return stats, err
			}
			stats.TablesCleared++
			stats.RowsDeleted += n
		}
		return stats, nil
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range persistence.ProjectDataTables {
		n, err := deleteForProject(ctx, tx, table, projectID)
		if err != nil {
			return stats, err
		}
		stats.TablesCleared++
		stats.RowsDeleted += n
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("project_cleanup: commit: %w", err)
	}
	return stats, nil
}

// deleteForProject runs DELETE FROM <table> WHERE project_id = $1.
// Table name validated against safeTableName before concatenation
// (the inputs are source-controlled but defensive is cheap here).
// Returns -1 for the row count when the driver doesn't report it.
func deleteForProject(ctx context.Context, exec persistence.DBTX, table, projectID string) (int64, error) {
	if !safeTableName.MatchString(table) {
		return 0, fmt.Errorf("project_cleanup: invalid table name %q", table)
	}
	q := "DELETE FROM " + table + " WHERE project_id = $1"
	res, err := exec.ExecContext(ctx, q, projectID)
	if err != nil {
		return 0, fmt.Errorf("project_cleanup: delete from %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return -1, nil
	}
	return n, nil
}
