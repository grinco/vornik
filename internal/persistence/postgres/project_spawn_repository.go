package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProjectSpawnRepository persists project_spawns rows. See
// https://docs.vornik.io §5.2.
type ProjectSpawnRepository struct {
	db DBTX
}

func NewProjectSpawnRepository(db DBTX) *ProjectSpawnRepository {
	return &ProjectSpawnRepository{db: db}
}

func (r *ProjectSpawnRepository) Create(ctx context.Context, s *persistence.ProjectSpawn) error {
	if s == nil {
		return errors.New("project_spawns: nil row")
	}
	if s.ID == "" {
		s.ID = persistence.GenerateID("ps")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_spawns (
			id, parent_task_id, parent_project, parent_step_id,
			spawned_project, template_slug, params
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`,
		s.ID, s.ParentTaskID, s.ParentProject, s.ParentStepID,
		s.SpawnedProject, s.TemplateSlug, jsonbValue(s.Params),
	)
	return mapDBError(err)
}

func (r *ProjectSpawnRepository) GetBySpawnedProject(ctx context.Context, spawnedProjectID string) (*persistence.ProjectSpawn, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, parent_task_id, parent_project, parent_step_id,
		       spawned_project, template_slug, params, created_at
		FROM project_spawns
		WHERE spawned_project = $1
	`, spawnedProjectID)
	var (
		out    persistence.ProjectSpawn
		params []byte
	)
	if err := row.Scan(
		&out.ID, &out.ParentTaskID, &out.ParentProject, &out.ParentStepID,
		&out.SpawnedProject, &out.TemplateSlug, &params, &out.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	out.Params = params
	return &out, nil
}

func (r *ProjectSpawnRepository) CountForProjectSince(ctx context.Context, parentProjectID string, since time.Time) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM project_spawns
		WHERE parent_project = $1 AND created_at >= $2
	`, parentProjectID, since).Scan(&n)
	if err != nil {
		return 0, mapDBError(err)
	}
	return n, nil
}
