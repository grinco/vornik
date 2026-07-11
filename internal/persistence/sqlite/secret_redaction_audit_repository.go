package sqlite

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SecretRedactionAuditRepository is the SQLite parity for the postgres
// SecretRedactionAuditRepository. Same shape; timestamps stored as TEXT
// via sqliteTime, empty task/execution IDs collapse to NULL.
type SecretRedactionAuditRepository struct {
	db DBTX
}

// NewSecretRedactionAuditRepository constructs the repo over db.
func NewSecretRedactionAuditRepository(db DBTX) *SecretRedactionAuditRepository {
	return &SecretRedactionAuditRepository{db: db}
}

// Record inserts a batch of redaction events. Empty input is a no-op;
// IDs, CreatedAt and Source default when left zero.
func (r *SecretRedactionAuditRepository) Record(ctx context.Context, events []persistence.SecretRedactionEvent) error {
	if len(events) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(events))
	args := make([]any, 0, len(events)*9)
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
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			e.ID, e.ProjectID, nullableString(e.TaskID), nullableString(e.ExecutionID),
			e.Checkpoint, e.FindingType, e.Count, e.Source, sqliteTime(e.CreatedAt),
		)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO secret_redaction_audit (
			id, project_id, task_id, execution_id,
			checkpoint, finding_type, count, source, created_at
		) VALUES `+strings.Join(placeholders, ","), args...)
	return err
}

// CountByTask sums redaction counts per finding type for a task.
func (r *SecretRedactionAuditRepository) CountByTask(ctx context.Context, taskID string) (map[string]int, int, error) {
	byType := make(map[string]int)
	if taskID == "" {
		return byType, 0, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT finding_type, COALESCE(SUM(count),0)
		FROM secret_redaction_audit
		WHERE task_id = ?
		GROUP BY finding_type`, taskID)
	if err != nil {
		return nil, 0, err
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
