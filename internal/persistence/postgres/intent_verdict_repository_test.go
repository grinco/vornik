package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestIntentVerdictRepository_Insert — the heuristic-tier row.
// LLM columns are NOT supplied by Insert; UpdateLLMRefinement
// fills them in async. Pin the column order so a schema rename
// trips the test rather than silently shifting positions.
func TestIntentVerdictRepository_Insert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewIntentVerdictRepository(db)

	created := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	v := &persistence.IntentVerdict{
		ID: "iv-1", ProjectID: "assistant",
		ToolName: "run_shell", ToolArgs: `{"cmd":"ls"}`,
		HeuristicRisk: "low", HeuristicConfidence: 0.4,
		HeuristicRecommendation: "approve",
		HeuristicReasoning:      "no rules matched",
		HeuristicLatencyMs:      1,
		FinalRisk:               "low",
		FinalRecommendation:     "approve",
		CreatedAt:               created,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO intent_verdicts")).
		WithArgs(v.ID, v.ProjectID, v.TaskID, v.ExecutionID, v.ChatID,
			v.ToolName, v.ToolArgs,
			v.HeuristicRisk, v.HeuristicConfidence, v.HeuristicRecommendation,
			v.HeuristicReasoning, v.HeuristicLatencyMs,
			v.FinalRisk, v.FinalRecommendation,
			v.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), v); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestIntentVerdictRepository_UpdateLLMRefinement — the async
// path: refiner finishes seconds after the heuristic row landed
// and updates the LLM columns. final_risk / final_rec must NOT
// move (the dispatcher already acted; mutating retroactively
// would corrupt the audit trail). refined_at gets stamped.
func TestIntentVerdictRepository_UpdateLLMRefinement(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewIntentVerdictRepository(db)

	risk := "high"
	conf := 0.92
	rec := "review"
	reason := "writes to /etc"
	latMs := int64(312)
	model := "gpt-oss-20b"
	v := &persistence.IntentVerdict{
		ID:                "iv-1",
		LLMRisk:           &risk,
		LLMConfidence:     &conf,
		LLMRecommendation: &rec,
		LLMReasoning:      &reason,
		LLMLatencyMs:      &latMs,
		LLMModel:          &model,
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE intent_verdicts SET")).
		WithArgs(&risk, &conf, &rec, &reason, &latMs, &model,
			sqlmock.AnyArg(), // refined_at = NOW()
			"iv-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpdateLLMRefinement(context.Background(), v); err != nil {
		t.Fatalf("UpdateLLMRefinement: %v", err)
	}
}

// TestIntentVerdictRepository_ListRecent_DecodesNullableFields —
// rows where the LLM refiner hasn't landed yet have NULL llm_*
// columns. The scan code must tolerate NULL without panicking
// and produce a row with nil pointer fields.
func TestIntentVerdictRepository_ListRecent_DecodesNullableFields(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewIntentVerdictRepository(db)

	created := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM intent_verdicts")).
		WithArgs("assistant", 25).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "execution_id", "chat_id",
			"tool_name", "tool_args",
			"heuristic_risk", "heuristic_conf", "heuristic_rec",
			"heuristic_reason", "heuristic_lat_ms",
			"llm_risk", "llm_conf", "llm_rec", "llm_reason", "llm_lat_ms", "llm_model",
			"final_risk", "final_rec",
			"created_at", "refined_at",
		}).AddRow("iv-1", "assistant", nil, nil, nil,
			"run_shell", "{}",
			"low", 0.4, "approve",
			"no rules", int64(1),
			nil, nil, nil, nil, nil, nil,
			"low", "approve",
			created, nil))

	got, err := repo.ListRecent(context.Background(), "assistant", 25)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	v := got[0]
	if v.ID != "iv-1" || v.HeuristicRisk != "low" {
		t.Errorf("heuristic fields wrong: %+v", v)
	}
	if v.LLMRisk != nil || v.LLMConfidence != nil || v.RefinedAt != nil {
		t.Errorf("LLM fields should be nil pre-refinement: %+v", v)
	}
}

// TestIntentVerdictRepository_ListRecent_DefaultsLimit — limit
// of 0 must fall back to the documented default (50), not silently
// return 0 rows.
func TestIntentVerdictRepository_ListRecent_DefaultsLimit(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewIntentVerdictRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("LIMIT $2")).
		WithArgs("assistant", 50). // default
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "execution_id", "chat_id",
			"tool_name", "tool_args",
			"heuristic_risk", "heuristic_conf", "heuristic_rec",
			"heuristic_reason", "heuristic_lat_ms",
			"llm_risk", "llm_conf", "llm_rec", "llm_reason", "llm_lat_ms", "llm_model",
			"final_risk", "final_rec",
			"created_at", "refined_at",
		}))

	if _, err := repo.ListRecent(context.Background(), "assistant", 0); err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
}
