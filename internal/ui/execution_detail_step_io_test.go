package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// A step row links to its stored boundary files exactly when the outcome row
// carries their hashes (step-I/O persistence design §5): input and result are
// independent, and a row with neither shows neither link.
func TestExecutionDetail_StepRowLinksToInputAndResult(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	rows := []*persistence.ExecutionStepOutcome{
		{ID: "outc_a", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1", StepID: "step-both", Role: "lead", Outcome: "ok", RecordedAt: t0,
			PromptHashes: persistence.StepPromptHashes{Input: "i1", Result: "r1"}},
		{ID: "outc_b", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1", StepID: "step-input-only", Role: "coder", Outcome: "failed", RecordedAt: t0.Add(time.Second),
			PromptHashes: persistence.StepPromptHashes{Input: "i2"}},
		{ID: "outc_c", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1", StepID: "step-neither", Role: "gate", Outcome: "ok", RecordedAt: t0.Add(2 * time.Second)},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: t0}
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusCompleted,
		CompletedSteps: []string{"step-both", "step-input-only", "step-neither"}}
	s := NewServer(
		WithTaskRepository(&mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == task.ID {
				return task, nil
			}
			return nil, persistence.ErrNotFound
		}}),
		WithExecutionRepository(&mocks.MockExecutionRepository{GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id == exec.ID {
				return exec, nil
			}
			return nil, persistence.ErrNotFound
		}}),
		WithStepOutcomeRepository(&shuffleOutcomeRepo{rows: rows}),
	)
	rec := httptest.NewRecorder()
	s.ExecutionDetail(rec, httptest.NewRequest(http.MethodGet, "/executions/e1", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	for _, want := range []string{"/api/v1/executions/e1/steps/step-both/input", "/api/v1/executions/e1/steps/step-both/result", "/api/v1/executions/e1/steps/step-input-only/input"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing link %s", want)
		}
	}
	for _, absent := range []string{"/api/v1/executions/e1/steps/step-input-only/result", "/api/v1/executions/e1/steps/step-neither/input", "/api/v1/executions/e1/steps/step-neither/result"} {
		if strings.Contains(body, absent) {
			t.Errorf("unexpected link %s", absent)
		}
	}
}
