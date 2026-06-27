package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// A2APushConfigRepository implements persistence.A2APushConfigRepository
// against SQLite.
type A2APushConfigRepository struct {
	db *sql.DB
}

// NewA2APushConfigRepository constructs a repo over db.
func NewA2APushConfigRepository(db *sql.DB) *A2APushConfigRepository {
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
		VALUES (?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET url = excluded.url, token = excluded.token, created_at = excluded.created_at`,
		cfg.TaskID, cfg.URL, token, sqliteTime(created),
	)
	return err
}

func (r *A2APushConfigRepository) Get(ctx context.Context, taskID string) (*persistence.A2APushConfig, error) {
	if taskID == "" {
		return nil, persistence.ErrNotFound
	}
	cfg := &persistence.A2APushConfig{TaskID: taskID}
	var token sql.NullString
	var createdText string
	err := r.db.QueryRowContext(ctx,
		`SELECT url, token, created_at FROM a2a_push_configs WHERE task_id = ?`, taskID,
	).Scan(&cfg.URL, &token, &createdText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cfg.Token = token.String
	if t, perr := time.Parse(time.RFC3339Nano, createdText); perr == nil {
		cfg.CreatedAt = t
	}
	return cfg, nil
}
