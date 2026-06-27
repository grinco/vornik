package workflowhealing

// Wave 3 — high-value tests for the self-healing genome's untested
// edges: deterministic-recipe construction + hashing, the
// step-selection heuristic, the recipe→proposal/candidate synthesis
// keystone, the replay trial's fail-closed wiring branches, and the
// load-bearing promotion safety gates. All TESTS ONLY (no production
// changes); these target branches the existing wave-1/2 suites leave
// uncovered (see coverprofile: BuildRetryBudgetCandidate,
// BuildVerifierInsertionCandidate, firstFailedTerminal, recipeProposalKind,
// RetryBudgetRecipe clamps, finalizeRecipe, runReplay registrar wiring).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ---- determinism + hashing -------------------------------------------

// TestW3HealRetryBudgetRecipeDeterministic: the same recipe inputs MUST
// produce a byte-identical candidate genome (same diff + same hash) on
// every run. The promotion gate compares against candidate_genome_hash,
// so a non-deterministic recipe would make a candidate un-trialable.
func TestW3HealRetryBudgetRecipeDeterministic(t *testing.T) {
	base := parseRecipeBaseline(t)
	r1, err := RetryBudgetRecipe(base, "write", 1, "failed", []string{"e1", "e2"})
	if err != nil {
		t.Fatalf("recipe #1: %v", err)
	}
	r2, err := RetryBudgetRecipe(parseRecipeBaseline(t), "write", 1, "failed", []string{"e1", "e2"})
	if err != nil {
		t.Fatalf("recipe #2: %v", err)
	}
	if r1.CandidateGenomeHash != r2.CandidateGenomeHash {
		t.Errorf("non-deterministic hash: %q vs %q", r1.CandidateGenomeHash, r2.CandidateGenomeHash)
	}
	if r1.ProposalDiff != r2.ProposalDiff {
		t.Error("non-deterministic candidate WORKFLOW.md diff")
	}
}

// TestW3HealVerifierInsertionRecipeDeterministic mirrors the above for
// the structurally-larger verifier-insertion recipe (adds a step + a
// rewire), which has more surface to drift.
func TestW3HealVerifierInsertionRecipeDeterministic(t *testing.T) {
	r1, err := VerifierInsertionRecipe(parseRecipeBaseline(t), "write", "verifier", "", []string{"e1"})
	if err != nil {
		t.Fatalf("recipe #1: %v", err)
	}
	r2, err := VerifierInsertionRecipe(parseRecipeBaseline(t), "write", "verifier", "", []string{"e1"})
	if err != nil {
		t.Fatalf("recipe #2: %v", err)
	}
	if r1.CandidateGenomeHash != r2.CandidateGenomeHash || r1.ProposalDiff != r2.ProposalDiff {
		t.Error("verifier-insertion recipe is not deterministic")
	}
}

// TestW3HealRetryBudgetRecipeNegativeBudgetFloorsAtZero exercises the
// lower clamp's floor arm: a negative requested budget is clamped UP to
// retryBudgetFloor (0), never below it.
func TestW3HealRetryBudgetRecipeNegativeBudgetFloorsAtZero(t *testing.T) {
	base := parseRecipeBaseline(t)
	res, err := RetryBudgetRecipe(base, "write", -5, "failed", nil)
	if err != nil {
		t.Fatalf("RetryBudgetRecipe: %v", err)
	}
	if got := res.CandidateWorkflow.Steps["write"].RetryPolicy.MaxRetries; got != 0 {
		t.Errorf("maxRetries = %d, want clamped to 0 (floor), never negative", got)
	}
}

// TestW3HealRetryBudgetRecipeOnFailTargetMayBeAStep: an on_fail target
// that is a STEP (not a terminal) is accepted — exercises the
// step-exists branch of the target-validation guard (the existing suite
// only covers a terminal target + a bogus one).
func TestW3HealRetryBudgetRecipeOnFailTargetMayBeAStep(t *testing.T) {
	base := parseRecipeBaseline(t)
	// "write" has no on_fail; route it to the "deploy" STEP. deploy exists,
	// so the recipe must accept it rather than ErrRecipeStepNotFound.
	res, err := RetryBudgetRecipe(base, "write", 1, "deploy", nil)
	if err != nil {
		t.Fatalf("on_fail to a step should be accepted: %v", err)
	}
	if got := res.CandidateWorkflow.Steps["write"].OnFail; got != "deploy" {
		t.Errorf("write.on_fail = %q, want deploy (a step target)", got)
	}
}

// ---- firstFailedTerminal (62.5% → exercise both arms) -----------------

