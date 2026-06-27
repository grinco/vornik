package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowHealingTrialRepository implements
// persistence.WorkflowHealingTrialRepository over postgres. Backs the
// Self-Healing Workflow Genome v1 trial ledger (migration 88). Each
// trial belongs to one candidate; the summary/scorecard blobs are
// JSONB the trial runner writes verbatim.
type WorkflowHealingTrialRepository struct {
	db DBTX
}

// NewWorkflowHealingTrialRepository constructs a repo over db.
func NewWorkflowHealingTrialRepository(db DBTX) *WorkflowHealingTrialRepository {
	return &WorkflowHealingTrialRepository{db: db}
}

var _ persistence.WorkflowHealingTrialRepository = (*WorkflowHealingTrialRepository)(nil)

const healingTrialColumns = `id, candidate_id, mode,
    evidence_execution_ids::text, baseline_summary::text,
    candidate_summary::text, scorecard::text,
    verdict, started_at, finished_at`

func (r *WorkflowHealingTrialRepository) Insert(ctx context.Context, tr *persistence.HealingTrial) error {
	if tr == nil {
		return fmt.Errorf("healing_trials: nil trial")
	}
	if tr.CandidateID == "" {
		return fmt.Errorf("healing_trials: candidate_id required")
	}
	if tr.Mode == "" {
		return fmt.Errorf("healing_trials: mode required")
	}
	if tr.ID == "" {
		tr.ID = persistence.GenerateID("wht")
	}
	if tr.StartedAt.IsZero() {
		tr.StartedAt = time.Now().UTC()
	}
	if tr.Verdict == "" {
		tr.Verdict = persistence.HealingTrialPending
	}
	evidence, err := marshalJSONArray(tr.EvidenceExecutionIDs)
	if err != nil {
		return fmt.Errorf("healing_trials: marshal evidence: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO workflow_healing_trials (
    id, candidate_id, mode, evidence_execution_ids,
    baseline_summary, candidate_summary, scorecard,
    verdict, started_at
) VALUES (
    $1, $2, $3, $4::jsonb,
    $5::jsonb, $6::jsonb, $7::jsonb,
    $8, $9
)
`,
		tr.ID, tr.CandidateID, string(tr.Mode), evidence,
		jsonObjectOrDefault(tr.BaselineSummary),
		jsonObjectOrDefault(tr.CandidateSummary),
		jsonObjectOrDefault(tr.Scorecard),
		string(tr.Verdict), tr.StartedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("healing_trials: insert: %w", err)
	}
	return nil
}

func (r *WorkflowHealingTrialRepository) Get(ctx context.Context, id string) (*persistence.HealingTrial, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+healingTrialColumns+` FROM workflow_healing_trials WHERE id = $1`, id)
	tr, err := scanHealingTrial(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("healing_trials: get: %w", err)
	}
	return tr, nil
}

func (r *WorkflowHealingTrialRepository) ListByCandidate(ctx context.Context, candidateID string) ([]*persistence.HealingTrial, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+healingTrialColumns+` FROM workflow_healing_trials
         WHERE candidate_id = $1 ORDER BY started_at DESC`,
		candidateID,
	)
	if err != nil {
		return nil, fmt.Errorf("healing_trials: list_by_candidate: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.HealingTrial
	for rows.Next() {
		tr, err := scanHealingTrial(rows)
		if err != nil {
			return nil, fmt.Errorf("healing_trials: list scan: %w", err)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

func (r *WorkflowHealingTrialRepository) Finish(ctx context.Context, id string, verdict persistence.HealingTrialVerdict, baselineSummary, candidateSummary, scorecard string) error {
	if verdict == "" {
		return fmt.Errorf("healing_trials: finish: verdict required")
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE workflow_healing_trials
SET verdict = $2,
    baseline_summary = $3::jsonb,
    candidate_summary = $4::jsonb,
    scorecard = $5::jsonb,
    finished_at = NOW()
WHERE id = $1
`,
		id, string(verdict),
		jsonObjectOrDefault(baselineSummary),
		jsonObjectOrDefault(candidateSummary),
		jsonObjectOrDefault(scorecard),
	)
	if err != nil {
		return fmt.Errorf("healing_trials: finish: %w", err)
	}
	return rowsAffectedNotFound(res)
}

// marshalJSONArray renders a string slice as a JSON array. A nil
// slice becomes "[]" (not "null") so the JSONB column always holds a
// valid array literal.
func marshalJSONArray(ss []string) (string, error) {
	if len(ss) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(ss)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// jsonObjectOrDefault returns "{}" for an empty blob so the JSONB
// column is never asked to parse an empty string (which postgres
// rejects).
func jsonObjectOrDefault(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

func scanHealingTrial(scanner interface {
	Scan(dest ...interface{}) error
}) (*persistence.HealingTrial, error) {
	var (
		tr         persistence.HealingTrial
		mode       string
		verdict    string
		evidence   string
		finishedAt sql.NullTime
	)
	if err := scanner.Scan(
		&tr.ID, &tr.CandidateID, &mode,
		&evidence, &tr.BaselineSummary, &tr.CandidateSummary, &tr.Scorecard,
		&verdict, &tr.StartedAt, &finishedAt,
	); err != nil {
		return nil, err
	}
	tr.Mode = persistence.HealingTrialMode(mode)
	tr.Verdict = persistence.HealingTrialVerdict(verdict)
	if evidence != "" {
		var ids []string
		if err := json.Unmarshal([]byte(evidence), &ids); err != nil {
			return nil, fmt.Errorf("healing_trials: unmarshal evidence: %w", err)
		}
		tr.EvidenceExecutionIDs = ids
	}
	if finishedAt.Valid {
		ts := finishedAt.Time
		tr.FinishedAt = &ts
	}
	return &tr, nil
}
