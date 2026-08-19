package quality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionScorerVersion identifies the production and benchmark scorer
// implementation. It is persisted independently from the benchmark harness
// version so old rows remain interpretable after unrelated harness changes.
const ExecutionScorerVersion = "1"

// ExecutionScorePublisher materializes the score read model from terminal executions.
// StepOutcomeReader is the narrow surface contract_satisfaction needs.
//
// A state snapshot cannot stand in for it: the snapshot records only steps that
// SUCCEEDED, so it cannot distinguish a step that ran and failed from one never
// reached — and for this metric those are opposite facts (a broken promise
// versus a road not taken). Optional: nil simply means contract_satisfaction
// cannot be scored, and it reports that rather than guessing.
type StepOutcomeReader interface {
	OutcomesByExecution(ctx context.Context, executionID string) (map[string]string, error)
}

type ExecutionScorePublisher struct {
	repo     persistence.ExecutionQualityScoreRepository
	outcomes StepOutcomeReader
	now      func() time.Time
	metrics  *ExecutionScoreMetrics
}

// ReconcileResult reports one bounded publication pass.
type ReconcileResult struct {
	Selected  int
	Published int
	Failed    int
}

// NewExecutionScorePublisher constructs a terminal-execution score publisher.
func NewExecutionScorePublisher(repo persistence.ExecutionQualityScoreRepository, now func() time.Time, metrics ...*ExecutionScoreMetrics) *ExecutionScorePublisher {
	if now == nil {
		now = time.Now
	}
	var m *ExecutionScoreMetrics
	if len(metrics) > 0 {
		m = metrics[0]
	}
	return &ExecutionScorePublisher{repo: repo, now: now, metrics: m}
}

// Publish scores one already-terminal execution. It never updates execution or
// task lifecycle state; persistence identity is derived from the execution row
// again by the repository's INSERT ... SELECT invariant.
func (p *ExecutionScorePublisher) Publish(ctx context.Context, exec *persistence.Execution) error {
	if p == nil || p.repo == nil {
		return fmt.Errorf("execution score publisher is not configured")
	}
	if exec == nil {
		return fmt.Errorf("execution score publisher received nil execution")
	}
	if !isTerminalExecutionStatus(exec.Status) {
		return fmt.Errorf("execution %q is not terminal (status %s)", exec.ID, exec.Status)
	}

	policy, policySHA, verdict := p.verdictFor(ctx, exec)
	if verdict.Kind == "" && policy != nil {
		verdict.Kind = policy.Kind
	}
	evidence, err := json.Marshal(verdict.CaseEvidence)
	if err != nil {
		return fmt.Errorf("marshal execution score evidence: %w", err)
	}
	row := &persistence.ExecutionQualityScore{
		ProjectID: exec.ProjectID, TaskID: exec.TaskID, ExecutionID: exec.ID,
		WorkflowID: exec.WorkflowID, WorkflowRevision: exec.WorkflowRevision,
		ScorerVersion: ExecutionScorerVersion, ScoringPolicySHA: policySHA,
		Kind: string(verdict.Kind), Status: string(verdict.Status), Score: verdict.Score,
		PassedCaseCount: verdict.PassedCaseCount, PinnedCaseCount: verdict.PinnedCaseCount,
		Diagnostic: verdict.Diagnostic, CaseEvidence: evidence, RecordedAt: p.now().UTC(),
	}
	if err := p.repo.Upsert(ctx, row); err != nil {
		if p.metrics != nil {
			p.metrics.WriteFailuresTotal.Inc()
		}
		return err
	}
	if p.metrics != nil {
		p.metrics.WritesTotal.WithLabelValues(row.Kind, row.Status).Inc()
	}
	return nil
}

// Reconcile is the completeness boundary: it asks persistence for terminal
// executions with no score row and continues after individual failures so one
// corrupt/write-blocked row cannot starve newer executions.
func (p *ExecutionScorePublisher) Reconcile(ctx context.Context, limit int) (ReconcileResult, error) {
	if p == nil || p.repo == nil {
		return ReconcileResult{}, fmt.Errorf("execution score publisher is not configured")
	}
	pending, err := p.repo.ListPendingTerminal(ctx, limit)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{Selected: len(pending)}
	var failures []error
	for _, exec := range pending {
		if err := p.Publish(ctx, exec); err != nil {
			result.Failed++
			failures = append(failures, fmt.Errorf("publish execution %s: %w", exec.ID, err))
			continue
		}
		result.Published++
	}
	if p.metrics != nil {
		stats, statsErr := p.repo.PendingTerminalStats(ctx, nil)
		if statsErr != nil {
			failures = append(failures, fmt.Errorf("read publication backlog: %w", statsErr))
		} else {
			p.metrics.PublicationPending.Set(float64(stats.Count))
			oldestSeconds := 0.0
			if stats.OldestAt != nil {
				oldestSeconds = p.now().Sub(*stats.OldestAt).Seconds()
				if oldestSeconds < 0 {
					oldestSeconds = 0
				}
			}
			p.metrics.OldestPendingSeconds.Set(oldestSeconds)
		}
	}
	return result, errors.Join(failures...)
}