// TestW3HealFirstFailedTerminalPicksAlphabeticallyFirst: with multiple
// FAILED-status terminals the helper must return the alphabetically
// smallest, deterministically. Built programmatically with UPPERCASE
// "FAILED" status (the parsed-markdown path normalises to lowercase, so
// the markdown fixtures never reach this arm).
func TestW3HealFirstFailedTerminalPicksAlphabeticallyFirst(t *testing.T) {
	wf := &registry.Workflow{
		Terminals: map[string]registry.WorkflowTerminal{
			"zfail":     {Status: "FAILED"},
			"afail":     {Status: "FAILED"},
			"completed": {Status: "COMPLETED"},
		},
	}
	if got := firstFailedTerminal(wf); got != "afail" {
		t.Errorf("firstFailedTerminal = %q, want afail (alphabetically first FAILED)", got)
	}
}

// TestW3HealFirstFailedTerminalNoneReturnsEmpty: a workflow with no
// FAILED terminal yields "" (the recipe then proposes no on_fail target).
func TestW3HealFirstFailedTerminalNoneReturnsEmpty(t *testing.T) {
	wf := &registry.Workflow{
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	if got := firstFailedTerminal(wf); got != "" {
		t.Errorf("firstFailedTerminal = %q, want empty (no FAILED terminal)", got)
	}
}

// w3RetryBaselineWithUpperTerminals builds a parsed-then-reconstructed
// baseline whose FAILED terminal carries the UPPERCASE status, so
// BuildRetryBudgetCandidate's firstFailedTerminal fallback actually
// resolves a target. The step under test lacks an on_fail so the
// fallback path is taken.
func w3RetryBaselineWithUpperTerminals() *registry.Workflow {
	return &registry.Workflow{
		ID:          "demo",
		Description: "demo workflow for retry-budget fallback",
		Version:     "1.0",
		Entrypoint:  "impl",
		Steps: map[string]registry.WorkflowStep{
			"impl": {
				Type:        "agent",
				Role:        "coder",
				Prompt:      "do the work",
				OnSuccess:   "done",
				RetryPolicy: registry.WorkflowRetryPolicy{MaxRetries: 4},
			},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
}

// TestW3HealBuildRetryBudgetUsesFirstFailedTerminalFallback: when the
// offending step has NO on_fail, the builder must route exhausted
// retries to firstFailedTerminal (the "failed" terminal here) — the
// fallback-target branch of BuildRetryBudgetCandidate.
func TestW3HealBuildRetryBudgetUsesFirstFailedTerminalFallback(t *testing.T) {
	base := w3RetryBaselineWithUpperTerminals()
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: "demo", EvidenceExecutionIDs: []string{"e1", "e2"}}
	proposal, cand, err := BuildRetryBudgetCandidate(base, trg, map[string]int{"impl": 4}, time.Now())
	if err != nil {
		t.Fatalf("BuildRetryBudgetCandidate: %v", err)
	}
	if proposal == nil || cand == nil {
		t.Fatal("expected proposal + candidate")
	}
	cw, err := registry.ParseWorkflowMarkdown([]byte(proposal.ProposalYAML), "demo.md")
	if err != nil {
		t.Fatalf("re-parse candidate: %v", err)
	}
	if got := cw.Steps["impl"].OnFail; got != "failed" {
		t.Errorf("impl.on_fail = %q, want failed (firstFailedTerminal fallback)", got)
	}
	if got := cw.Steps["impl"].RetryPolicy.MaxRetries; got != 2 {
		t.Errorf("impl maxRetries = %d, want 2 (4 halved)", got)
	}
}

// ---- BuildRetryBudgetCandidate / BuildVerifierInsertionCandidate edges -

// TestW3HealBuildRetryBudgetNilTriggerFallsBack: a nil trigger is a
// guard-clause fallback to the architect (ErrNoRecipeApplies), never a
// panic.
func TestW3HealBuildRetryBudgetNilTriggerFallsBack(t *testing.T) {
	if _, _, err := BuildRetryBudgetCandidate(parseRecipeBaseline(t), nil, map[string]int{"write": 3}, time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("nil trigger err = %v, want ErrNoRecipeApplies", err)
	}
}

// TestW3HealBuildRetryBudgetMissingStepFallsBack: the offending step
// inferred from the tally is absent from the genome (the genome shifted
// under the heuristic) → defer to the architect.
func TestW3HealBuildRetryBudgetMissingStepFallsBack(t *testing.T) {
	base := parseRecipeBaseline(t)
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: "dev-pipeline"}
	if _, _, err := BuildRetryBudgetCandidate(base, trg, map[string]int{"ghost-step": 9}, time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("missing step err = %v, want ErrNoRecipeApplies", err)
	}
}

// TestW3HealBuildVerifierInsertionNilArgsFallBack covers the guard
// clause: a nil baseline or nil trigger both defer to the architect
// rather than producing a half-built candidate.
func TestW3HealBuildVerifierInsertionNilArgsFallBack(t *testing.T) {
	base := parseRecipeBaseline(t)
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: base.ID}
	if _, _, err := BuildVerifierInsertionCandidate(nil, trg, map[string]int{"write": 3}, "verifier", time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("nil baseline err = %v, want ErrNoRecipeApplies", err)
	}
	if _, _, err := BuildVerifierInsertionCandidate(base, nil, map[string]int{"write": 3}, "verifier", time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("nil trigger err = %v, want ErrNoRecipeApplies", err)
	}
}

