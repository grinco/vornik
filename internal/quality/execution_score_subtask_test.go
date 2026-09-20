package quality

// The D3' scorer invariant suite.
//
// This file IS the mitigation. The bench tasks remain too small to exercise
// the rework loop — every scored execution to date visited each step exactly
// once — so this resolution ships UNEXERCISED by any arm until the harder task
// tier lands. §12.16's rework tripwire is NOT the guard: it detects that the
// loop RAN, not that the scorer computed the right number, so a scoring defect
// would ship dormant while the tripwire reported "rework happened".
//
// HarnessVersion 7 does not land until these are green. The six cases below
// are the ones the design enumerates, in its order.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func groupedProducer(t *testing.T, groups ...SubtaskGroup) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{"analysis": map[string]any{"subtasks": groups}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func verifierReport(t *testing.T, cases map[string]string) json.RawMessage {
	t.Helper()
	list := make([]map[string]string, 0, len(cases))
	for id, status := range cases {
		list = append(list, map[string]string{"id": id, "status": status})
	}
	body, err := json.Marshal(map[string]any{"testing": map[string]any{"cases": list}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func snapshotWith(t *testing.T, producer json.RawMessage, verifierVisits int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"stepResults": map[string]json.RawMessage{"analyze": producer},
		"visitCounts": map[string]int{"analyze": 1, "test": verifierVisits},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// ---- 1. the whole-task union as the denominator, across subtask groups ----

func TestD3Invariant1_DenominatorIsTheWholeTaskUnion(t *testing.T) {
	groups := []SubtaskGroup{
		{ID: "s1", TestCaseIDs: []string{"s1_case_1", "s1_case_2"}},
		{ID: "s2", TestCaseIDs: []string{"s2_case_1"}},
	}
	producer := groupedProducer(t, groups...)

	// Only s1 was tested. The denominator must still be the whole task —
	// otherwise a partially-done feature scores 1.0 for doing half the work.
	visits := []VerifierVisit{{Visit: 1, Result: verifierReport(t, map[string]string{
		"s1_case_1": "passed", "s1_case_2": "passed",
	})}}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 1), visits)
	if err != nil {
		t.Fatalf("ScoreExecutionAcrossVisits() = %v", err)
	}
	if got.PinnedCaseCount != 3 {
		t.Fatalf("PinnedCaseCount = %d, want 3 (the union, not the tested group)", got.PinnedCaseCount)
	}
	if got.PassedCaseCount != 2 {
		t.Fatalf("PassedCaseCount = %d, want 2", got.PassedCaseCount)
	}
	if got.Score == nil || *got.Score != 2.0/3.0 {
		t.Fatalf("Score = %v, want 2/3", got.Score)
	}
	// The untested case reads as absent, not as failed: nothing claimed it
	// ran and lost.
	var sawAbsent bool
	for _, e := range got.CaseEvidence {
		if e.ID == "s2_case_1" && e.Status == "absent" {
			sawAbsent = true
		}
	}
	if !sawAbsent {
		t.Fatalf("CaseEvidence = %+v, want s2_case_1 absent", got.CaseEvidence)
	}
}

func TestD3Invariant1_UnionDeduplicatesAndKeepsOrder(t *testing.T) {
	union := UnionSubtaskCases([]SubtaskGroup{
		{ID: "s1", TestCaseIDs: []string{"a", "b"}},
		{ID: "s2", TestCaseIDs: []string{"b", "c"}},
	})
	if strings.Join(union, ",") != "a,b,c" {
		t.Fatalf("UnionSubtaskCases() = %v, want [a b c] — deduplicated, declared order", union)
	}
}

// ---- 2. the latest-visit verdict on a REGRESSION ----

func TestD3Invariant2_ALaterFailureBeatsAnEarlierPass(t *testing.T) {
	// The reachable hazard: test(s2) passes, review rejects,
	// implement(s2) redoes it, test(s2) now fails because the redo
	// regressed it. Under "ever-passed" the case scores as validated and a
	// checkpointed regression reads as success.
	producer := groupedProducer(t, SubtaskGroup{ID: "s2", TestCaseIDs: []string{"s2_case_1", "s2_case_2"}})
	visits := []VerifierVisit{
		{Visit: 1, Result: verifierReport(t, map[string]string{"s2_case_1": "passed", "s2_case_2": "passed"})},
		{Visit: 2, Result: verifierReport(t, map[string]string{"s2_case_1": "failed", "s2_case_2": "passed"})},
	}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 2), visits)
	if err != nil {
		t.Fatalf("ScoreExecutionAcrossVisits() = %v", err)
	}
	if got.Status != ScoreStatusScored {
		t.Fatalf("Status = %q, want scored — a multi-visit verifier with bodies is scorable", got.Status)
	}
	if got.PassedCaseCount != 1 {
		t.Fatalf("PassedCaseCount = %d, want 1: the regressed case must not keep its earlier pass", got.PassedCaseCount)
	}
}

