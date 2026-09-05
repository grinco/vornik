package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProjectFirstSeenRepository implements persistence.ProjectFirstSeenRepository.
//
// CE defaults to SQLite, and the counter this backs under-reports adoption
// exactly where CE deployments live, so this is the lane that matters most.
type ProjectFirstSeenRepository struct {
	db *sql.DB
}

// NewProjectFirstSeenRepository wires the repository.
func NewProjectFirstSeenRepository(db *sql.DB) *ProjectFirstSeenRepository {
	return &ProjectFirstSeenRepository{db: db}
}

// MarkSeen inserts the marker and reports whether this call created it.
//
// RowsAffected rather than RETURNING: SQLite supports RETURNING only from 3.35,
// and this driver's floor is not worth raising for one statement. The insert is
// still ONE statement, which is the property that matters — a read-then-write
// would let a start racing a reload emit twice.
func (r *ProjectFirstSeenRepository) MarkSeen(ctx context.Context, projectID, source string) (bool, error) {
	if r == nil || r.db == nil {
		return false, fmt.Errorf("sqlite: project_first_seen: no database handle")
	}
	if projectID == "" {
		return false, fmt.Errorf("sqlite: project_first_seen: project_id required")
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO project_first_seen (project_id, first_seen_at, source)
		VALUES (?, ?, ?)
		ON CONFLICT (project_id) DO NOTHING`,
		projectID, sqliteTime(time.Now()), source)
	if err != nil {
		return false, fmt.Errorf("sqlite: project_first_seen mark seen: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Cannot tell new from existing. Report NOT-first: a missed event
		// under-counts by one, where a wrong one re-emits on every restart and
		// makes the whole series meaningless.
		return false, nil
	}
	return n > 0, nil
}

var _ persistence.ProjectFirstSeenRepository = (*ProjectFirstSeenRepository)(nil)