// TestW3HealBuildVerifierInsertionRecipeErrorFallsBack: the selected
// offending step has NO on_success target to gate (VerifierInsertionRecipe
// returns ErrRecipeNoChange) → the builder maps that to ErrNoRecipeApplies.
func TestW3HealBuildVerifierInsertionRecipeErrorFallsBack(t *testing.T) {
	base := parseRecipeBaseline(t)
	// Clear deploy's on_success so the recipe has nothing to gate.
	s := base.Steps["deploy"]
	s.OnSuccess = ""
	base.Steps["deploy"] = s
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: base.ID, EvidenceExecutionIDs: []string{"e1"}}
	if _, _, err := BuildVerifierInsertionCandidate(base, trg, map[string]int{"deploy": 5}, "verifier", time.Now()); !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("recipe-no-change err = %v, want ErrNoRecipeApplies", err)
	}
}

// TestW3HealBuildVerifierInsertionInvalidGenomeFallsBack: the recipe
// applies cleanly but the SERIALIZED candidate genome fails WORKFLOW.md
// validation (here: the baseline lacks a `description`, so the candidate
// trips description_missing → res.Valid == false). The builder must NOT
// ship an invalid candidate — it defers (ErrNoRecipeApplies), exercising
// the !res.Valid guard rather than the recipe-error guard.
func TestW3HealBuildVerifierInsertionInvalidGenomeFallsBack(t *testing.T) {
	// validWorkflowMD (genome_test.go) parses fine but has NO description,
	// which the validator flags as an ERROR on the serialized candidate.
	// Its "plan" step has on_success=complete, so the recipe APPLIES (no
	// ErrRecipeNoChange) — the only failure is validation.
	base, err := registry.ParseWorkflowMarkdown([]byte(validWorkflowMD), "heal.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	trg := &persistence.HealingTrigger{ID: "t", WorkflowID: base.ID, EvidenceExecutionIDs: []string{"e1"}}
	_, _, err = BuildVerifierInsertionCandidate(base, trg, map[string]int{"plan": 3}, "verifier", time.Now())
	if !errors.Is(err, ErrNoRecipeApplies) {
		t.Errorf("invalid-genome err = %v, want ErrNoRecipeApplies (must not ship an invalid candidate)", err)
	}
}

// ---- recipeProposalKind default arm -----------------------------------

// TestW3HealProposalFromRecipeUnknownClassIsUnspecified: a recipe result
// carrying an unrecognised candidate class maps to the Unspecified
// proposal kind (the default arm of recipeProposalKind), so an unknown
// class never silently masquerades as a known structural kind.
func TestW3HealProposalFromRecipeUnknownClassIsUnspecified(t *testing.T) {
	r := &RecipeResult{
		CandidateClass: persistence.HealingCandidateClass("totally-unknown"),
		ProposalDiff:   "x",
	}
	p := ProposalFromRecipeResult("demo", r, time.Now())
	if p == nil {
		t.Fatal("expected a proposal")
	}
	if p.Kind != persistence.WorkflowProposalKindUnspecified {
		t.Errorf("Kind = %q, want unspecified for an unknown class", p.Kind)
	}
}

// TestW3HealProposalFromRecipeConfidenceIsTransformationCertainty pins
// the documented invariant: Confidence==1.0 denotes certainty of the
// STRUCTURAL transformation, not a prediction the repair helps (the
// trial scorecard gates promotion, not this field).
func TestW3HealProposalFromRecipeConfidenceIsTransformationCertainty(t *testing.T) {
	r, err := RetryBudgetRecipe(parseRecipeBaseline(t), "write", 1, "failed", nil)
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	p := ProposalFromRecipeResult("dev-pipeline", r, time.Now())
	if p.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0 (transformation certainty)", p.Confidence)
	}
	if p.Status != persistence.WorkflowProposalStatusPending {
		t.Errorf("Status = %q, want pending (operator approval still required)", p.Status)
	}
}