func TestD3Invariant2_ALaterPassBeatsAnEarlierFailure(t *testing.T) {
	// The ordinary rework direction, which must also work — otherwise the
	// loop this resolution restores could never score better than its
	// first attempt.
	producer := groupedProducer(t, SubtaskGroup{ID: "s2", TestCaseIDs: []string{"s2_case_1"}})
	visits := []VerifierVisit{
		{Visit: 1, Result: verifierReport(t, map[string]string{"s2_case_1": "failed"})},
		{Visit: 2, Result: verifierReport(t, map[string]string{"s2_case_1": "passed"})},
	}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 2), visits)
	if err != nil {
		t.Fatal(err)
	}
	if got.PassedCaseCount != 1 {
		t.Fatalf("PassedCaseCount = %d, want 1", got.PassedCaseCount)
	}
}

// ---- 3. the latest-visit verdict on a SILENT ABSENCE ----

func TestD3Invariant3_SilenceRetainsTheEarlierVerdict(t *testing.T) {
	// Listed separately from the regression case because the rule resolves
	// them DIFFERENTLY: one is contradiction, the other is retention, and a
	// scorer that collapsed them would pass the regression case and fail
	// this one.
	producer := groupedProducer(t,
		SubtaskGroup{ID: "s1", TestCaseIDs: []string{"s1_case_1"}},
		SubtaskGroup{ID: "s2", TestCaseIDs: []string{"s2_case_1"}},
	)
	visits := []VerifierVisit{
		{Visit: 1, Result: verifierReport(t, map[string]string{"s1_case_1": "passed"})},
		// Visit 3 tests s2 only, and says NOTHING about s1_case_1.
		{Visit: 3, Result: verifierReport(t, map[string]string{"s2_case_1": "passed"})},
	}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 2), visits)
	if err != nil {
		t.Fatal(err)
	}
	if got.PassedCaseCount != 2 {
		t.Fatalf("PassedCaseCount = %d, want 2: a silence is not a contradiction and must not flip a pass", got.PassedCaseCount)
	}

	// And the converse: a silence must not flip a FAILURE to a pass either.
	failFirst := []VerifierVisit{
		{Visit: 1, Result: verifierReport(t, map[string]string{"s1_case_1": "failed"})},
		{Visit: 3, Result: verifierReport(t, map[string]string{"s2_case_1": "passed"})},
	}
	got, err = ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 2), failFirst)
	if err != nil {
		t.Fatal(err)
	}
	if got.PassedCaseCount != 1 {
		t.Fatalf("PassedCaseCount = %d, want 1: a silence must not rehabilitate a failure", got.PassedCaseCount)
	}
}

// ---- 4. an empty subtask group ----

func TestD3Invariant4_AnEmptySubtaskGroupIsRefusedAtTheProducer(t *testing.T) {
	// An analyst that pins zero cases for s2 hands the tester an empty
	// list, nothing is `missing`, and s2 passes vacuously — a subtask that
	// was never validated reading as one that was. Without this rule the
	// gate's strictness is true and useless.
	producer := groupedProducer(t,
		SubtaskGroup{ID: "s1", TestCaseIDs: []string{"s1_case_1"}},
		SubtaskGroup{ID: "s2"},
	)
	_, ok, diagnostic := DecodeSubtaskGroups(producer)
	if !ok {
		t.Fatal("DecodeSubtaskGroups did not see the groups at all")
	}
	if diagnostic != DiagnosticEmptySubtaskGroup {
		t.Fatalf("diagnostic = %q, want %q", diagnostic, DiagnosticEmptySubtaskGroup)
	}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 1),
		[]VerifierVisit{{Visit: 1, Result: verifierReport(t, map[string]string{"s1_case_1": "passed"})}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusInvalidEvidence {
		t.Fatalf("Status = %q, want invalid_evidence — a vacuous subtask must not score", got.Status)
	}
}

