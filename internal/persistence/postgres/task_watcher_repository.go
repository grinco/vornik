package postgres

import (
	"context"
	"time"
)

// TaskWatcherRepository implements persistence.TaskWatcherRepository using PostgreSQL.
type TaskWatcherRepository struct {
	db DBTX
}

// NewTaskWatcherRepository creates a new TaskWatcherRepository.
func NewTaskWatcherRepository(db DBTX) *TaskWatcherRepository {
	return &TaskWatcherRepository{db: db}
}

// Watch registers a chat to be notified when a task completes.
func (r *TaskWatcherRepository) Watch(ctx context.Context, taskID string, chatID int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_watchers (task_id, chat_id, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (task_id, chat_id) DO NOTHING`,
		taskID, chatID, time.Now())
	return err
}

// GetWatchers returns all chat IDs watching a task.
func (r *TaskWatcherRepository) GetWatchers(ctx context.Context, taskID string) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT chat_id FROM task_watchers WHERE task_id = $1`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var chatIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		chatIDs = append(chatIDs, id)
	}
	return chatIDs, rows.Err()
}

// RemoveWatchers deletes all watchers for a task (after notification).
func (r *TaskWatcherRepository) RemoveWatchers(ctx context.Context, taskID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM task_watchers WHERE task_id = $1`, taskID)
	return err
}
