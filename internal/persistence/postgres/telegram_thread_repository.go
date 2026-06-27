package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TelegramThreadRepository is the Postgres backing for
// telegram_task_threads (migration v28). Persists the
// (chat_id, message_thread_id) → task_id map so Telegram Forum
// Topic routing survives bot restarts. See
// https://docs.vornik.io
type TelegramThreadRepository struct {
	db DBTX
}

// NewTelegramThreadRepository constructs the repo over a DBTX.
func NewTelegramThreadRepository(db DBTX) *TelegramThreadRepository {
	return &TelegramThreadRepository{db: db}
}

// Insert writes a new mapping row. On (chat_id, thread_id) conflict,
// returns persistence.ErrDuplicateKey so the caller can re-resolve
// the winner's row via GetByTask. Generates an ID when empty.
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
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO telegram_task_threads (
			id, task_id, chat_id, thread_id, topic_name, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)
	`,
		t.ID, t.TaskID, t.ChatID, t.ThreadID, t.TopicName, t.CreatedAt,
	); err != nil {
		return mapDBError(err)
	}
	return nil
}

// GetByTask returns the thread mapped to the given task or
// ErrNotFound when none exists.
func (r *TelegramThreadRepository) GetByTask(ctx context.Context, taskID string) (*persistence.TelegramTaskThread, error) {
	if taskID == "" {
		return nil, fmt.Errorf("TelegramThreadRepository.GetByTask: task_id required")
	}
	return r.scanOne(ctx,
		`SELECT id, task_id, chat_id, thread_id, topic_name, created_at, closed_at
		   FROM telegram_task_threads WHERE task_id = $1`,
		taskID)
}

// GetByThread returns the thread for the (chat_id, thread_id) pair.
// Used to route inbound messages in a forum topic back to their
// task. Returns ErrNotFound when the pair is unknown — the caller
// falls through to the dispatcher path.
func (r *TelegramThreadRepository) GetByThread(ctx context.Context, chatID, threadID int64) (*persistence.TelegramTaskThread, error) {
	if chatID == 0 || threadID == 0 {
		return nil, persistence.ErrNotFound
	}
	return r.scanOne(ctx,
		`SELECT id, task_id, chat_id, thread_id, topic_name, created_at, closed_at
		   FROM telegram_task_threads WHERE chat_id = $1 AND thread_id = $2`,
		chatID, threadID)
}

// MarkClosed stamps closed_at=now() on the task's thread row.
// Idempotent: calling on an already-closed thread leaves closed_at
// at its existing value (we don't bump on every terminal-event
// retry so the timestamp reflects the actual close, not the last
// notification).
func (r *TelegramThreadRepository) MarkClosed(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("TelegramThreadRepository.MarkClosed: task_id required")
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE telegram_task_threads SET closed_at = now()
		   WHERE task_id = $1 AND closed_at IS NULL`,
		taskID,
	); err != nil {
		return mapDBError(err)
	}
	return nil
}

// scanOne runs a single-row query and decodes a TelegramTaskThread.
// Returns persistence.ErrNotFound when no row matches.
func (r *TelegramThreadRepository) scanOne(ctx context.Context, query string, args ...any) (*persistence.TelegramTaskThread, error) {
	var t persistence.TelegramTaskThread
	var closedAt sql.NullTime
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&t.ID, &t.TaskID, &t.ChatID, &t.ThreadID, &t.TopicName, &t.CreatedAt, &closedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	if closedAt.Valid {
		ts := closedAt.Time
		t.ClosedAt = &ts
	}
	return &t, nil
}
