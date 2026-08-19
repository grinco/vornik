package quality

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ScoreKind identifies a deterministic execution-quality scoring contract.
type ScoreKind string

const (
	// ScoreKindPinnedCaseValidation grades the fraction of analyst-pinned test
	// cases that the verifier reports as passed or manually validated.
	ScoreKindPinnedCaseValidation ScoreKind = "pinned_case_validation"
)

// ScoreStatus separates a numeric verdict from applicability and evidence
// failures. Only not_applicable has no numeric score.
type ScoreStatus string

// ScoreStatus values distinguish scored, invalid, and inapplicable executions.
const (
	ScoreStatusScored          ScoreStatus = "scored"
	ScoreStatusMissingContract ScoreStatus = "missing_contract"
	ScoreStatusInvalidEvidence ScoreStatus = "invalid_evidence"
	ScoreStatusNotApplicable   ScoreStatus = "not_applicable"
)

// Score diagnostics are stable machine-readable reasons for non-clean verdicts.
const (
	DiagnosticMissingScoringContract = "missing_scoring_contract"
	// DiagnosticMissingProducerStep / DiagnosticMissingVerifierStep split
	// the old catch-all so a zero says WHICH half of the contract went
	// missing. Producer-only evidence means the contract is live and the
	// verifier failed — the state the operator surface has to make loud —
	// while neither half means the workflow does not really carry it.
	// Collapsing them (pre-2026-08-18) made the two indistinguishable in
	// the journal and on the quality page.
	DiagnosticMissingProducerStep     = "missing_producer_step"
	DiagnosticMissingVerifierStep     = "missing_verifier_step"
	DiagnosticMalformedEvidence       = "malformed_evidence"
	DiagnosticUnknownCaseID           = "unknown_case_id"
	DiagnosticUnknownCaseStatus       = "unknown_case_status"
	DiagnosticDuplicateAnalystCaseID  = "duplicate_analyst_case_id"
	DiagnosticPinnedCaseCountMismatch = "pinned_case_count_mismatch"
	DiagnosticNoPinnedCases           = "no_pinned_cases"
	DiagnosticConflictingCaseStatus   = "conflicting_case_status"
	DiagnosticEmptyCaseID             = "empty_case_id"
	DiagnosticCorruptStateSnapshot    = "corrupt_state_snapshot"
	DiagnosticCorruptWorkflowSnapshot = "corrupt_workflow_snapshot"
	DiagnosticUnsupportedScorePolicy  = "unsupported_score_policy"
)

// ScoringPolicy is the workflow-pinned description of where the scorer reads
// its producer and verifier evidence.
type ScoringPolicy struct {
	Kind         ScoreKind `json:"kind" yaml:"kind"`
	ProducerStep string    `json:"producerStep" yaml:"producerStep"`
	VerifierStep string    `json:"verifierStep" yaml:"verifierStep"`
}

// PinnedCaseEvidence is one verifier-emitted testing.cases[] entry.
type PinnedCaseEvidence struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// NormalizedCaseEvidence is the journal/storage representation. It contains
// exactly one row per analyst-pinned case, in analyst order; an omitted
// verifier row is represented explicitly as absent.
type NormalizedCaseEvidence struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Credited bool   `json:"credited"`
}

// ExecutionScore is the deterministic result of scoring one execution state
// snapshot. Score is nil only when the workflow has no scoring policy.
type ExecutionScore struct {
	Kind            ScoreKind                `json:"kind,omitempty"`
	Status          ScoreStatus              `json:"status"`
	Score           *float64                 `json:"score,omitempty"`
	PassedCaseCount int                      `json:"passedCaseCount"`
	PinnedCaseCount int                      `json:"pinnedCaseCount"`
	Diagnostic      string                   `json:"diagnostic,omitempty"`
	CaseEvidence    []NormalizedCaseEvidence `json:"caseEvidence,omitempty"`
	// ObligationEvidence is the contract_satisfaction analogue of
	// CaseEvidence: one entry per declared obligation. Empty for every other
	// scoring kind.
	ObligationEvidence []ObligationEvidence `json:"obligationEvidence,omitempty"`
}

type scoreState struct {
	StepResults map[string]json.RawMessage `json:"stepResults"`
}

