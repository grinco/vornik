package sqlite

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionLiveEventRepository is the SQLite stub. Single-process
// deployments don't need cross-replica fanout; the in-memory
// livepubsub publisher already covers them. Persistence here
// would just burn IOPS for no observable benefit, so Append is a
// no-op returning the requested-as-zero seq and ListSince /
// LatestSeq report "nothing stored". The publisher wrapper
// notices the no-op and skips its NOTIFY path.
type ExecutionLiveEventRepository struct {
	_ *sql.DB
}

// NewExecutionLiveEventRepository returns the stub.
func NewExecutionLiveEventRepository(db *sql.DB) *ExecutionLiveEventRepository {
	return &ExecutionLiveEventRepository{}
}

// Append always returns (0, nil). The wrapping publisher uses
// the in-process seq for the local subscriber path; the DB seq
// returned here is unused on SQLite.
func (r *ExecutionLiveEventRepository) Append(_ context.Context, _, _ string, _ []byte) (int64, error) {
	return 0, nil
}

func (r *ExecutionLiveEventRepository) ListSince(_ context.Context, _ string, _ int64, _ int) ([]*persistence.ExecutionLiveEvent, error) {
	return nil, nil
}

func (r *ExecutionLiveEventRepository) LatestSeq(_ context.Context, _ string) (int64, error) {
	return -1, nil
}

func (r *ExecutionLiveEventRepository) DeleteOlderThan(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