func TestD3Invariant4_DuplicateAndBlankSubtaskIDsAreRefused(t *testing.T) {
	dup := groupedProducer(t,
		SubtaskGroup{ID: "s1", TestCaseIDs: []string{"a"}},
		SubtaskGroup{ID: "s1", TestCaseIDs: []string{"b"}},
	)
	if _, _, d := DecodeSubtaskGroups(dup); d != DiagnosticDuplicateSubtaskID {
		t.Fatalf("diagnostic = %q, want %q: 'which cases belong to s1' must have one answer", d, DiagnosticDuplicateSubtaskID)
	}

	blank := groupedProducer(t, SubtaskGroup{ID: "  ", TestCaseIDs: []string{"a"}})
	if _, _, d := DecodeSubtaskGroups(blank); d != DiagnosticEmptySubtaskID {
		// A group nothing can select still counts in the denominator, so
		// its cases can never be run and never be credited.
		t.Fatalf("diagnostic = %q, want %q", d, DiagnosticEmptySubtaskID)
	}
}

// ---- 5. absent, empty, and unknown subtask_id ----

func TestD3Invariant5_TheThreeSubtaskIDRefusalsAreDistinct(t *testing.T) {
	groups := []SubtaskGroup{{ID: "s1", TestCaseIDs: []string{"s1_case_1"}}}

	t.Run("absent", func(t *testing.T) {
		if _, err := SelectSubtaskCases(groups, ""); !errors.Is(err, ErrEmptySubtaskID) {
			t.Fatalf("err = %v, want ErrEmptySubtaskID", err)
		}
	})
	t.Run("empty after trimming", func(t *testing.T) {
		// The dangerous one: a field that is PRESENT and blank resolves to
		// "", matches no group, and would hand the tester nothing — a
		// silent degradation where the absent case is a loud one.
		if _, err := SelectSubtaskCases(groups, "   "); !errors.Is(err, ErrEmptySubtaskID) {
			t.Fatalf("err = %v, want ErrEmptySubtaskID", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		// Present, non-empty, referentially broken: a coder that invented
		// an id, or an analyst that renamed one. A hard failure, not an
		// empty list handed down to pass vacuously.
		_, err := SelectSubtaskCases(groups, "s2")
		if !errors.Is(err, ErrUnknownSubtask) {
			t.Fatalf("err = %v, want ErrUnknownSubtask", err)
		}
		if !strings.Contains(err.Error(), "s1") {
			t.Fatalf("err = %q, want it to name the groups that DO exist", err)
		}
	})
	t.Run("known", func(t *testing.T) {
		ids, err := SelectSubtaskCases(groups, "s1")
		if err != nil || len(ids) != 1 {
			t.Fatalf("SelectSubtaskCases() = %v, %v", ids, err)
		}
	})
}

// ---- 6. the reader's row-per-visit assumption ----

func TestD3Invariant6_OneRowPerVisitOrderedNewestMentionWins(t *testing.T) {
	// Pinned because the F1 exchange established that the BODIES exist —
	// one execution_step_outcomes row per visit, migration 178 carrying
	// result_hash into the content-addressed store — while the query that
	// joins them is the new work. The accumulation depends on the rows
	// arriving in visit order, so that is asserted rather than assumed.
	known := map[string]struct{}{"a": {}, "b": {}}
	visits := []VerifierVisit{
		{Visit: 1, Result: verifierReport(t, map[string]string{"a": "passed", "b": "passed"})},
		{Visit: 2, Result: verifierReport(t, map[string]string{"a": "failed"})},
		{Visit: 3, Result: verifierReport(t, map[string]string{"a": "passed"})},
	}
	reported, _, diagnostic := AccumulateVerifierCases(visits, known)
	if diagnostic != "" {
		t.Fatalf("diagnostic = %q", diagnostic)
	}
	if reported["a"] != "passed" {
		t.Fatalf("a = %q, want the LAST visit's verdict", reported["a"])
	}
	if reported["b"] != "passed" {
		t.Fatalf("b = %q, want visit 1's verdict retained through two silences", reported["b"])
	}
}

func TestD3Invariant6_AnIncoherentVisitVoidsTheAccumulation(t *testing.T) {
	// Within ONE visit, a case that both passed and failed is incoherent
	// evidence. Skipping it and scoring the rest would report a number
	// assembled from evidence the scorer itself judged unreadable.
	known := map[string]struct{}{"a": {}}
	body := json.RawMessage(`{"testing":{"cases":[{"id":"a","status":"passed"},{"id":"a","status":"failed"}]}}`)
	_, _, diagnostic := AccumulateVerifierCases([]VerifierVisit{{Visit: 1, Result: body}}, known)
	if diagnostic != DiagnosticConflictingCaseStatus {
		t.Fatalf("diagnostic = %q, want %q", diagnostic, DiagnosticConflictingCaseStatus)
	}
}

func TestD3Invariant6_NoVisitsIsMissingEvidenceNotAnEmptyPass(t *testing.T) {
	_, _, diagnostic := AccumulateVerifierCases(nil, map[string]struct{}{"a": {}})
	if diagnostic != DiagnosticMissingScoringContract {
		t.Fatalf("diagnostic = %q, want %q", diagnostic, DiagnosticMissingScoringContract)
	}
}

// ---- the floor this replaces stays reachable ----

func TestD3_WithoutVisitBodiesTheUnscorableFloorStillApplies(t *testing.T) {
	// "The ledger was wiped by the next arm" is a real state, and scoring
	// it from the single-visit mirror is exactly the defect the floor was
	// built for.
	producer := groupedProducer(t, SubtaskGroup{ID: "s1", TestCaseIDs: []string{"s1_case_1"}})
	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, producer, 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusUnscorable {
		t.Fatalf("Status = %q, want unscorable with no per-visit bodies", got.Status)
	}
}

func TestD3_AReEntrantProducerIsStillUnscorable(t *testing.T) {
	// The analyst runs once BY CONTRACT. A second run means the contract
	// broke, not that more evidence exists.
	producer := groupedProducer(t, SubtaskGroup{ID: "s1", TestCaseIDs: []string{"s1_case_1", "s1_case_2"}})
	snapshot, err := json.Marshal(map[string]any{
		"stepResults": map[string]json.RawMessage{"analyze": producer},
		"visitCounts": map[string]int{"analyze": 2, "test": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	visits := []VerifierVisit{{Visit: 1, Result: verifierReport(t, map[string]string{"s1_case_1": "passed"})}}

	got, scoreErr := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshot, visits)
	if scoreErr != nil {
		t.Fatal(scoreErr)
	}
	if got.Status != ScoreStatusUnscorable || got.Diagnostic != DiagnosticMultiVisitLastOnly {
		t.Fatalf("got %q/%q, want unscorable/%s", got.Status, got.Diagnostic, DiagnosticMultiVisitLastOnly)
	}
	// The denominator is still readable, and carrying it is the difference
	// between "we could not score this" and "there was nothing here".
	if got.PinnedCaseCount != 2 {
		t.Fatalf("PinnedCaseCount = %d, want 2", got.PinnedCaseCount)
	}
}

// ---- the harness-6 shape keeps scoring ----

func TestD3_AFlatProducerIsStillScored(t *testing.T) {
	// A producer emitting the old analysis.test_case_ids must not become
	// unreadable, or every harness-6 journal would stop replaying.
	flat := json.RawMessage(`{"analysis":{"test_case_ids":["c1","c2"],"test_cases_pinned":2}}`)
	visits := []VerifierVisit{{Visit: 1, Result: verifierReport(t, map[string]string{"c1": "passed", "c2": "failed"})}}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, flat, 1), visits)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ScoreStatusScored || got.PinnedCaseCount != 2 || got.PassedCaseCount != 1 {
		t.Fatalf("got %+v, want a scored 1/2", got)
	}
}

func TestD3_GroupsWinWhenBothShapesArePresent(t *testing.T) {
	// The flat list is DERIVED once groups exist. If they disagree, the
	// authority decides — otherwise there are two answers and nothing says
	// which is right.
	both := json.RawMessage(`{"analysis":{"test_case_ids":["stale"],"test_cases_pinned":1,` +
		`"subtasks":[{"id":"s1","test_case_ids":["s1_case_1","s1_case_2"]}]}}`)
	visits := []VerifierVisit{{Visit: 1, Result: verifierReport(t, map[string]string{"s1_case_1": "passed", "s1_case_2": "passed"})}}

	got, err := ScoreExecutionAcrossVisits(pinnedPolicy(), snapshotWith(t, both, 1), visits)
	if err != nil {
		t.Fatal(err)
	}
	if got.PinnedCaseCount != 2 {
		t.Fatalf("PinnedCaseCount = %d, want the GROUPED authority's 2, not the flat list's 1", got.PinnedCaseCount)
	}
}
