package quality

// D3' — the unit is the SUBTASK, and the scorer follows.
//
// The design is https://docs.vornik.io
// design.md, section "D3' — RESOLVED 2026-09-19".
//
// THE PROBLEM IT SOLVES. The analyst pins case ids for a whole task and the
// tester validates them. Handing the tester the WHOLE list makes every id whose
// subtask is not yet implemented `missing`, any `missing` forces
// `testing.passed` false, and the gate routes false back to `implement` — so on
// a genuine multi-subtask run the review step is never reached until the final
// subtask, and the checkpoint-report branch the autonomy loop depends on to
// resume across ticks is unreachable.
//
// The resolution: the producer groups its ids BY SUBTASK, the tester is handed
// one subtask's worth, and the scorer accumulates evidence across every visit
// of the verifier so the denominator stays the whole task.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SubtaskGroup is one subtask's pinned cases, as the producer declared them.
type SubtaskGroup struct {
	ID          string   `json:"id"`
	TestCaseIDs []string `json:"test_case_ids"`
}

// SubtaskProducerFieldPath is where the grouped authority lives, paired with
// the struct tags below exactly as PinnedProducerFieldPath is.
const SubtaskProducerFieldPath = "analysis.subtasks"

type subtaskProducerEnvelope struct {
	Analysis *struct {
		// Tags must spell out SubtaskProducerFieldPath.
		Subtasks []SubtaskGroup `json:"subtasks"`
	} `json:"analysis"`
}

// Diagnostics this file can raise.
const (
	// DiagnosticEmptySubtaskGroup is the producer minimum of D3'.4. An
	// analyst that pins zero cases for a subtask hands the tester an empty
	// list, nothing is `missing`, and the subtask passes vacuously — a
	// subtask that was never validated reading as one that was.
	DiagnosticEmptySubtaskGroup = "empty_subtask_group"
	// DiagnosticDuplicateSubtaskID means two groups claim one id, so
	// "which cases belong to s2" has two answers.
	DiagnosticDuplicateSubtaskID = "duplicate_subtask_id"
	// DiagnosticEmptySubtaskID is a group with no id: it can never be
	// selected, so its cases can never be run, while still counting in the
	// denominator.
	DiagnosticEmptySubtaskID = "empty_subtask_id"
)

// DecodeSubtaskGroups reads the grouped producer contract.
//
// It returns ok=false when the producer emitted no `analysis.subtasks` at all,
// which is the harness-6 shape and is not an error here — the caller falls back
// to the flat list. A diagnostic is returned only when groups ARE present and
// are wrong.
//
// EXPLICIT GROUPING, NEVER INFERRED FROM ID SPELLING. The obvious shortcut is
// to read the `sN_` prefix the analyst already tends to use. dp-10's analyst
// emitted `case_1…case_10` and then `s2_case_1…s2_case_4` in one list, so the
// prefix is a convention the producer sometimes follows — and inferring
// structure from a string another agent chose is the two-channel defect this
// whole amendment exists to remove.
func DecodeSubtaskGroups(raw json.RawMessage) (groups []SubtaskGroup, ok bool, diagnostic string) {
	var envelope subtaskProducerEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Analysis == nil || envelope.Analysis.Subtasks == nil {
		return nil, false, ""
	}
	groups = envelope.Analysis.Subtasks
	if len(groups) == 0 {
		return nil, false, ""
	}

	seen := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		id := strings.TrimSpace(g.ID)
		if id == "" {
			return nil, true, DiagnosticEmptySubtaskID
		}
		if _, dup := seen[id]; dup {
			return nil, true, DiagnosticDuplicateSubtaskID
		}
		seen[id] = struct{}{}
		if len(g.TestCaseIDs) == 0 {
			return nil, true, DiagnosticEmptySubtaskGroup
		}
	}
	return groups, true, ""
}