type producerEnvelope struct {
	Analysis *struct {
		TestCaseIDs     []string `json:"test_case_ids"`
		TestCasesPinned *int     `json:"test_cases_pinned"`
	} `json:"analysis"`
}

type verifierEnvelope struct {
	Testing *struct {
		Cases []PinnedCaseEvidence `json:"cases"`
	} `json:"testing"`
}

// ValidateScoringPolicy checks the portable part of a scoring contract. A
// workflow registry additionally verifies that both named steps exist.
func ValidateScoringPolicy(policy *ScoringPolicy) error {
	if policy == nil {
		return nil
	}
	switch policy.Kind {
	case ScoreKindPinnedCaseValidation:
		// Producer/verifier pair required; checked below.
	case ScoreKindContractSatisfaction:
		// Reads the workflow's declared obligations, not a step pair.
		return nil
	default:
		return fmt.Errorf("unsupported execution score kind %q", policy.Kind)
	}
	if strings.TrimSpace(policy.ProducerStep) == "" || strings.TrimSpace(policy.VerifierStep) == "" {
		return fmt.Errorf("execution scoring policy requires producer and verifier steps")
	}
	if policy.ProducerStep == policy.VerifierStep {
		return fmt.Errorf("execution scoring producer and verifier steps must be distinct")
	}
	return nil
}

// ScoreExecution evaluates one state_snapshot using the workflow-pinned
// policy. Invalid JSON is a ledger-integrity error so benchmark callers can
// refuse the run; valid JSON with missing/malformed agent evidence is a durable
// zero verdict for production and benchmark aggregation.
func ScoreExecution(policy *ScoringPolicy, stateSnapshot []byte) (ExecutionScore, error) {
	if policy == nil {
		return ExecutionScore{Status: ScoreStatusNotApplicable}, nil
	}
	if err := ValidateScoringPolicy(policy); err != nil {
		return ExecutionScore{}, err
	}
	if len(stateSnapshot) == 0 {
		return scoreZero(policy.Kind, ScoreStatusMissingContract, DiagnosticMissingScoringContract, 0), nil
	}
	if !json.Valid(stateSnapshot) {
		return ExecutionScore{}, fmt.Errorf("decode execution state snapshot: invalid JSON")
	}

	var state scoreState
	if err := json.Unmarshal(stateSnapshot, &state); err != nil {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, DiagnosticMalformedEvidence, 0), nil
	}
	producerRaw, producerOK := state.StepResults[policy.ProducerStep]
	verifierRaw, verifierOK := state.StepResults[policy.VerifierStep]
	switch {
	case !producerOK && !verifierOK:
		return scoreZero(policy.Kind, ScoreStatusMissingContract, DiagnosticMissingScoringContract, 0), nil
	case !producerOK:
		return scoreZero(policy.Kind, ScoreStatusMissingContract, DiagnosticMissingProducerStep, 0), nil
	case !verifierOK:
		// Carry the producer's denominator when it is readable. A
		// pinned count on a missing-verifier zero is the difference
		// between "this workflow declares nothing" and "13 cases were
		// pinned and the tester never reported against them". Evidence
		// the producer did not publish stays 0 rather than guessed.
		pinned := 0
		if ids, count, diagnostic := decodePinnedProducer(producerRaw); diagnostic == "" && len(ids) > 0 {
			pinned = count
		}
		return scoreZero(policy.Kind, ScoreStatusMissingContract, DiagnosticMissingVerifierStep, pinned), nil
	}

	ids, pinned, diagnostic := decodePinnedProducer(producerRaw)
	fatal, softDiag := splitDiagnostic(diagnostic)
	if fatal != "" {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, fatal, pinned), nil
	}
	known, diagnostic := validatePinnedIDs(ids)
	if diagnostic != "" {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, diagnostic, pinned), nil
	}
	reported, diagnostic := decodeVerifierCases(verifierRaw, known)
	if diagnostic != "" {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, diagnostic, pinned), nil
	}

	result := ExecutionScore{
		Kind:            policy.Kind,
		Status:          ScoreStatusScored,
		PinnedCaseCount: pinned,
		Diagnostic:      softDiag,
		CaseEvidence:    make([]NormalizedCaseEvidence, 0, pinned),
	}
	for _, id := range ids {
		status, ok := reported[id]
		if !ok {
			status = "absent"
		}
		credited := status == "passed" || status == "manual"
		if credited {
			result.PassedCaseCount++
		}
		result.CaseEvidence = append(result.CaseEvidence, NormalizedCaseEvidence{
			ID: id, Status: status, Credited: credited,
		})
	}
	score := float64(result.PassedCaseCount) / float64(result.PinnedCaseCount)
	result.Score = &score
	return result, nil
}

