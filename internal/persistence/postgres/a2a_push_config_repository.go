package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// A2APushConfigRepository implements persistence.A2APushConfigRepository
// against PostgreSQL.
type A2APushConfigRepository struct {
	db DBTX
}

// NewA2APushConfigRepository constructs a repo over db.
func NewA2APushConfigRepository(db DBTX) *A2APushConfigRepository {
	return &A2APushConfigRepository{db: db}
}

func (r *A2APushConfigRepository) Set(ctx context.Context, cfg persistence.A2APushConfig) error {
	if cfg.TaskID == "" || cfg.URL == "" {
		return fmt.Errorf("a2a push config: task_id and url required")
	}
	created := cfg.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var token any
	if cfg.Token != "" {
		token = cfg.Token
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO a2a_push_configs (task_id, url, token, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (task_id) DO UPDATE SET url = EXCLUDED.url, token = EXCLUDED.token, created_at = EXCLUDED.created_at`,
		cfg.TaskID, cfg.URL, token, created,
	)
	return mapDBError(err)
}

func (r *A2APushConfigRepository) Get(ctx context.Context, taskID string) (*persistence.A2APushConfig, error) {
	if taskID == "" {
		return nil, persistence.ErrNotFound
	}
	cfg := &persistence.A2APushConfig{TaskID: taskID}
	var token sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT url, token, created_at FROM a2a_push_configs WHERE task_id = $1`, taskID,
	).Scan(&cfg.URL, &token, &cfg.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	cfg.Token = token.String
	return cfg, nil
}