// UnionSubtaskCases is the whole-task denominator: the concatenation of every
// group's ids, in declared order, deduplicated.
//
// Computed by the SCORER rather than published by the producer, so there is one
// authority and one derivation. A published union could disagree with the
// groups it claims to summarise, and nothing would say which was right.
func UnionSubtaskCases(groups []SubtaskGroup) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, g := range groups {
		for _, id := range g.TestCaseIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// ErrUnknownSubtask is returned when a coder declares a subtask id the analyst
// never grouped.
var ErrUnknownSubtask = fmt.Errorf("quality: subtask id names no declared group")

// ErrEmptySubtaskID is returned for an absent or blank declaration.
var ErrEmptySubtaskID = fmt.Errorf("quality: subtask id is required and must not be empty")

// SelectSubtaskCases returns the cases the tester should be handed for one
// subtask.
//
// THREE REFUSALS, and they are deliberately distinct (D3'.2):
//
//   - an ABSENT or EMPTY id. An absent field already fails loudly through the
//     prompt_ref_unresolved rule; an EMPTY one resolves to "", matches no
//     group, and would hand the tester nothing — a silent degradation where
//     the absent case is a loud one.
//   - an id naming NO declared group. Present, non-empty, and referentially
//     broken: reachable from a coder that invents an id or an analyst that
//     renamed one. It degrades into the same vacuous pass by another route, so
//     it is a hard failure rather than an empty list handed down.
//   - a group that exists and is EMPTY cannot occur here, because
//     DecodeSubtaskGroups refuses it at the producer.
func SelectSubtaskCases(groups []SubtaskGroup, subtaskID string) ([]string, error) {
	id := strings.TrimSpace(subtaskID)
	if id == "" {
		return nil, ErrEmptySubtaskID
	}
	for _, g := range groups {
		if strings.TrimSpace(g.ID) == id {
			return g.TestCaseIDs, nil
		}
	}
	return nil, fmt.Errorf("%w: %q (declared: %s)", ErrUnknownSubtask, id, strings.Join(subtaskIDs(groups), ", "))
}

func subtaskIDs(groups []SubtaskGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.ID)
	}
	return out
}

// VerifierVisit is one visit of the verifier step, with the result body that
// visit produced.
//
// The bodies come from `execution_step_outcomes` — one row per visit, each
// carrying `result_hash` into the content-addressed store that migration 178
// populates. The snapshot's `stepResults` mirror cannot serve this and is not
// asked to: it is a single-visit latest-value view, not an append-history.
type VerifierVisit struct {
	Visit  int
	Result json.RawMessage
}

// AccumulateVerifierCases folds every visit's evidence into one verdict per
// case.
//
// THE RULE: per case, the verdict from the most recent visit that MENTIONED it.
//
// Not "passed in any visit", which an earlier draft said and which credits a
// case a later visit contradicted — `test(s2)` reports a pass, review rejects,
// `implement(s2)` redoes it, `test(s2)` now fails because the redo regressed
// it, and the case still scores as validated. A checkpointed regression reading
// as success is the opposite of what pinned_case_validation is for.
//
// SILENT ABSENCE RETAINS. A case reported in visit 1 and absent from visit 3
// keeps its visit-1 verdict, because visit 3 did not mention it. So the metric
// reads "validated unless a later visit contradicted it", not "validated by the
// latest visit" — a weaker claim than the rule's name suggests, and stated
// because a reader would assume the stronger one.
//
// The within-visit conflict check still applies PER VISIT: one report that says
// a case both passed and failed is incoherent evidence. Across visits the same
// disagreement is the regression above, and is exactly what must be honoured.
func AccumulateVerifierCases(visits []VerifierVisit, known map[string]struct{}) (map[string]string, int, string) {
	if len(visits) == 0 {
		return nil, 0, DiagnosticMissingScoringContract
	}

	reported := make(map[string]string)
	var extra int
	var soft string
	sawAny := false

	for _, v := range visits {
		perVisit, visitExtra, diagnostic := decodeVerifierCases(v.Result, known)
		fatal, visitSoft := splitDiagnostic(diagnostic)
		if fatal != "" {
			// One incoherent visit voids the accumulation. The
			// alternative — skip it and score the rest — would report a
			// number assembled from evidence the scorer itself judged
			// unreadable.
			return nil, 0, fatal
		}
		if visitSoft != "" && soft == "" {
			soft = softDiagnostic(visitSoft)
		}
		extra += visitExtra
		sawAny = true
		for id, status := range perVisit {
			reported[id] = status
		}
	}
	if !sawAny {
		return nil, 0, DiagnosticMissingScoringContract
	}
	return reported, extra, soft
}

