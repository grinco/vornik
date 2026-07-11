package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SecretRedactionAuditRepository implements
// persistence.SecretRedactionAuditRepository against PostgreSQL. It backs
// the secret-leak Phase 3 operator surface: the task-detail "🔒 N secrets
// redacted" badge (CountByTask) and the `vornikctl secrets scan-history`
// retro-scan (Record with source="scan"). Runtime sinks Record with
// source="live".
type SecretRedactionAuditRepository struct {
	db DBTX
}

// NewSecretRedactionAuditRepository constructs the repo over db.
func NewSecretRedactionAuditRepository(db DBTX) *SecretRedactionAuditRepository {
	return &SecretRedactionAuditRepository{db: db}
}

// Record inserts a batch of redaction events in one multi-row INSERT.
// Empty input is a no-op. IDs and CreatedAt default when left zero.
func (r *SecretRedactionAuditRepository) Record(ctx context.Context, events []persistence.SecretRedactionEvent) error {
	if len(events) == 0 {
		return nil
	}
	var (
		placeholders = make([]string, 0, len(events))
		args         = make([]any, 0, len(events)*9)
	)
	for i := range events {
		e := &events[i]
		if e.ID == "" {
			e.ID = persistence.GenerateID("secred")
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now().UTC()
		}
		if e.Source == "" {
			e.Source = "live"
		}
		base := i * 9
		placeholders = append(placeholders, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9))
		args = append(args,
			e.ID, e.ProjectID, textOrNull(e.TaskID), textOrNull(e.ExecutionID),
			e.Checkpoint, e.FindingType, e.Count, e.Source, e.CreatedAt,
		)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO secret_redaction_audit (
			id, project_id, task_id, execution_id,
			checkpoint, finding_type, count, source, created_at
		) VALUES `+strings.Join(placeholders, ","), args...)
	return mapDBError(err)
}

// CountByTask sums redaction counts per finding type for a task.
func (r *SecretRedactionAuditRepository) CountByTask(ctx context.Context, taskID string) (map[string]int, int, error) {
	byType := make(map[string]int)
	if taskID == "" {
		return byType, 0, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT finding_type, COALESCE(SUM(count),0)::int
		FROM secret_redaction_audit
		WHERE task_id = $1
		GROUP BY finding_type`, taskID)
	if err != nil {
		return nil, 0, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	total := 0
	for rows.Next() {
		var ft string
		var n int
		if err := rows.Scan(&ft, &n); err != nil {
			return nil, 0, err
		}
		byType[ft] = n
		total += n
	}
	return byType, total, rows.Err()
}