// WithStepOutcomes wires the reader contract_satisfaction needs. Chainable so
// the daemon can opt in without changing the constructor's signature.
func (p *ExecutionScorePublisher) WithStepOutcomes(r StepOutcomeReader) *ExecutionScorePublisher {
	if p == nil {
		return p
	}
	p.outcomes = r
	return p
}

// verdictFor dispatches on the pinned policy's kind. pinned_case_validation
// reads the state snapshot alone; contract_satisfaction additionally needs the
// step outcomes, so it is scored only when a reader is wired.
func (p *ExecutionScorePublisher) verdictFor(ctx context.Context, exec *persistence.Execution) (*ScoringPolicy, string, ExecutionScore) {
	policy, policySHA, verdict := scoringVerdict(exec.WorkflowSnapshot, exec.StateSnapshot)
	if policy == nil || policy.Kind != ScoreKindContractSatisfaction {
		return policy, policySHA, verdict
	}
	if p.outcomes == nil {
		// Declared but unscorable on this deployment. Say so rather than
		// charging the measured system a zero for our missing wiring.
		return policy, policySHA, ExecutionScore{
			Kind: ScoreKindContractSatisfaction, Status: ScoreStatusNotApplicable,
			Diagnostic: "step_outcome_reader_not_configured",
		}
	}
	outcomes, err := p.outcomes.OutcomesByExecution(ctx, exec.ID)
	if err != nil {
		return policy, policySHA, invalidProductionVerdict(ScoreKindContractSatisfaction, DiagnosticMalformedEvidence)
	}
	contract, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: exec.WorkflowSnapshot,
		StepOutcomes:     outcomes,
		TerminalStatus:   string(exec.Status),
	})
	if err != nil {
		// Unreadable pinned evidence is a harness fault; production keeps a row
		// so the execution is never silently absent (§12.11.6).
		return policy, policySHA, invalidProductionVerdict(ScoreKindContractSatisfaction, DiagnosticCorruptWorkflowSnapshot)
	}
	return policy, policySHA, contract
}

func scoringVerdict(workflowSnapshot, stateSnapshot []byte) (*ScoringPolicy, string, ExecutionScore) {
	if len(workflowSnapshot) == 0 {
		verdict, _ := ScoreExecution(nil, stateSnapshot)
		return nil, "", verdict
	}
	if !json.Valid(workflowSnapshot) {
		return nil, "", invalidProductionVerdict("", DiagnosticCorruptWorkflowSnapshot)
	}
	var pinned struct {
		QualityScoring *ScoringPolicy `json:"qualityScoring"`
	}
	if err := json.Unmarshal(workflowSnapshot, &pinned); err != nil {
		return nil, "", invalidProductionVerdict("", DiagnosticCorruptWorkflowSnapshot)
	}
	if pinned.QualityScoring == nil {
		verdict, _ := ScoreExecution(nil, stateSnapshot)
		return nil, "", verdict
	}
	policySHA, err := scoringPolicyDigest(pinned.QualityScoring)
	if err != nil {
		return pinned.QualityScoring, "", invalidProductionVerdict(pinned.QualityScoring.Kind, DiagnosticUnsupportedScorePolicy)
	}
	if pinned.QualityScoring.Kind == ScoreKindContractSatisfaction {
		// Scored by the caller, which has the step outcomes this kind needs.
		return pinned.QualityScoring, policySHA, ExecutionScore{Kind: ScoreKindContractSatisfaction}
	}
	verdict, err := ScoreExecution(pinned.QualityScoring, stateSnapshot)
	if err != nil {
		diagnostic := DiagnosticCorruptStateSnapshot
		if pinned.QualityScoring.Kind != ScoreKindPinnedCaseValidation {
			diagnostic = DiagnosticUnsupportedScorePolicy
		}
		return pinned.QualityScoring, policySHA, invalidProductionVerdict(pinned.QualityScoring.Kind, diagnostic)
	}
	return pinned.QualityScoring, policySHA, verdict
}

func invalidProductionVerdict(kind ScoreKind, diagnostic string) ExecutionScore {
	zero := 0.0
	return ExecutionScore{
		Kind: kind, Status: ScoreStatusInvalidEvidence, Score: &zero,
		Diagnostic: diagnostic,
	}
}

func scoringPolicyDigest(policy *ScoringPolicy) (string, error) {
	if policy == nil {
		return "", nil
	}
	b, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func isTerminalExecutionStatus(status persistence.ExecutionStatus) bool {
	switch status {
	case persistence.ExecutionStatusCompleted, persistence.ExecutionStatusFailed, persistence.ExecutionStatusCancelled:
		return true
	default:
		return false
	}
}