// ScoreExecutionAcrossVisits is the harness-7 scorer: the same contract as
// ScoreExecution, with two differences that D3' requires.
//
//  1. The producer's authority may be `analysis.subtasks`, whose union is the
//     denominator. A producer still emitting the flat `analysis.test_case_ids`
//     is scored exactly as before, so a harness-6-shaped result does not become
//     unreadable.
//  2. A verifier visited more than once is ACCUMULATED from the per-visit
//     bodies instead of refused, provided those bodies were supplied.
//
// Supplying no visits is not an error: it means the caller could not read the
// ledger, and the result is then the `unscorable` floor that shipped on
// 2026-09-19 — a multi-visit execution refuses rather than being scored from
// one visit. That floor is what this replaces, and it stays reachable, because
// "the ledger was wiped by the next arm" is a real state and scoring it from
// the mirror would be exactly the defect the floor was built for.
//
// A re-entrant PRODUCER is still unscorable, whatever the visits say: the
// analyst runs once by contract, and a second run means the contract broke
// rather than that more evidence exists.
func ScoreExecutionAcrossVisits(policy *ScoringPolicy, stateSnapshot []byte, visits []VerifierVisit) (ExecutionScore, error) {
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

	producerVisits := state.VisitCounts[policy.ProducerStep]
	verifierVisits := state.VisitCounts[policy.VerifierStep]

	// Without per-visit bodies there is nothing to accumulate, so defer to
	// the single-visit scorer — which refuses a looped step rather than
	// scoring its last visit.
	if len(visits) == 0 {
		return ScoreExecution(policy, stateSnapshot)
	}
	if producerVisits > 1 {
		out := scoreZero(policy.Kind, ScoreStatusUnscorable, DiagnosticMultiVisitLastOnly, 0)
		out.ProducerVisits = producerVisits
		out.VerifierVisits = verifierVisits
		if raw, ok := state.StepResults[policy.ProducerStep]; ok {
			if ids, _, diag := decodeProducerContract(raw); diag == "" {
				out.PinnedCaseCount = len(ids)
			}
		}
		return out, nil
	}

	producerRaw, producerOK := state.StepResults[policy.ProducerStep]
	if !producerOK {
		return scoreZero(policy.Kind, ScoreStatusMissingContract, DiagnosticMissingProducerStep, 0), nil
	}

	ids, pinned, diagnostic := decodeProducerContract(producerRaw)
	fatal, softDiag := splitDiagnostic(diagnostic)
	if fatal != "" {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, fatal, pinned), nil
	}
	known, diagnostic := validatePinnedIDs(ids)
	if diagnostic != "" {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, diagnostic, pinned), nil
	}

	reported, extraCases, diagnostic := AccumulateVerifierCases(visits, known)
	verifierFatal, verifierSoft := splitDiagnostic(diagnostic)
	if verifierFatal == DiagnosticMissingScoringContract {
		return scoreZero(policy.Kind, ScoreStatusMissingContract, DiagnosticMissingVerifierStep, pinned), nil
	}
	if verifierFatal != "" {
		return scoreZero(policy.Kind, ScoreStatusInvalidEvidence, verifierFatal, pinned), nil
	}
	if softDiag == "" {
		softDiag = verifierSoft
	}

	return assemblePinnedScore(policy.Kind, ids, pinned, reported, extraCases, softDiag, verifierVisits), nil
}

// decodeProducerContract reads whichever producer shape is present: the
// grouped `analysis.subtasks` (harness 7) or the flat `analysis.test_case_ids`
// (harness 6). Groups win when both are present, because they are the
// authority and the flat list is derived.
//
// It returns the UNION rather than the groups: every caller wants the
// denominator, and the grouping itself is read from the producer directly by
// whoever needs it (DecodeSubtaskGroups is exported for that).
func decodeProducerContract(raw json.RawMessage) (ids []string, pinned int, diagnostic string) {
	groups, grouped, groupDiag := DecodeSubtaskGroups(raw)
	if groupDiag != "" {
		return nil, 0, groupDiag
	}
	if grouped {
		union := UnionSubtaskCases(groups)
		if len(union) == 0 {
			return nil, 0, DiagnosticNoPinnedCases
		}
		return union, len(union), ""
	}
	flatIDs, flatPinned, flatDiag := decodePinnedProducer(raw)
	return flatIDs, flatPinned, flatDiag
}

// assemblePinnedScore is the shared tail: credit each pinned id from the
// accumulated evidence, and divide.
func assemblePinnedScore(kind ScoreKind, ids []string, pinned int, reported map[string]string, extraCases int, softDiag string, verifierVisits int) ExecutionScore {
	result := ExecutionScore{
		Kind:            kind,
		Status:          ScoreStatusScored,
		PinnedCaseCount: pinned,
		ExtraCaseCount:  extraCases,
		Diagnostic:      softDiag,
		VerifierVisits:  verifierVisits,
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
	if pinned == 0 {
		return scoreZero(kind, ScoreStatusInvalidEvidence, DiagnosticNoPinnedCases, 0)
	}
	score := float64(result.PassedCaseCount) / float64(pinned)
	result.Score = &score
	return result
}
