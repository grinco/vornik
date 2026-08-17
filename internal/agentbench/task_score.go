package agentbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"vornik.io/vornik/internal/quality"
)

// PinnedCaseValidationMetric is the pre-registration name for graded task quality.
const PinnedCaseValidationMetric = "pinned_case_validation_score"

// TaskScore is one self-contained graded verdict for one submitted task
// repeat. ExecutionIDs retain the ledger provenance without changing the unit
// of analysis from task to execution.
type TaskScore struct {
	TaskID          string                           `json:"taskId"`
	Repeat          int                              `json:"repeat"`
	Kind            quality.ScoreKind                `json:"kind"`
	Status          quality.ScoreStatus              `json:"status"`
	Score           float64                          `json:"score"`
	PassedCaseCount int                              `json:"passedCaseCount"`
	PinnedCaseCount int                              `json:"pinnedCaseCount"`
	Diagnostic      string                           `json:"diagnostic,omitempty"`
	CaseEvidence    []quality.NormalizedCaseEvidence `json:"caseEvidence,omitempty"`
	ExecutionIDs    []string                         `json:"executionIds"`
}

// ScoreTask adapts the shared production scorer to the benchmark's task/repeat
// unit. The root execution snapshot is selected by the runner; all execution
// IDs remain attached for auditability.
func ScoreTask(taskID string, repeat int, policy *quality.ScoringPolicy, executionIDs []string, rootSnapshot []byte) (TaskScore, error) {
	if policy == nil {
		return TaskScore{}, fmt.Errorf("task %q has no scoring policy", taskID)
	}
	if repeat < 1 {
		return TaskScore{}, fmt.Errorf("task %q has invalid repeat %d", taskID, repeat)
	}
	if len(executionIDs) == 0 {
		return TaskScore{}, fmt.Errorf("task %q produced no execution to score", taskID)
	}
	verdict, err := quality.ScoreExecution(policy, rootSnapshot)
	if err != nil {
		return TaskScore{}, fmt.Errorf("score task %s repeat %d: %w", taskID, repeat, err)
	}
	if verdict.Score == nil {
		return TaskScore{}, fmt.Errorf("task %q scoring policy produced no numeric score", taskID)
	}
	return TaskScore{
		TaskID: taskID, Repeat: repeat, Kind: verdict.Kind, Status: verdict.Status,
		Score: *verdict.Score, PassedCaseCount: verdict.PassedCaseCount,
		PinnedCaseCount: verdict.PinnedCaseCount, Diagnostic: verdict.Diagnostic,
		CaseEvidence: verdict.CaseEvidence,
		ExecutionIDs: append([]string(nil), executionIDs...),
	}, nil
}

func snapshotHasStepResult(snapshot []byte, stepID string) (bool, error) {
	if len(snapshot) == 0 {
		return false, nil
	}
	var state struct {
		StepResults map[string]json.RawMessage `json:"stepResults"`
	}
	if err := json.Unmarshal(snapshot, &state); err != nil {
		return false, err
	}
	_, ok := state.StepResults[stepID]
	return ok, nil
}

// ScoringPolicyDigest identifies every task-level scoring contract without
// changing the task-set digest (which identifies what the agents ran).
func ScoringPolicyDigest(tasks []TaskSpec) string {
	type entry struct {
		TaskID string                `json:"taskId"`
		Policy quality.ScoringPolicy `json:"policy"`
	}
	entries := make([]entry, 0, len(tasks))
	for _, task := range tasks {
		if task.Scoring != nil {
			entries = append(entries, entry{TaskID: task.ID, Policy: *task.Scoring})
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TaskID < entries[j].TaskID })
	b, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TaskScoreComparison summarizes the paired, repeat-averaged task differences.
type TaskScoreComparison struct {
	MeanDelta float64
	Magnitude float64
	SigmaD    float64
	PairCount int
}

// CompareTaskScores averages repeats inside a task before producing the paired
// task difference vector. b-a is the signed direction.
func CompareTaskScores(a, b []TaskScore, kind quality.ScoreKind) (TaskScoreComparison, error) {
	left, err := taskRepeatScores(a, kind)
	if err != nil {
		return TaskScoreComparison{}, fmt.Errorf("first journal: %w", err)
	}
	right, err := taskRepeatScores(b, kind)
	if err != nil {
		return TaskScoreComparison{}, fmt.Errorf("second journal: %w", err)
	}
	if len(left) != len(right) {
		return TaskScoreComparison{}, fmt.Errorf("task-score pair sets differ: %d vs %d tasks", len(left), len(right))
	}
	tasks := make([]string, 0, len(left))
	for taskID := range left {
		if _, ok := right[taskID]; !ok {
			return TaskScoreComparison{}, fmt.Errorf("task-score pair %q is missing from the second journal", taskID)
		}
		tasks = append(tasks, taskID)
	}
	if len(tasks) == 0 {
		return TaskScoreComparison{}, fmt.Errorf("no %s task scores to compare", kind)
	}
	sort.Strings(tasks)
	diffs := make([]float64, 0, len(tasks))
	for _, taskID := range tasks {
		la, lb := left[taskID], right[taskID]
		if len(la) != len(lb) {
			return TaskScoreComparison{}, fmt.Errorf("task %q has %d vs %d repeats", taskID, len(la), len(lb))
		}
		for repeat := range la {
			if _, ok := lb[repeat]; !ok {
				return TaskScoreComparison{}, fmt.Errorf("task %q repeat %d is missing from the second journal", taskID, repeat)
			}
		}
		diffs = append(diffs, meanScore(lb)-meanScore(la))
	}
	mean := meanFloat(diffs)
	return TaskScoreComparison{
		MeanDelta: mean, Magnitude: math.Abs(mean), SigmaD: sampleStdDev(diffs, mean), PairCount: len(diffs),
	}, nil
}

func taskRepeatScores(scores []TaskScore, kind quality.ScoreKind) (map[string]map[int]float64, error) {
	out := map[string]map[int]float64{}
	for _, score := range scores {
		if score.Kind != kind {
			continue
		}
		if score.TaskID == "" || score.Repeat < 1 || math.IsNaN(score.Score) || math.IsInf(score.Score, 0) || score.Score < 0 || score.Score > 1 {
			return nil, fmt.Errorf("invalid task score %#v", score)
		}
		if out[score.TaskID] == nil {
			out[score.TaskID] = map[int]float64{}
		}
		if _, exists := out[score.TaskID][score.Repeat]; exists {
			return nil, fmt.Errorf("duplicate task/repeat pair %s/%d", score.TaskID, score.Repeat)
		}
		out[score.TaskID][score.Repeat] = score.Score
	}
	return out, nil
}

func meanScore(scores map[int]float64) float64 {
	values := make([]float64, 0, len(scores))
	for _, score := range scores {
		values = append(values, score)
	}
	return meanFloat(values)
}

func meanFloat(values []float64) float64 {
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func sampleStdDev(values []float64, mean float64) float64 {
	if len(values) < 2 {
		return 0
	}
	var sumSquares float64
	for _, value := range values {
		d := value - mean
		sumSquares += d * d
	}
	return math.Sqrt(sumSquares / float64(len(values)-1))
}
