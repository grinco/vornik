package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// IntentVerdictRepository persists two-tier intent-judge verdicts.
type IntentVerdictRepository struct {
	db DBTX
}

func NewIntentVerdictRepository(db DBTX) *IntentVerdictRepository {
	return &IntentVerdictRepository{db: db}
}

// Insert writes the initial heuristic-only verdict.
func (r *IntentVerdictRepository) Insert(ctx context.Context, v *persistence.IntentVerdict) error {
	if v == nil || v.ProjectID == "" {
		return fmt.Errorf("intent verdict: missing project id")
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO intent_verdicts (
			id, project_id, task_id, execution_id, chat_id,
			tool_name, tool_args,
			heuristic_risk, heuristic_conf, heuristic_rec,
			heuristic_reason, heuristic_lat_ms,
			final_risk, final_rec,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.ProjectID, v.TaskID, v.ExecutionID, v.ChatID,
		v.ToolName, v.ToolArgs,
		v.HeuristicRisk, v.HeuristicConfidence, v.HeuristicRecommendation,
		v.HeuristicReasoning, v.HeuristicLatencyMs,
		v.FinalRisk, v.FinalRecommendation,
		sqliteTime(v.CreatedAt),
	)
	return err
}

// UpdateLLMRefinement upserts the LLM-tier columns.
func (r *IntentVerdictRepository) UpdateLLMRefinement(ctx context.Context, v *persistence.IntentVerdict) error {
	if v == nil || v.ID == "" {
		return fmt.Errorf("intent verdict: missing id for refinement")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE intent_verdicts SET
			llm_risk    = ?,
			llm_conf    = ?,
			llm_rec     = ?,
			llm_reason  = ?,
			llm_lat_ms  = ?,
			llm_model   = ?,
			refined_at  = ?
		WHERE id = ?`,
		v.LLMRisk, v.LLMConfidence, v.LLMRecommendation,
		v.LLMReasoning, v.LLMLatencyMs, v.LLMModel,
		sqliteTime(time.Now().UTC()), v.ID,
	)
	return err
}

// ListRecent returns the most recent verdicts for a project.
func (r *IntentVerdictRepository) ListRecent(ctx context.Context, projectID string, limit int) ([]*persistence.IntentVerdict, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, task_id, execution_id, chat_id,
		       tool_name, tool_args,
		       heuristic_risk, heuristic_conf, heuristic_rec,
		       heuristic_reason, heuristic_lat_ms,
		       llm_risk, llm_conf, llm_rec, llm_reason, llm_lat_ms, llm_model,
		       final_risk, final_rec,
		       created_at, refined_at
		FROM intent_verdicts
		WHERE project_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.IntentVerdict
	for rows.Next() {
		var v persistence.IntentVerdict
		var (
			taskID, execID                       sql.NullString
			chatID                               sql.NullInt64
			llmRisk, llmRec, llmReason, llmModel sql.NullString
			llmConf                              sql.NullFloat64
			llmLatMs                             sql.NullInt64
			createdAt                            sqlTime
			refinedAt                            sqlNullTime
		)
		if err := rows.Scan(
			&v.ID, &v.ProjectID, &taskID, &execID, &chatID,
			&v.ToolName, &v.ToolArgs,
			&v.HeuristicRisk, &v.HeuristicConfidence, &v.HeuristicRecommendation,
			&v.HeuristicReasoning, &v.HeuristicLatencyMs,
			&llmRisk, &llmConf, &llmRec, &llmReason, &llmLatMs, &llmModel,
			&v.FinalRisk, &v.FinalRecommendation,
			&createdAt, &refinedAt,
		); err != nil {
			return nil, err
		}
		if taskID.Valid {
			v.TaskID = &taskID.String
		}
		if execID.Valid {
			v.ExecutionID = &execID.String
		}
		if chatID.Valid {
			v.ChatID = &chatID.Int64
		}
		if llmRisk.Valid {
			s := llmRisk.String
			v.LLMRisk = &s
		}
		if llmConf.Valid {
			f := llmConf.Float64
			v.LLMConfidence = &f
		}
		if llmRec.Valid {
			s := llmRec.String
			v.LLMRecommendation = &s
		}
		if llmReason.Valid {
			s := llmReason.String
			v.LLMReasoning = &s
		}
		if llmLatMs.Valid {
			i := llmLatMs.Int64
			v.LLMLatencyMs = &i
		}
		if llmModel.Valid {
			s := llmModel.String
			v.LLMModel = &s
		}
		v.CreatedAt = createdAt.Time
		if refinedAt.Valid {
			t := refinedAt.Time
			v.RefinedAt = &t
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}
