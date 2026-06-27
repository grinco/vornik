package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TelegramThreadRepository is the SQLite
// persistence.TelegramThreadRepository. Maps (chat_id, thread_id) →
// task_id for Telegram Forum Topic routing.
type TelegramThreadRepository struct {
	db DBTX
}

func NewTelegramThreadRepository(db DBTX) *TelegramThreadRepository {
	return &TelegramThreadRepository{db: db}
}

// Insert persists a new mapping. (chat_id, thread_id) UNIQUE
// conflict surfaces as ErrDuplicateKey.
func (r *TelegramThreadRepository) Insert(ctx context.Context, t *persistence.TelegramTaskThread) error {
	if t == nil {
		return fmt.Errorf("TelegramThreadRepository.Insert: nil thread")
	}
	if t.TaskID == "" {
		return fmt.Errorf("TelegramThreadRepository.Insert: task_id required")
	}
	if t.ChatID == 0 || t.ThreadID == 0 {
		return fmt.Errorf("TelegramThreadRepository.Insert: chat_id and thread_id required")
	}
	if t.ID == "" {
		t.ID = persistence.GenerateID("tgth")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO telegram_task_threads (id, task_id, chat_id, thread_id, topic_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.TaskID, t.ChatID, t.ThreadID, t.TopicName, sqliteTime(t.CreatedAt),
	)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return persistence.ErrDuplicateKey
	}
	return err
}

// GetByTask resolves the thread for a task.
func (r *TelegramThreadRepository) GetByTask(ctx context.Context, taskID string) (*persistence.TelegramTaskThread, error) {
	if taskID == "" {
		return nil, fmt.Errorf("TelegramThreadRepository.GetByTask: task_id required")
	}
	return scanTGThread(r.db.QueryRowContext(ctx,
		`SELECT id, task_id, chat_id, thread_id, topic_name, created_at, closed_at
		 FROM telegram_task_threads WHERE task_id = ?`, taskID))
}

// GetByThread resolves the task for an inbound (chat_id, thread_id).
func (r *TelegramThreadRepository) GetByThread(ctx context.Context, chatID, threadID int64) (*persistence.TelegramTaskThread, error) {
	if chatID == 0 || threadID == 0 {
		return nil, persistence.ErrNotFound
	}
	return scanTGThread(r.db.QueryRowContext(ctx,
		`SELECT id, task_id, chat_id, thread_id, topic_name, created_at, closed_at
		 FROM telegram_task_threads WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID))
}

// MarkClosed stamps closed_at=now() once. Idempotent.
func (r *TelegramThreadRepository) MarkClosed(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("TelegramThreadRepository.MarkClosed: task_id required")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE telegram_task_threads SET closed_at = ?
		 WHERE task_id = ? AND closed_at IS NULL`,
		sqliteTime(time.Now().UTC()), taskID)
	return err
}

func scanTGThread(row *sql.Row) (*persistence.TelegramTaskThread, error) {
	var (
		t         persistence.TelegramTaskThread
		createdAt sqlTime
		closedAt  sqlNullTime
	)
	err := row.Scan(&t.ID, &t.TaskID, &t.ChatID, &t.ThreadID, &t.TopicName, &createdAt, &closedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = createdAt.Time
	if closedAt.Valid {
		ts := closedAt.Time
		t.ClosedAt = &ts
	}
	return &t, nil
}
