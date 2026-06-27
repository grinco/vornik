package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// WebhookEventRepository is the SQLite persistence.WebhookEventRepository.
type WebhookEventRepository struct {
	db DBTX
}

func NewWebhookEventRepository(db DBTX) *WebhookEventRepository {
	return &WebhookEventRepository{db: db}
}

// Record inserts one webhook ingress audit row.
func (r *WebhookEventRepository) Record(ctx context.Context, e *persistence.WebhookEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO webhook_events (
			id, project_id, source, event_id, payload_hash,
			status, task_id, error_code, error_message, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ProjectID, e.Source, e.EventID, e.PayloadHash,
		e.Status, e.TaskID, e.ErrorCode, e.ErrorMessage, sqliteTime(e.CreatedAt),
	)
	return err
}

// List returns webhook events matching filter, newest-first.
func (r *WebhookEventRepository) List(ctx context.Context, filter persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, source, event_id, payload_hash,
		       status, task_id, error_code, error_message, created_at
		FROM webhook_events WHERE 1=1`)
	args := make([]any, 0, 5)
	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Source != nil {
		b.WriteString(" AND source = ?")
		args = append(args, *filter.Source)
	}
	if filter.Status != nil {
		b.WriteString(" AND status = ?")
		args = append(args, *filter.Status)
	}
	b.WriteString(" ORDER BY created_at DESC")
	if filter.PageSize > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, filter.PageSize)
	}
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.WebhookEvent
	for rows.Next() {
		var (
			e         persistence.WebhookEvent
			taskID    sql.NullString
			createdAt sqlTime
		)
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.Source, &e.EventID, &e.PayloadHash,
			&e.Status, &taskID, &e.ErrorCode, &e.ErrorMessage, &createdAt,
		); err != nil {
			return nil, err
		}
		if taskID.Valid {
			e.TaskID = &taskID.String
		}
		e.CreatedAt = createdAt.Time
		out = append(out, &e)
	}
	return out, rows.Err()
}
