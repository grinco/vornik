package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// TaskMessageRepository is the Postgres backing for the task_messages
// table introduced in migration v24 (Phase 23 of the conversational
// task lifecycle; see
// https://docs.vornik.io).
//
// Each row is one entry in a task's persistent conversation. The
// thread can span weeks of calendar time and many executions.
type TaskMessageRepository struct {
	db DBTX
}

// NewTaskMessageRepository constructs the repo over a DBTX.
func NewTaskMessageRepository(db DBTX) *TaskMessageRepository {
	return &TaskMessageRepository{db: db}
}

// Insert writes one message. Generates an ID when empty so callers
// can let the repo own ID assignment. CreatedAt defaults to now()
// server-side; if the caller pre-fills it (replay/import paths),
// the explicit value wins.
//
// Side effect: updates tasks.message_count and, when the message is
// a checkpoint, sets tasks.open_checkpoint_id. Both maintenance ops
// run in the same transaction so the task row never disagrees with
// its messages.
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

	// Insert + housekeeping ride one transaction so a partial state
	// (message persisted but message_count stale) can never appear.
	//
	// persistence.BeginTx recognises both the raw *sql.DB pool and the
	// daemon's *DBWithMetrics wrapper. The previous r.db.(beginCtx)
	// assertion only matched *sql.DB, so under the metrics-wrapped
	// handle the daemon injects ok was always false and the two
	// statements ran non-transactionally — exactly the partial state
	// this code claims to prevent (bug sweep 2026-06-04).
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		// DBTX is an *sql.Tx already (caller wrapped us in a tx).
		// Run statements directly; caller owns commit/rollback.
		return r.insertWithExec(ctx, r.db, msg)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.insertWithExec(ctx, tx, msg); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

// insertWithExec runs the INSERT + housekeeping ops against any
// DBTX (real *sql.Tx or the bare DB pool when caller already holds
// a transaction).
func (r *TaskMessageRepository) insertWithExec(ctx context.Context, exec DBTX, msg *persistence.TaskMessage) error {
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO task_messages (
			id, task_id, execution_id, parent_id,
			author_kind, author_id, message_kind, content,
			metadata, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		msg.ID, msg.TaskID, msg.ExecutionID, msg.ParentID,
		msg.AuthorKind, msg.AuthorID, msg.MessageKind, msg.Content,
		jsonOrNull(msg.Metadata), msg.CreatedAt,
	); err != nil {
		return mapDBError(err)
	}

	// Bump the task's denormalised counter + open-checkpoint
	// pointer. The counter drives UI badges; the pointer drives
	// the inbox query without joining task_messages.
	updateSQL := `
		UPDATE tasks
		SET message_count = message_count + 1,
		    updated_at    = now()
	`
	args := []any{}
	if msg.MessageKind == persistence.TaskMessageKindCheckpoint {
		updateSQL += `, open_checkpoint_id = $1 WHERE id = $2`
		args = append(args, msg.ID, msg.TaskID)
	} else {
		updateSQL += ` WHERE id = $1`
		args = append(args, msg.TaskID)
	}
	if _, err := exec.ExecContext(ctx, updateSQL, args...); err != nil {
		return mapDBError(err)
	}
	return nil
}

