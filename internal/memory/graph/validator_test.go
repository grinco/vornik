package graph

import (
	"context"
	"strings"
	"testing"
)

func TestValidator_KeepsHighScoreDropsLowScore(t *testing.T) {
	chunk := "ACME quoted $9,500 for the new ledger build."
	props := []EdgeProposal{
		{From: "kent-A", To: "kent-P", Predicate: "QUOTED_PRICE", Evidence: "ACME quoted $9,500"},
		{From: "kent-A", To: "kent-G", Predicate: "CHOSEN_OVER", Evidence: "ACME quoted $9,500"}, // not really chosen-over
	}
	body := `[
	  {"id":"prop-0","score":0.95,"reason":"explicit price quote"},
	  {"id":"prop-1","score":0.2,"reason":"no comparison stated"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "fake")

	got, m, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 scored edges, got %d", len(got))
	}
	if !got[0].Kept || got[1].Kept {
		t.Errorf("threshold gate wrong: %+v / %+v", got[0], got[1])
	}
	if m.Kept != 1 || m.Dropped != 1 {
		t.Errorf("metrics off: %+v", m)
	}
}

func TestValidator_OmittedProposalDefaultsToDrop(t *testing.T) {
	chunk := "x"
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
		{From: "c", To: "d", Predicate: "RELATES_TO", Evidence: "x"},
	}
	// Model only returned a score for prop-0; prop-1 missing.
	body := `[{"id":"prop-0","score":0.9,"reason":"ok"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")

	got, _, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !got[0].Kept || got[1].Kept {
		t.Errorf("expected prop-0 kept, prop-1 dropped (omitted), got %+v / %+v", got[0], got[1])
	}
	if got[1].Reason == "" {
		t.Errorf("omitted proposal missing reason")
	}
}

func TestValidator_ClampsScore(t *testing.T) {
	cases := map[float32]float32{
		0.5:  0.5,
		1.5:  1,    // clamp >1
		-0.1: 0,    // clamp <0
		95:   0.95, // 0..100 scale auto-corrected
	}
	for in, want := range cases {
		if got := clampScore(in); got != want {
			t.Errorf("clampScore(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestValidator_ThresholdRespected(t *testing.T) {
	chunk := "x"
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
	}
	body := `[{"id":"prop-0","score":0.7,"reason":"borderline"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")
	v.Threshold = 0.71 // bump above default

	got, _, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got[0].Kept {
		t.Errorf("score 0.7 with threshold 0.71 should drop, got Kept=true")
	}
}

func TestValidator_PreservesInputOrder(t *testing.T) {
	chunk := "x"
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
		{From: "c", To: "d", Predicate: "RELATES_TO", Evidence: "x"},
		{From: "e", To: "f", Predicate: "RELATES_TO", Evidence: "x"},
	}
	// Out-of-order LLM response.
	body := `[
	  {"id":"prop-2","score":0.9},
	  {"id":"prop-0","score":0.1},
	  {"id":"prop-1","score":0.85}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")

	got, _, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got[0].Proposal.From != "a" || got[1].Proposal.From != "c" || got[2].Proposal.From != "e" {
		t.Errorf("input order not preserved: %+v", got)
	}
	if got[0].Kept || !got[1].Kept || !got[2].Kept {
		t.Errorf("scores misapplied to wrong proposals: %+v", got)
	}
}

func TestValidator_EmptyProposalsShortCircuits(t *testing.T) {
	fp := &fakeProvider{}
	v := NewValidator(fp, "")
	got, _, err := v.Validate(context.Background(), "any", nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil output for empty input, got %+v", got)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("expected no LLM call, got %d", fp.calls.Load())
	}
}

func TestValidator_BuildsUserMessage(t *testing.T) {
	captured := &capturingProvider{reply: `[{"id":"prop-0","score":0.9}]`}
	v := NewValidator(captured, "")
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "trivia"},
	}
	_, _, err := v.Validate(context.Background(), "the chunk", props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !strings.Contains(captured.userMsg, "CHUNK:") || !strings.Contains(captured.userMsg, "PROPOSED RELATIONSHIPS:") {
		t.Errorf("user message missing labels: %q", captured.userMsg)
	}
}

func TestValidator_NilClient(t *testing.T) {
	v := &Validator{}
	_, _, err := v.Validate(context.Background(), "x", []EdgeProposal{{}})
	if err == nil {
		t.Fatal("expected error when Client is nil")
	}
}

func TestValidator_AcceptsObjectWrapping(t *testing.T) {
	chunk := "x"
	props := []EdgeProposal{{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"}}
	body := `{"scores":[{"id":"prop-0","score":0.9}]}`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")
	got, _, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !got[0].Kept {
		t.Errorf("object-wrap parse failed: %+v", got)
	}
}

// 2026-05-25 follow-on to the relationship-stage per-reason
// metric (commit 62c3e50). The validator has two distinct drop
// modes — LLM truncated its output (missing_score) vs LLM
// scored low (below_threshold). Conflating them obscures whether
// the daemon should re-prompt the validator (missing_score
// dominant) or tune the threshold (below_threshold dominant).

func TestValidator_DropsByReason_MissingScore(t *testing.T) {
	chunk := "x"
	// Two proposals; validator returns score for prop-0 only.
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
		{From: "c", To: "d", Predicate: "RELATES_TO", Evidence: "x"},
	}
	body := `[{"id":"prop-0","score":0.9,"reason":"ok"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")
	_, m, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.DropsByReason[ValidatorDropReasonMissingScore] != 1 {
		t.Errorf("expected missing_score=1, got map=%v", m.DropsByReason)
	}
	if m.DropsByReason[ValidatorDropReasonBelowThreshold] != 0 {
		t.Errorf("missing_score case must not increment below_threshold, got map=%v", m.DropsByReason)
	}
}

func TestValidator_DropsByReason_BelowThreshold(t *testing.T) {
	chunk := "x"
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
	}
	body := `[{"id":"prop-0","score":0.3,"reason":"weak evidence"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")
	_, m, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.DropsByReason[ValidatorDropReasonBelowThreshold] != 1 {
		t.Errorf("expected below_threshold=1, got map=%v", m.DropsByReason)
	}
	if m.DropsByReason[ValidatorDropReasonMissingScore] != 0 {
		t.Errorf("below_threshold case must not increment missing_score, got map=%v", m.DropsByReason)
	}
}

func TestValidator_DropsByReason_SumEqualsTotal(t *testing.T) {
	// Mixed batch — one kept, one missing-score, one below-
	// threshold. Sum of DropsByReason must equal Dropped.
	chunk := "x"
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
		{From: "c", To: "d", Predicate: "RELATES_TO", Evidence: "x"},
		{From: "e", To: "f", Predicate: "RELATES_TO", Evidence: "x"},
	}
	// prop-0 kept (0.9); prop-1 omitted entirely; prop-2 scored
	// below threshold.
	body := `[
	  {"id":"prop-0","score":0.9,"reason":"ok"},
	  {"id":"prop-2","score":0.2,"reason":"weak"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")
	_, m, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Kept != 1 || m.Dropped != 2 {
		t.Fatalf("expected kept=1 dropped=2, got kept=%d dropped=%d", m.Kept, m.Dropped)
	}
	sum := 0
	for _, n := range m.DropsByReason {
		sum += n
	}
	if sum != m.Dropped {
		t.Errorf("DropsByReason sum %d != Dropped %d (map=%v)", sum, m.Dropped, m.DropsByReason)
	}
	if m.DropsByReason[ValidatorDropReasonMissingScore] != 1 {
		t.Errorf("expected missing_score=1, got %d", m.DropsByReason[ValidatorDropReasonMissingScore])
	}
	if m.DropsByReason[ValidatorDropReasonBelowThreshold] != 1 {
		t.Errorf("expected below_threshold=1, got %d", m.DropsByReason[ValidatorDropReasonBelowThreshold])
	}
}

func TestValidator_DropsByReason_AllKeptIsEmpty(t *testing.T) {
	// Defence: when every proposal is kept, DropsByReason must
	// be non-nil but empty. A nil map would crash the pipeline's
	// `for reason, n := range m.Validate.DropsByReason` emission
	// loop on some Go versions; we want the explicit invariant.
	chunk := "x"
	props := []EdgeProposal{
		{From: "a", To: "b", Predicate: "RELATES_TO", Evidence: "x"},
	}
	body := `[{"id":"prop-0","score":0.95,"reason":"strong"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	v := NewValidator(fp, "")
	_, m, err := v.Validate(context.Background(), chunk, props)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.DropsByReason == nil {
		t.Fatal("DropsByReason must be non-nil even when empty")
	}
	if len(m.DropsByReason) != 0 {
		t.Errorf("DropsByReason should be empty when all kept, got %v", m.DropsByReason)
	}
	if m.Dropped != 0 || m.Kept != 1 {
		t.Errorf("counts off: kept=%d dropped=%d", m.Kept, m.Dropped)
	}
}
