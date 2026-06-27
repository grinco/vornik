package sqlite

import (
	"context"
	"time"
)

// TaskWatcherRepository is the SQLite-backed implementation of
// persistence.TaskWatcherRepository. Watch is idempotent via the
// composite primary key on (task_id, chat_id); duplicate inserts
// land as no-ops to match the Postgres ON CONFLICT DO NOTHING.
type TaskWatcherRepository struct {
	db DBTX
}

// NewTaskWatcherRepository constructs a TaskWatcherRepository over db.
func NewTaskWatcherRepository(db DBTX) *TaskWatcherRepository {
	return &TaskWatcherRepository{db: db}
}

// Watch registers a chat for completion notifications on taskID.
// SQLite reports `UNIQUE constraint failed` on the duplicate path;
// `INSERT OR IGNORE` swallows that so the caller can re-register
// idempotently.
func (r *TaskWatcherRepository) Watch(ctx context.Context, taskID string, chatID int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO task_watchers (task_id, chat_id, created_at)
		VALUES (?, ?, ?)`,
		taskID, chatID, sqliteTime(time.Now()))
	return err
}

// GetWatchers returns every chat_id watching taskID.
func (r *TaskWatcherRepository) GetWatchers(ctx context.Context, taskID string) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT chat_id FROM task_watchers WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RemoveWatchers clears every watcher row for taskID.
func (r *TaskWatcherRepository) RemoveWatchers(ctx context.Context, taskID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM task_watchers WHERE task_id = ?`, taskID)
	return err
}
