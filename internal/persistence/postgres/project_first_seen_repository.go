package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ProjectFirstSeenRepository implements persistence.ProjectFirstSeenRepository.
type ProjectFirstSeenRepository struct {
	db persistence.DBTX
}

// NewProjectFirstSeenRepository wires the repository.
func NewProjectFirstSeenRepository(db persistence.DBTX) *ProjectFirstSeenRepository {
	return &ProjectFirstSeenRepository{db: db}
}

// MarkSeen inserts the marker and reports whether this call created it.
//
// ONE STATEMENT, and the RETURNING is what makes it one. `ON CONFLICT DO
// NOTHING` returns no row when the marker already existed, so `sql.ErrNoRows` IS
// the "already seen" answer — no second query, and therefore no window in which
// two daemons both read absent and both emit.
func (r *ProjectFirstSeenRepository) MarkSeen(ctx context.Context, projectID, source string) (bool, error) {
	if r == nil || r.db == nil {
		return false, fmt.Errorf("postgres: project_first_seen: no database handle")
	}
	if projectID == "" {
		// An empty id would claim one shared marker for every unnamed project,
		// so the first would emit and the rest would be silently swallowed.
		return false, fmt.Errorf("postgres: project_first_seen: project_id required")
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO project_first_seen (project_id, source)
		VALUES ($1, $2)
		ON CONFLICT (project_id) DO NOTHING
		RETURNING project_id`, projectID, source).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case errorsIsNoRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("postgres: project_first_seen mark seen: %w", mapDBError(err))
	}
}

// errorsIsNoRows keeps the switch above readable; sql.ErrNoRows is the
// already-seen signal rather than a failure.
func errorsIsNoRows(err error) bool { return err == sql.ErrNoRows }

var _ persistence.ProjectFirstSeenRepository = (*ProjectFirstSeenRepository)(nil)
