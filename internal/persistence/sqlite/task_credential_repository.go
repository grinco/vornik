package sqlite

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskCredentialRepository is the SQLite parity for the postgres
// TaskCredentialRepository. Same shape; created_at stored as TEXT via
// sqliteTime. Backs credential carryover (see
// https://docs.vornik.io).
type TaskCredentialRepository struct {
	db DBTX
}

// NewTaskCredentialRepository constructs the repo over db.
func NewTaskCredentialRepository(db DBTX) *TaskCredentialRepository {
	return &TaskCredentialRepository{db: db}
}

// Upsert records a captured credential, overwriting the value in place on a
// conflict of (task_id, execution_id, tool, artifact_url). ID and CreatedAt
// default when left zero.
func (r *TaskCredentialRepository) Upsert(ctx context.Context, cred *persistence.TaskCredential) error {
	if cred.ID == "" {
		cred.ID = persistence.GenerateID("taskcred")
	}
	if cred.CreatedAt.IsZero() {
		cred.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_credentials (
			id, task_id, execution_id, tool, label, value, artifact_url, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id, execution_id, tool, artifact_url)
		DO UPDATE SET label = excluded.label, value = excluded.value, created_at = excluded.created_at`,
		cred.ID, cred.TaskID, cred.ExecutionID, cred.Tool, cred.Label, cred.Value, cred.ArtifactURL, sqliteTime(cred.CreatedAt),
	)
	return err
}

// ListByTaskLatestExecution returns the credentials captured by the task's
// most-recently-capturing execution only. Empty when none.
func (r *TaskCredentialRepository) ListByTaskLatestExecution(ctx context.Context, taskID string) ([]*persistence.TaskCredential, error) {
	if taskID == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, task_id, execution_id, tool, label, value, artifact_url, created_at
		FROM task_credentials
		WHERE task_id = ?
		  AND execution_id = (
		      SELECT execution_id FROM task_credentials
		      WHERE task_id = ? ORDER BY created_at DESC LIMIT 1
		  )
		ORDER BY created_at`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TaskCredential
	for rows.Next() {
		var (
			c         persistence.TaskCredential
			createdAt string
		)
		if err := rows.Scan(&c.ID, &c.TaskID, &c.ExecutionID, &c.Tool, &c.Label, &c.Value, &c.ArtifactURL, &createdAt); err != nil {
			return nil, err
		}
		if t, perr := parseSqliteTime(createdAt); perr == nil {
			c.CreatedAt = t
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
