package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// ExecutionQualityScore is the durable, one-row-per-execution quality verdict.
// Score is nil only for status=not_applicable.
type ExecutionQualityScore struct {
	ProjectID        string          `json:"project_id"`
	TaskID           string          `json:"task_id"`
	ExecutionID      string          `json:"execution_id"`
	WorkflowID       string          `json:"workflow_id"`
	WorkflowRevision string          `json:"workflow_revision"`
	ScorerVersion    string          `json:"scorer_version"`
	ScoringPolicySHA string          `json:"scoring_policy_sha"`
	Kind             string          `json:"kind"`
	Status           string          `json:"status"`
	Score            *float64        `json:"score,omitempty"`
	PassedCaseCount  int             `json:"passed_case_count"`
	PinnedCaseCount  int             `json:"pinned_case_count"`
	Diagnostic       string          `json:"diagnostic,omitempty"`
	CaseEvidence     json.RawMessage `json:"case_evidence"`
	RecordedAt       time.Time       `json:"recorded_at"`
}

// ExecutionQualityScoreFilter scopes and paginates operator score queries.
type ExecutionQualityScoreFilter struct {
	ProjectIDs  []string
	TaskID      string
	ExecutionID string
	WorkflowID  string
	Statuses    []string
	Since       *time.Time
	MaxScore    *float64
	PageSize    int
	Offset      int
}

// ExecutionQualityPendingStats describes terminal executions awaiting publication.
type ExecutionQualityPendingStats struct {
	Count    int64      `json:"count"`
	OldestAt *time.Time `json:"oldest_at,omitempty"`
}

// ExecutionQualityScoreRepository persists scores and finds publication gaps.
type ExecutionQualityScoreRepository interface {
	Upsert(ctx context.Context, score *ExecutionQualityScore) error
	GetByExecution(ctx context.Context, executionID string) (*ExecutionQualityScore, error)
	List(ctx context.Context, filter ExecutionQualityScoreFilter) ([]*ExecutionQualityScore, error)
	ListPendingTerminal(ctx context.Context, limit int) ([]*Execution, error)
	PendingTerminalStats(ctx context.Context, projectIDs []string) (ExecutionQualityPendingStats, error)
}

// ValidateExecutionQualityScore enforces the durable score-row invariants.
func ValidateExecutionQualityScore(s *ExecutionQualityScore) error {
	if s == nil {
		return fmt.Errorf("execution quality score is nil")
	}
	if s.ProjectID == "" || s.TaskID == "" || s.ExecutionID == "" || s.WorkflowID == "" {
		return fmt.Errorf("execution quality score requires project_id, task_id, execution_id, and workflow_id")
	}
	switch s.Status {
	case "scored", "missing_contract", "invalid_evidence":
		if s.Score == nil {
			return fmt.Errorf("execution quality score status %q requires a numeric score", s.Status)
		}
	case "not_applicable":
		if s.Score != nil {
			return fmt.Errorf("not_applicable execution quality score must not be numeric")
		}
	default:
		return fmt.Errorf("unknown execution quality score status %q", s.Status)
	}
	if s.Score != nil && (math.IsNaN(*s.Score) || math.IsInf(*s.Score, 0) || *s.Score < 0 || *s.Score > 1) {
		return fmt.Errorf("execution quality score must be finite and within [0,1]")
	}
	if s.PassedCaseCount < 0 || s.PinnedCaseCount < 0 || s.PassedCaseCount > s.PinnedCaseCount {
		return fmt.Errorf("invalid execution quality case counts %d/%d", s.PassedCaseCount, s.PinnedCaseCount)
	}
	return nil
}
