package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProjectSpawnRepository is the SQLite stub for spawn_project's
// lineage ledger. Same Postgres-only-in-v1 rationale as the
// CrossProjectCallRepository stub — the JSONB / TIMESTAMPTZ
// types don't translate cleanly to SQLite without a parallel
// schema, and the spawn handler nil-checks the repo for the
// disabled path. Stub satisfies the interface so storage Repos
// stays fully-populated on SQLite (tests + local dev).
type ProjectSpawnRepository struct {
	_ *sql.DB
}

func NewProjectSpawnRepository(db *sql.DB) *ProjectSpawnRepository {
	return &ProjectSpawnRepository{}
}

var ErrProjectSpawnSQLiteNotSupported = errors.New("project_spawns: spawn_project is Postgres-only in v1 (SQLite migration deferred)")

func (r *ProjectSpawnRepository) Create(context.Context, *persistence.ProjectSpawn) error {
	return ErrProjectSpawnSQLiteNotSupported
}

func (r *ProjectSpawnRepository) GetBySpawnedProject(context.Context, string) (*persistence.ProjectSpawn, error) {
	return nil, ErrProjectSpawnSQLiteNotSupported
}

func (r *ProjectSpawnRepository) CountForProjectSince(context.Context, string, time.Time) (int64, error) {
	return 0, ErrProjectSpawnSQLiteNotSupported
}
