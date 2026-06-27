package postgres

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// WebhookEventRepository implements persistence.WebhookEventRepository using PostgreSQL.
type WebhookEventRepository struct {
	db DBTX
}

// NewWebhookEventRepository creates a new WebhookEventRepository.
func NewWebhookEventRepository(db DBTX) *WebhookEventRepository {
	return &WebhookEventRepository{db: db}
}

// Record inserts one webhook ingress audit event.
func (r *WebhookEventRepository) Record(ctx context.Context, event *persistence.WebhookEvent) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO webhook_events (
			id, project_id, source, event_id, payload_hash,
			status, task_id, error_code, error_message, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		event.ID, event.ProjectID, event.Source, event.EventID, event.PayloadHash,
		event.Status, event.TaskID, event.ErrorCode, event.ErrorMessage, event.CreatedAt,
	)
	return mapDBError(err)
}

// List returns webhook events matching the filter, newest first.
func (r *WebhookEventRepository) List(ctx context.Context, filter persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	query := `
		SELECT id, project_id, source, event_id, payload_hash,
		       status, task_id, error_code, error_message, created_at
		FROM webhook_events WHERE 1=1`
	args := make([]any, 0, 5)
	argPos := 1

	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.Source != nil {
		query += fmt.Sprintf(" AND source = $%d", argPos)
		args = append(args, *filter.Source)
		argPos++
	}
	if filter.Status != nil {
		query += fmt.Sprintf(" AND status = $%d", argPos)
		args = append(args, *filter.Status)
		argPos++
	}

	query += " ORDER BY created_at DESC"
	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argPos)
		args = append(args, filter.PageSize)
		argPos++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argPos)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var events []*persistence.WebhookEvent
	for rows.Next() {
		var event persistence.WebhookEvent
		if err := rows.Scan(
			&event.ID, &event.ProjectID, &event.Source, &event.EventID, &event.PayloadHash,
			&event.Status, &event.TaskID, &event.ErrorCode, &event.ErrorMessage, &event.CreatedAt,
		); err != nil {
			return nil, err
		}
		events = append(events, &event)
	}
	return events, rows.Err()
}
