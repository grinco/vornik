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

// TaskMessageRepository is the SQLite persistence.TaskMessageRepository.
//
// Insert maintains the task.message_count counter and (for
// checkpoints) task.open_checkpoint_id pointer in the same transaction
// as the message INSERT so the task row never disagrees with its
// messages — mirrors the Postgres housekeeping pattern.
type TaskMessageRepository struct {
	db DBTX
}

func NewTaskMessageRepository(db DBTX) *TaskMessageRepository {
	return &TaskMessageRepository{db: db}
}

// Insert writes one message + bumps the task housekeeping fields.
func (r *TaskMessageRepository) Insert(ctx context.Context, msg *persistence.TaskMessage) error {
	if msg == nil {
		return fmt.Errorf("TaskMessageRepository.Insert: nil message")
	}
	if msg.TaskID == "" {
		return fmt.Errorf("TaskMessageRepository.Insert: task_id required")
	}
	if msg.AuthorKind == "" || msg.MessageKind == "" {
		return fmt.Errorf("TaskMessageRepository.Insert: author_kind + message_kind required")
	}
	if msg.ID == "" {
		msg.ID = persistence.GenerateID("tmsg")
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	// Caller may have wrapped us in a Tx already; only wrap when
	// we can actually start one.
	if db, ok := r.db.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := r.insertWithExec(ctx, tx, msg); err != nil {
			return err
		}
		return tx.Commit()
	}
	return r.insertWithExec(ctx, r.db, msg)
}

func (r *TaskMessageRepository) insertWithExec(ctx context.Context, exec DBTX, msg *persistence.TaskMessage) error {
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO task_messages (
			id, task_id, execution_id, parent_id,
			author_kind, author_id, message_kind, content,
			metadata, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.TaskID, msg.ExecutionID, msg.ParentID,
		msg.AuthorKind, msg.AuthorID, msg.MessageKind, msg.Content,
		nullableBlob(msg.Metadata), sqliteTime(msg.CreatedAt),
	); err != nil {
		return err
	}

	// Bump the task's counter + open-checkpoint pointer.
	now := sqliteTime(time.Now().UTC())
	if msg.MessageKind == persistence.TaskMessageKindCheckpoint {
		_, err := exec.ExecContext(ctx, `
			UPDATE tasks
			SET message_count = message_count + 1,
			    open_checkpoint_id = ?,
			    updated_at = ?
			WHERE id = ?`,
			msg.ID, now, msg.TaskID)
		return err
	}
	_, err := exec.ExecContext(ctx, `
		UPDATE tasks
		SET message_count = message_count + 1,
		    updated_at = ?
		WHERE id = ?`,
		now, msg.TaskID)
	return err
}

// List returns messages for a task, chronological order, with a
// cursor (After) and a kind allow-list.
func (r *TaskMessageRepository) List(ctx context.Context, filter persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	if filter.TaskID == "" {
		return nil, fmt.Errorf("TaskMessageRepository.List: task_id required")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var b strings.Builder
	b.WriteString(`
		SELECT id, task_id, execution_id, parent_id,
		       author_kind, author_id, message_kind, content,
		       metadata, created_at
		FROM task_messages
		WHERE task_id = ?`)
	args := []any{filter.TaskID}

	if filter.After != nil && *filter.After != "" {
		b.WriteString(" AND id > ?")
		args = append(args, *filter.After)
	}
	if len(filter.MessageKinds) > 0 {
		placeholders := strings.Repeat("?,", len(filter.MessageKinds))
		placeholders = placeholders[:len(placeholders)-1]
		b.WriteString(" AND message_kind IN (" + placeholders + ")")
		for _, k := range filter.MessageKinds {
			args = append(args, k)
		}
	}
	b.WriteString(" ORDER BY created_at ASC, id ASC LIMIT ?")
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TaskMessage
	for rows.Next() {
		m, err := scanTaskMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetOpenCheckpoint returns the open checkpoint message for a task,
// or (nil, nil) when none exists.
func (r *TaskMessageRepository) GetOpenCheckpoint(ctx context.Context, taskID string) (*persistence.TaskMessage, error) {
	if taskID == "" {
		return nil, fmt.Errorf("TaskMessageRepository.GetOpenCheckpoint: task_id required")
	}
	var ptr sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT open_checkpoint_id FROM tasks WHERE id = ?`,
		taskID,
	).Scan(&ptr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !ptr.Valid || ptr.String == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, execution_id, parent_id,
		       author_kind, author_id, message_kind, content,
		       metadata, created_at
		FROM task_messages WHERE id = ?`, ptr.String)
	m, err := scanTaskMessage(row)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

// MarkCheckpointResolved flips the checkpoint message's metadata
// "resolved" flag and clears tasks.open_checkpoint_id.
func (r *TaskMessageRepository) MarkCheckpointResolved(ctx context.Context, taskID, checkpointID string) error {
	if taskID == "" || checkpointID == "" {
		return fmt.Errorf("MarkCheckpointResolved: task_id + checkpoint_id required")
	}
	// SQLite has json_set; we use it to merge {resolved: true} into
	// the existing metadata. COALESCE handles the NULL-metadata path.
	if _, err := r.db.ExecContext(ctx, `
		UPDATE task_messages
		SET metadata = json_set(COALESCE(CAST(metadata AS TEXT), '{}'), '$.resolved', json('true'))
		WHERE id = ? AND task_id = ? AND message_kind = 'checkpoint'`,
		checkpointID, taskID,
	); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET open_checkpoint_id = NULL, updated_at = ?
		WHERE id = ? AND open_checkpoint_id = ?`,
		sqliteTime(time.Now().UTC()), taskID, checkpointID)
	return err
}

func scanTaskMessage(scanner interface{ Scan(dest ...any) error }) (*persistence.TaskMessage, error) {
	var (
		m           persistence.TaskMessage
		executionID sql.NullString
		parentID    sql.NullString
		authorID    sql.NullString
		metadata    sql.NullString
		createdAt   sqlTime
	)
	err := scanner.Scan(
		&m.ID, &m.TaskID, &executionID, &parentID,
		&m.AuthorKind, &authorID, &m.MessageKind, &m.Content,
		&metadata, &createdAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if executionID.Valid {
		m.ExecutionID = &executionID.String
	}
	if parentID.Valid {
		m.ParentID = &parentID.String
	}
	if authorID.Valid {
		m.AuthorID = &authorID.String
	}
	if metadata.Valid {
		m.Metadata = []byte(metadata.String)
	}
	m.CreatedAt = createdAt.Time
	return &m, nil
}
