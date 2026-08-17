package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionQualityScoreRepository is the SQLite execution-score store.
type ExecutionQualityScoreRepository struct{ db DBTX }

// NewExecutionQualityScoreRepository constructs a SQLite score repository.
func NewExecutionQualityScoreRepository(db DBTX) *ExecutionQualityScoreRepository {
	return &ExecutionQualityScoreRepository{db: db}
}

// Upsert records the score only when its identity matches the execution ledger.
func (r *ExecutionQualityScoreRepository) Upsert(ctx context.Context, s *persistence.ExecutionQualityScore) error {
	if err := persistence.ValidateExecutionQualityScore(s); err != nil {
		return err
	}
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}
	evidence := s.CaseEvidence
	if len(evidence) == 0 {
		evidence = json.RawMessage(`[]`)
	}
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_quality_scores (
			project_id, task_id, execution_id, workflow_id, workflow_revision,
			scorer_version, scoring_policy_sha, kind, status, score,
			passed_case_count, pinned_case_count, diagnostic, case_evidence, recorded_at)
		SELECT e.project_id, e.task_id, e.id, e.workflow_id, e.workflow_revision,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		FROM executions e
		WHERE e.id = ? AND e.project_id = ? AND e.task_id = ? AND e.workflow_id = ?
		ON CONFLICT(execution_id) DO UPDATE SET
			project_id = excluded.project_id,
			task_id = excluded.task_id,
			workflow_id = excluded.workflow_id,
			workflow_revision = excluded.workflow_revision,
			scorer_version = excluded.scorer_version,
			scoring_policy_sha = excluded.scoring_policy_sha,
			kind = excluded.kind,
			status = excluded.status,
			score = excluded.score,
			passed_case_count = excluded.passed_case_count,
			pinned_case_count = excluded.pinned_case_count,
			diagnostic = excluded.diagnostic,
			case_evidence = excluded.case_evidence,
			recorded_at = excluded.recorded_at`,
		s.ScorerVersion, s.ScoringPolicySHA, s.Kind, s.Status, s.Score,
		s.PassedCaseCount, s.PinnedCaseCount, s.Diagnostic, string(evidence), sqliteTime(s.RecordedAt),
		s.ExecutionID, s.ProjectID, s.TaskID, s.WorkflowID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("execution quality score identity does not match execution %q", s.ExecutionID)
	}
	return nil
}

// GetByExecution returns the one score row for an execution.
func (r *ExecutionQualityScoreRepository) GetByExecution(ctx context.Context, executionID string) (*persistence.ExecutionQualityScore, error) {
	if executionID == "" {
		return nil, persistence.ErrNotFound
	}
	return scanExecutionQualityScore(r.db.QueryRowContext(ctx, executionQualityScoreSelect+` WHERE execution_id = ?`, executionID))
}

// List returns project-scoped score rows matching the supplied filter.
func (r *ExecutionQualityScoreRepository) List(ctx context.Context, f persistence.ExecutionQualityScoreFilter) ([]*persistence.ExecutionQualityScore, error) {
	var b strings.Builder
	b.WriteString(executionQualityScoreSelect + ` WHERE 1=1`)
	args := make([]any, 0, 12)
	if len(f.ProjectIDs) > 0 {
		b.WriteString(` AND project_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.ProjectIDs)), ",") + `)`)
		for _, id := range f.ProjectIDs {
			args = append(args, id)
		}
	}
	if f.TaskID != "" {
		b.WriteString(` AND task_id = ?`)
		args = append(args, f.TaskID)
	}
	if f.ExecutionID != "" {
		b.WriteString(` AND execution_id = ?`)
		args = append(args, f.ExecutionID)
	}
	if f.WorkflowID != "" {
		b.WriteString(` AND workflow_id = ?`)
		args = append(args, f.WorkflowID)
	}
	if len(f.Statuses) > 0 {
		b.WriteString(` AND status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Statuses)), ",") + `)`)
		for _, status := range f.Statuses {
			args = append(args, status)
		}
	}
	if f.Since != nil {
		b.WriteString(` AND recorded_at >= ?`)
		args = append(args, sqliteTime(*f.Since))
	}
	if f.MaxScore != nil {
		b.WriteString(` AND score <= ?`)
		args = append(args, *f.MaxScore)
	}
	limit := f.PageSize
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	b.WriteString(` ORDER BY recorded_at DESC, execution_id DESC LIMIT ? OFFSET ?`)
	args = append(args, limit, max(f.Offset, 0))
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ExecutionQualityScore
	for rows.Next() {
		s, err := scanExecutionQualityScore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListPendingTerminal returns terminal executions lacking a score row.
func (r *ExecutionQualityScoreRepository) ListPendingTerminal(ctx context.Context, limit int) ([]*persistence.Execution, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.task_id, e.project_id, e.workflow_id, e.workflow_revision,
		       e.workflow_snapshot, e.status, e.current_step_id, e.completed_steps,
		       e.state_snapshot, e.result, e.error_message, e.error_code,
		       e.started_at, e.completed_at, e.created_at, e.updated_at,
		       e.parent_execution_id, e.forked_from_step_id, e.forked_prompt_override
		FROM executions e
		LEFT JOIN execution_quality_scores s ON s.execution_id = e.id
		WHERE e.status IN ('COMPLETED','FAILED','CANCELLED') AND s.execution_id IS NULL
		ORDER BY e.created_at ASC, e.id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.Execution
	for rows.Next() {
		e, err := scanSqliteExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PendingTerminalStats summarizes the project-scoped publication backlog.
func (r *ExecutionQualityScoreRepository) PendingTerminalStats(ctx context.Context, projectIDs []string) (persistence.ExecutionQualityPendingStats, error) {
	query := `
		SELECT COUNT(*), MIN(COALESCE(e.completed_at, e.updated_at, e.created_at))
		FROM executions e
		LEFT JOIN execution_quality_scores s ON s.execution_id = e.id
		WHERE e.status IN ('COMPLETED','FAILED','CANCELLED') AND s.execution_id IS NULL`
	args := make([]any, 0, len(projectIDs))
	if len(projectIDs) > 0 {
		query += ` AND e.project_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(projectIDs)), ",") + `)`
		for _, id := range projectIDs {
			args = append(args, id)
		}
	}
	var (
		stats  persistence.ExecutionQualityPendingStats
		oldest sqlNullTime
	)
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&stats.Count, &oldest); err != nil {
		return persistence.ExecutionQualityPendingStats{}, err
	}
	if oldest.Valid {
		t := oldest.Time
		stats.OldestAt = &t
	}
	return stats, nil
}

const executionQualityScoreSelect = `
SELECT project_id, task_id, execution_id, workflow_id, workflow_revision,
       scorer_version, scoring_policy_sha, kind, status, score,
       passed_case_count, pinned_case_count, diagnostic, case_evidence, recorded_at
FROM execution_quality_scores`

func scanExecutionQualityScore(scanner interface{ Scan(dest ...any) error }) (*persistence.ExecutionQualityScore, error) {
	var (
		s        persistence.ExecutionQualityScore
		score    sql.NullFloat64
		evidence string
		recorded sqlTime
	)
	err := scanner.Scan(&s.ProjectID, &s.TaskID, &s.ExecutionID, &s.WorkflowID, &s.WorkflowRevision,
		&s.ScorerVersion, &s.ScoringPolicySHA, &s.Kind, &s.Status, &score,
		&s.PassedCaseCount, &s.PinnedCaseCount, &s.Diagnostic, &evidence, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if score.Valid {
		s.Score = &score.Float64
	}
	s.CaseEvidence = json.RawMessage(evidence)
	s.RecordedAt = recorded.Time
	return &s, nil
}