// ---- replay trial fail-closed wiring (runReplay branches) -------------

// w3errRegistrar is a WorkflowRegistrar that fails RegisterTransient, to
// drive runReplay's "could not register candidate genome" errored branch.
type w3errRegistrar struct{ err error }

func (r *w3errRegistrar) RegisterTransient(string, *registry.Workflow) error { return r.err }
func (r *w3errRegistrar) DeregisterTransient(string)                         {}

// TestW3HealReplayErroredWhenRegistrarRejects: if the candidate genome
// cannot be registered for routing, the replay fails CLOSED (errored
// verdict) rather than silently replaying the baseline genome.
func TestW3HealReplayErroredWhenRegistrarRejects(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "completed", 0.1, 10)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.1, 10)
		eng.baseline["ev2"] = &v
	}

	r := NewTrialRunner(cands, trials, eng, DefaultGateThresholds(), 2, zerolog.Nop()).
		WithRegistrar(&w3errRegistrar{err: errors.New("register boom")})
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialErrored {
		t.Fatalf("verdict = %q, want errored (registrar rejected the candidate)", res.Verdict)
	}
	// The engine must NOT have been asked to replay — we failed before the loop.
	if len(eng.replayedFor) != 0 {
		t.Errorf("replayed %d evidence, want 0 (failed closed before routing)", len(eng.replayedFor))
	}
}

// TestW3HealReplayErroredWhenCandidateDiffUnparseable: a replay whose
// stored candidate diff cannot be parsed back into a genome cannot route
// at the candidate — errored, not a false pass. (Static catches this as
// FAILED; replay must catch it as ERRORED since it's a wiring failure,
// not a candidate-quality verdict.)
func TestW3HealReplayErroredWhenCandidateDiffUnparseable(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, "not a parseable workflow at all", "")

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "completed", 0.1, 10)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.1, 10)
		eng.baseline["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialErrored {
		t.Fatalf("verdict = %q, want errored (candidate diff unparseable for replay)", res.Verdict)
	}
}

// TestW3HealReplayErroredWithoutRegistrar: a replay engine wired WITHOUT
// a registrar cannot route at the candidate genome and must fail closed
// (errored), not replay the baseline. Built with NewTrialRunner directly
// (not newTestRunner, which auto-wires a registrar).
func TestW3HealReplayErroredWithoutRegistrar(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "completed", 0.1, 10)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.1, 10)
		eng.baseline["ev2"] = &v
	}

	r := NewTrialRunner(cands, trials, eng, DefaultGateThresholds(), 2, zerolog.Nop()) // NO registrar
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialErrored {
		t.Fatalf("verdict = %q, want errored (no registrar to route the candidate)", res.Verdict)
	}
}

// ---- promotion safety gate, end-to-end through a recipe ----------------

// TestW3HealRecipeCandidateStaticPassIsNonPromotable is the keystone
// safety assertion: a deterministic-recipe candidate that PASSES a
// static (shape-only) trial must NOT become promotable. Only a
// replay-gated pass advances to trial_passed; a static pass leaves the
// recipe candidate at draft so an operator can never promote a candidate
// that bypassed the quantitative gate entirely. This wires the WHOLE
// path: recipe → proposal/candidate synthesis → seeded candidate → static
// trial → non-promotable.
func TestW3HealRecipeCandidateStaticPassIsNonPromotable(t *testing.T) {
	base := parseRecipeBaseline(t)
	res, err := RetryBudgetRecipe(base, "write", 1, "failed", []string{"e1", "e2"})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	if !res.Valid {
		t.Fatalf("recipe candidate should validate clean; findings=%v", res.ValidationFindings)
	}
	now := time.Now()
	proposal := ProposalFromRecipeResult(base.ID, res, now)
	trg := &persistence.HealingTrigger{ID: "trg", ProjectID: "proj", WorkflowID: base.ID}
	cand := CandidateFromRecipeResult(trg, proposal, res)
	if cand == nil || proposal == nil {
		t.Fatal("synthesis produced nil")
	}

	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	cand.ID = persistence.GenerateID("whc")
	cand.WorkflowID = base.ID
	cands.put(cand)

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	tres, err := r.RunTrial(context.Background(), cand.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial(static): %v", err)
	}
	if tres.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("static verdict = %q, want passed (recipe genome is clean); reasons=%v", tres.Verdict, tres.Scorecard.Reasons)
	}
	// THE GATE: a static pass must NOT be promotable.
	if cands.lastStatus() != persistence.HealingCandidateDraft {
		t.Errorf("candidate status = %q, want draft (static pass is NON-promotable)", cands.lastStatus())
	}
	if cands.lastStatus() == persistence.HealingCandidateTrialPassed {
		t.Error("a static pass must never advance a recipe candidate to trial_passed")
	}
}

