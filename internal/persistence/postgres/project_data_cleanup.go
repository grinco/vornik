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
	// evictCache removes rows a project-scoped DELETE cannot reach. See
	// ProjectCacheEvictor.
	evictCache ProjectCacheEvictor
}

// ProjectCacheEvictor deletes rows derived from a project's data that carry no
// project_id column, so ProjectDataTables cannot reach them.
//
// The only member today is embedding_cache, keyed (content_hash, model): the
// vector computed from a project's text survived the project, because a
// project-scoped DELETE has nothing to match on. Every other chunk-deletion
// path already evicts it, which made this one the outlier.
//
// It is a HOOK rather than a direct call because the derivation lives in
// internal/memory (the cache key is a hash of the CONTEXTUALISED embed input,
// not of content_hash), and internal/memory imports internal/persistence — this
// package cannot import it back. The wiring layer supplies
// memory.EvictProjectEmbeddingCache.
//
// It runs INSIDE the wipe's transaction and BEFORE any DELETE, because the keys
// are computed from the chunks' own source_name and content: afterwards there
// is nothing left to compute them from.
type ProjectCacheEvictor func(ctx context.Context, tx persistence.DBTX, projectID string) (int, error)

// NewProjectDataCleanupRepository constructs the deleter over db.
// Pass a *sql.DB (so the deleter can begin its own transaction).
//
// A nil evictor PANICS. It is a wiring mistake, not a configuration: the wipe it
// produces deletes a project's chunks and keeps every vector derived from them,
// and the only trace is a false in an audit row somebody has to read afterwards.
// Failing at construction puts that at daemon start, deterministically, instead
// of at the first deletion.
//
// Reporting the omission (ProjectDataStats.CacheEvictionRan) is still there and
// still necessary — it is what makes an audit row honest — but the 2026-09-07
// review's point stands: an honest record of a gap is not the same as not having
// the gap, and it only becomes visible after a deletion has already happened.
//
// Use NewProjectDataCleanupRepositoryWithoutCacheEviction when the omission is
// deliberate, so it reads as a decision in the diff rather than an oversight.
func NewProjectDataCleanupRepository(db DBTX, evictCache ProjectCacheEvictor) *ProjectDataCleanupRepository {
	if evictCache == nil {
		panic("postgres: NewProjectDataCleanupRepository needs a ProjectCacheEvictor; " +
			"without one a project wipe keeps every vector derived from the deleted " +
			"content. Use NewProjectDataCleanupRepositoryWithoutCacheEviction if that " +
			"is genuinely intended.")
	}
	return &ProjectDataCleanupRepository{db: db, evictCache: evictCache}
}

// NewProjectDataCleanupRepositoryWithoutCacheEviction constructs the deleter with
// no derived-row eviction, deliberately.
//
// The wipe then leaves embedding_cache rows behind — which is the pre-2026-09-07
// behaviour and a data-retention gap, so this exists for callers that do not
// exercise the memory tables at all (the transaction-shape tests) rather than as
// a supported deployment shape. ProjectDataStats.CacheEvictionRan reports false
// for anything built this way.
func NewProjectDataCleanupRepositoryWithoutCacheEviction(db DBTX) *ProjectDataCleanupRepository {
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
		if err := r.runCacheEviction(ctx, r.db, projectID, &stats); err != nil {
			return stats, err
		}
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

	// BEFORE the table loop: the cache keys are derived from the chunks this
	// wipe is about to delete.
	if err := r.runCacheEviction(ctx, tx, projectID, &stats); err != nil {
		return stats, err
	}

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

// runCacheEviction invokes the evictor, if one is wired, and records BOTH the
// count and whether it ran. A zero that could mean "nothing to evict" or "never
// attempted" is the shape of control this codebase treats as a defect.
func (r *ProjectDataCleanupRepository) runCacheEviction(ctx context.Context, tx persistence.DBTX, projectID string, stats *persistence.ProjectDataStats) error {
	if r.evictCache == nil {
		return nil
	}
	n, err := r.evictCache(ctx, tx, projectID)
	if err != nil {
		return fmt.Errorf("project_cleanup: evict derived cache: %w", err)
	}
	stats.CacheEvictionRan = true
	stats.CachedEmbeddingsEvicted = n
	return nil
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