// List returns messages for a task in chronological order. Filter
// supports a cursor (After) and a kind allow-list. Limit is capped
// at 500 to keep the API surface bounded; callers paginate.
func (r *TaskMessageRepository) List(ctx context.Context, filter persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	if filter.TaskID == "" {
		return nil, fmt.Errorf("TaskMessageRepository.List: task_id required")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	query := `
		SELECT id, task_id, execution_id, parent_id,
		       author_kind, author_id, message_kind, content,
		       metadata, created_at
		FROM task_messages
		WHERE task_id = $1`
	args := []any{filter.TaskID}
	pos := 2

	if filter.After != nil && *filter.After != "" {
		query += fmt.Sprintf(` AND id > $%d`, pos)
		args = append(args, *filter.After)
		pos++
	}
	if len(filter.MessageKinds) > 0 {
		query += fmt.Sprintf(` AND message_kind = ANY($%d)`, pos)
		args = append(args, pq.Array(filter.MessageKinds))
		pos++
	}
	query += fmt.Sprintf(` ORDER BY created_at ASC, id ASC LIMIT $%d`, pos)
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*persistence.TaskMessage, 0)
	for rows.Next() {
		var m persistence.TaskMessage
		var meta sql.NullString
		if err := rows.Scan(
			&m.ID, &m.TaskID, &m.ExecutionID, &m.ParentID,
			&m.AuthorKind, &m.AuthorID, &m.MessageKind, &m.Content,
			&meta, &m.CreatedAt,
		); err != nil {
			return nil, err
		}
		if meta.Valid && meta.String != "" {
			m.Metadata = []byte(meta.String)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// GetOpenCheckpoint returns the unresolved checkpoint message, if
// any. We read tasks.open_checkpoint_id (the cheap path) rather
// than joining + filtering on metadata, because the pointer is
// kept consistent by Insert + MarkCheckpointResolved.
func (r *TaskMessageRepository) GetOpenCheckpoint(ctx context.Context, taskID string) (*persistence.TaskMessage, error) {
	if taskID == "" {
		return nil, fmt.Errorf("TaskMessageRepository.GetOpenCheckpoint: task_id required")
	}
	var ptr sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT open_checkpoint_id FROM tasks WHERE id = $1`,
		taskID,
	).Scan(&ptr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	if !ptr.Valid || ptr.String == "" {
		return nil, nil
	}

	var m persistence.TaskMessage
	var meta sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT id, task_id, execution_id, parent_id,
		       author_kind, author_id, message_kind, content,
		       metadata, created_at
		FROM task_messages WHERE id = $1`, ptr.String,
	).Scan(
		&m.ID, &m.TaskID, &m.ExecutionID, &m.ParentID,
		&m.AuthorKind, &m.AuthorID, &m.MessageKind, &m.Content,
		&meta, &m.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Pointer dangling — the message was deleted out of
			// band. Clear the pointer + return no checkpoint so
			// the task can advance.
			_, _ = r.db.ExecContext(ctx,
				`UPDATE tasks SET open_checkpoint_id = NULL WHERE id = $1`,
				taskID)
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	if meta.Valid && meta.String != "" {
		m.Metadata = []byte(meta.String)
	}
	return &m, nil
}

// MarkCheckpointResolved flips the checkpoint's metadata.resolved
// flag and clears the task's open_checkpoint_id pointer in one
// statement run. Idempotent: a second call is a no-op (the metadata
// JSON merge keeps resolved=true; the task pointer is already NULL).
func (r *TaskMessageRepository) MarkCheckpointResolved(ctx context.Context, taskID, checkpointID string) error {
	if taskID == "" || checkpointID == "" {
		return fmt.Errorf("MarkCheckpointResolved: task_id + checkpoint_id required")
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE task_messages
		SET metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('resolved', true)
		WHERE id = $1
		  AND task_id = $2
		  AND message_kind = 'checkpoint'
	`, checkpointID, taskID); err != nil {
		return mapDBError(err)
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE tasks
		SET open_checkpoint_id = NULL,
		    updated_at         = now()
		WHERE id = $1
		  AND open_checkpoint_id = $2
	`, taskID, checkpointID); err != nil {
		return mapDBError(err)
	}
	return nil
}

// jsonOrNull returns the byte slice as-is if non-empty, otherwise
// nil so the column lands as SQL NULL rather than the literal
// JSON string "null".
func jsonOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	// Validate: a malformed JSON blob would let bad data through
	// because the column doesn't enforce JSON shape on insert; we
	// re-marshal through json.RawMessage as a defensive parse.
	var rm json.RawMessage = b
	if err := json.Unmarshal(b, &rm); err != nil {
		return nil
	}
	return string(b)
}