// TestW3HealRecipeCandidateGenomeHashRoundTrips: the synthesized
// proposal's WORKFLOW.md, re-parsed and re-hashed, MUST equal the
// candidate row's CandidateGenomeHash — otherwise the static trial's
// policy check (hash-consistency gate) would reject a genuine recipe
// candidate as internally inconsistent.
func TestW3HealRecipeCandidateGenomeHashRoundTrips(t *testing.T) {
	base := parseRecipeBaseline(t)
	res, err := RetryBudgetRecipe(base, "write", 1, "failed", nil)
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	proposal := ProposalFromRecipeResult(base.ID, res, time.Now())
	cand := CandidateFromRecipeResult(&persistence.HealingTrigger{ID: "t", WorkflowID: base.ID}, proposal, res)

	h, err := GenomeHashFromMarkdown([]byte(proposal.ProposalYAML), base.ID+".md")
	if err != nil {
		t.Fatalf("re-hash proposal: %v", err)
	}
	if h != cand.CandidateGenomeHash {
		t.Errorf("round-trip hash %q != candidate hash %q", h, cand.CandidateGenomeHash)
	}
	if !strings.HasPrefix(proposal.ID, "wpr_") {
		t.Errorf("proposal ID = %q, want wpr_ prefix", proposal.ID)
	}
}

// TestW3HealReplayGatedPassIsPromotable is the positive counterpart to
// the static-non-promotable gate: a recipe candidate that clears the
// quantitative replay gate DOES advance to trial_passed. Together the
// two tests pin both arms of the promotion safety rule.
func TestW3HealReplayGatedPassIsPromotable(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	// Strict improvement: baseline 1/2 success, candidate 2/2, cheaper.
	{
		v := traceWith("ev1", "failed", 0.20, 30)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.20, 30)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 15)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 15)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial(replay): %v", err)
	}
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("replay verdict = %q, want passed; reasons=%v", res.Verdict, res.Scorecard.Reasons)
	}
	if cands.lastStatus() != persistence.HealingCandidateTrialPassed {
		t.Errorf("candidate status = %q, want trial_passed (replay-gated pass IS promotable)", cands.lastStatus())
	}
}

// TestW3HealCandidateFromRecipeResultDenormalisesDiff: the candidate row
// carries a denormalised copy of the recipe's ProposalDiff (so the
// candidates UI + static trial don't have to re-fetch the proposal). Pin
// that it matches the recipe result exactly.
func TestW3HealCandidateFromRecipeResultDenormalisesDiff(t *testing.T) {
	base := parseRecipeBaseline(t)
	res, err := VerifierInsertionRecipe(base, "write", "verifier", "", []string{"e1"})
	if err != nil {
		t.Fatalf("recipe: %v", err)
	}
	p := ProposalFromRecipeResult(base.ID, res, time.Now())
	cand := CandidateFromRecipeResult(&persistence.HealingTrigger{ID: "t", ProjectID: "p", WorkflowID: base.ID}, p, res)
	if cand.ProposalDiff != res.ProposalDiff {
		t.Error("candidate ProposalDiff must denormalise the recipe diff")
	}
	if cand.ProposalDiff != p.ProposalYAML {
		t.Error("candidate diff and proposal YAML must agree (same source genome)")
	}
	if cand.CandidateClass != persistence.HealingCandidateVerifierInsertion {
		t.Errorf("class = %q, want verifier_insertion", cand.CandidateClass)
	}
	if cand.RiskLevel != persistence.HealingRiskMedium {
		t.Errorf("risk = %q, want medium (verifier insertion adds a step)", cand.RiskLevel)
	}
}

// TestW3HealSelectOffendingStepIgnoresNegativeTallies: a defensive edge —
// negative counts (should never occur, but guard against it) are treated
// as "no failure" and never selected.
func TestW3HealSelectOffendingStepIgnoresNegativeTallies(t *testing.T) {
	step, ok := SelectOffendingStep(map[string]int{"a": -3, "b": 2})
	if !ok || step != "b" {
		t.Errorf("got (%q,%v), want (b,true) — negatives must not win", step, ok)
	}
	if _, ok := SelectOffendingStep(map[string]int{"only": -1}); ok {
		t.Error("an all-negative tally must select nothing")
	}
}