func decodePinnedProducer(raw json.RawMessage) ([]string, int, string) {
	var producer producerEnvelope
	if err := json.Unmarshal(raw, &producer); err != nil || producer.Analysis == nil ||
		producer.Analysis.TestCasesPinned == nil || producer.Analysis.TestCaseIDs == nil {
		return nil, 0, DiagnosticMalformedEvidence
	}
	ids := producer.Analysis.TestCaseIDs
	pinned := *producer.Analysis.TestCasesPinned
	if pinned <= 0 || len(ids) == 0 {
		return nil, 0, DiagnosticNoPinnedCases
	}
	// A mismatch does NOT void the evidence. `test_cases_pinned` is documented
	// in the analyst schema as "How many cases you pinned. Must equal the length
	// of test_case_ids" — a count of a list the scorer can already see, so it
	// carries no information the ids do not, and only a failure mode. On
	// 2026-08-19 one miscounted integer (7 ids declared as 8) floored an
	// otherwise perfectly gradeable dev-pipeline run to invalid_evidence.
	//
	// The PUBLISHED ids are authoritative: a case the analyst did not publish
	// cannot be validated by the tester, the scorer, or anyone else, whatever
	// the count claims. So the denominator is len(ids), and the slip is returned
	// as a SOFT diagnostic — recorded, because an analyst that cannot count its
	// own list is worth knowing about, but not fatal.
	if pinned != len(ids) {
		return ids, len(ids), softDiagnostic(DiagnosticPinnedCaseCountMismatch)
	}
	return ids, pinned, ""
}

// softDiagnostic marks a diagnostic as recordable-but-not-fatal. Diagnostics are
// plain strings on the wire; the prefix is stripped before the value is stored,
// so nothing downstream sees it.
func softDiagnostic(d string) string { return softDiagnosticPrefix + d }

// softDiagnosticPrefix is internal to this file and never reaches a stored row.
const softDiagnosticPrefix = "soft:"

// splitDiagnostic separates a fatal diagnostic from a soft one.
func splitDiagnostic(d string) (fatal, soft string) {
	if rest, ok := strings.CutPrefix(d, softDiagnosticPrefix); ok {
		return "", rest
	}
	return d, ""
}

func validatePinnedIDs(ids []string) (map[string]struct{}, string) {
	known := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, DiagnosticEmptyCaseID
		}
		if _, duplicate := known[id]; duplicate {
			return nil, DiagnosticDuplicateAnalystCaseID
		}
		known[id] = struct{}{}
	}
	return known, ""
}

func decodeVerifierCases(raw json.RawMessage, known map[string]struct{}) (map[string]string, string) {
	var verifier verifierEnvelope
	if err := json.Unmarshal(raw, &verifier); err != nil || verifier.Testing == nil || verifier.Testing.Cases == nil {
		return nil, DiagnosticMalformedEvidence
	}
	reported := make(map[string]string, len(verifier.Testing.Cases))
	for _, c := range verifier.Testing.Cases {
		if strings.TrimSpace(c.ID) == "" {
			return nil, DiagnosticEmptyCaseID
		}
		if _, ok := known[c.ID]; !ok {
			return nil, DiagnosticUnknownCaseID
		}
		if !knownCaseStatus(c.Status) {
			return nil, DiagnosticUnknownCaseStatus
		}
		if prior, duplicate := reported[c.ID]; duplicate && prior != c.Status {
			return nil, DiagnosticConflictingCaseStatus
		}
		reported[c.ID] = c.Status
	}
	return reported, ""
}

func scoreZero(kind ScoreKind, status ScoreStatus, diagnostic string, pinned int) ExecutionScore {
	zero := 0.0
	return ExecutionScore{
		Kind: kind, Status: status, Score: &zero,
		PinnedCaseCount: pinned, Diagnostic: diagnostic,
	}
}

func knownCaseStatus(status string) bool {
	switch status {
	case "passed", "manual", "failed", "missing":
		return true
	default:
		return false
	}
}
