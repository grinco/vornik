package workflowhealing

// Deterministic candidate recipes — Self-Healing Workflow Genome v1.
//
// A recipe is a bounded, STRUCTURAL transformation of a workflow
// genome that addresses one obvious regression class without an LLM
// call. The LLD (§ Candidate Generator) calls out two recipes for v1:
//
//   - retry_loop          → lower a step's retry budget + add a
//                           failure transition (RetryBudgetRecipe)
//   - verifier_failures   → insert an explicit verifier step ahead of
//                           the reviewer (VerifierInsertionRecipe)
//
// Recipes NEVER rewrite prompt bodies (same scope ceiling the memetic
// architect enforces). They only touch structural fields: retry
// budgets, transitions, and step graph wiring. Each recipe returns a
// RecipeResult carrying everything the LLD requires of a candidate —
// diff, expected effect, risk level, evidence IDs, and a validation
// result — plus the parsed candidate genome so the caller can compute
// the candidate_genome_hash and link the row to a WorkflowProposal.
//
// A recipe that cannot apply (target step missing, nothing to change)
// returns a sentinel error rather than emitting a no-op candidate, so
// the caller can fall back to the architect.

import (
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

var (
	// ErrRecipeStepNotFound is returned when the named step the recipe
	// targets is absent from the baseline genome.
	ErrRecipeStepNotFound = errors.New("workflowhealing: recipe target step not found")
	// ErrRecipeNoChange is returned when the recipe would produce a
	// genome identical to the baseline (nothing to lower / nothing to
	// insert) — emitting it would be a no-op candidate.
	ErrRecipeNoChange = errors.New("workflowhealing: recipe produced no structural change")
	// ErrRecipeNilWorkflow guards a nil baseline genome.
	ErrRecipeNilWorkflow = errors.New("workflowhealing: baseline workflow is nil")
)

// retryBudgetFloor is the lowest retry budget a recipe will propose.
// Dropping to 0 retries is legitimate (fail fast, route on on_fail),
// so the floor is 0 — the recipe simply refuses to RAISE a budget.
const retryBudgetFloor = 0

// RecipeResult is the structural-only output of a deterministic
// recipe. It carries the full candidate WORKFLOW.md (ProposalDiff —
// the canonical serialized genome, not a unified diff) plus the
// metadata the candidate ledger and promotion gate need. The caller
// links this to a WorkflowProposal and persists a HealingCandidate.
type RecipeResult struct {
	// CandidateClass tags which recipe produced this result.
	CandidateClass persistence.HealingCandidateClass
	// CandidateWorkflow is the mutated, re-parsed genome. Its Hash is
	// the candidate_genome_hash. Never nil on a successful recipe.
	CandidateWorkflow *registry.Workflow
	// BaselineGenomeHash / CandidateGenomeHash fingerprint the genome
	// before and after the mutation (registry.Workflow.Hash).
	BaselineGenomeHash  string
	CandidateGenomeHash string
	// ProposalDiff is the full canonical WORKFLOW.md text of the
	// candidate genome (MarshalWorkflowMarkdown output). The UI renders
	// it; the trial runner re-parses it. Named "diff" to match the LLD
	// candidate data model, though it is the whole document.
	ProposalDiff string
	// Motivation explains WHY this structural change addresses the
	// regression. Operator-facing.
	Motivation string
	// ExpectedEffect states the intended outcome (LLD: "expected
	// effect"). Operator-facing.
	ExpectedEffect string
	// RiskLevel is the blast-radius banner.
	RiskLevel persistence.HealingRiskLevel
	// EvidenceExecutionIDs are the executions that justify the recipe
	// (carried through from the trigger).
	EvidenceExecutionIDs []string
	// Valid reports whether the candidate genome passed WORKFLOW.md
	// validation. ValidationFindings carries the human-readable
	// ERROR-severity messages when Valid is false. A recipe that
	// produces an invalid genome still returns the result (with
	// Valid=false) so the operator sees WHY it was rejected, rather
	// than swallowing it as a generic error.
	Valid              bool
	ValidationFindings []string
}

// RetryBudgetRecipe addresses a retry_loop regression on one step by
// lowering its retry budget to newBudget AND ensuring it has an
// on_fail transition (so an exhausted retry routes to a failure step
// instead of looping or hard-failing the whole execution). When the
// step already has no retries and an on_fail target, there is nothing
// to change and the recipe returns ErrRecipeNoChange.
//
// Structural only: it touches RetryPolicy.MaxRetries and OnFail. It
// never edits the prompt. newBudget is clamped to [retryBudgetFloor,
// current] — the recipe only LOWERS the budget, never raises it
// (raising a budget would worsen a retry loop). onFailTarget is the
// step the exhausted retry routes to; when empty and the step lacks an
// on_fail, the recipe is a no-op on the transition (budget-only).
func RetryBudgetRecipe(baseline *registry.Workflow, stepID string, newBudget int, onFailTarget string, evidenceIDs []string) (*RecipeResult, error) {
	if baseline == nil {
		return nil, ErrRecipeNilWorkflow
	}
	step, ok := baseline.Steps[stepID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRecipeStepNotFound, stepID)
	}

	candidate := cloneWorkflow(baseline)
	cstep := candidate.Steps[stepID]

	// Lower-only clamp. Never raise the budget; refuse anything above
	// the current value by capping at the current budget.
	target := newBudget
	if target < retryBudgetFloor {
		target = retryBudgetFloor
	}
	if target > cstep.RetryPolicy.MaxRetries {
		target = cstep.RetryPolicy.MaxRetries
	}

	changedBudget := target != cstep.RetryPolicy.MaxRetries
	cstep.RetryPolicy.MaxRetries = target

	// Ensure a failure transition exists so an exhausted retry routes
	// cleanly. Only set it when the step lacks one and the caller named
	// a target — never overwrite an existing on_fail (operator intent).
	changedTransition := false
	if cstep.OnFail == "" && onFailTarget != "" {
		if _, exists := baseline.Steps[onFailTarget]; !exists {
			if _, isTerminal := baseline.Terminals[onFailTarget]; !isTerminal {
				return nil, fmt.Errorf("%w: on_fail target %q is neither a step nor a terminal", ErrRecipeStepNotFound, onFailTarget)
			}
		}
		cstep.OnFail = onFailTarget
		changedTransition = true
	}

	if !changedBudget && !changedTransition {
		return nil, ErrRecipeNoChange
	}
	candidate.Steps[stepID] = cstep

	motivation := fmt.Sprintf(
		"Step %q showed a retry-loop regression. Lowering its retry budget from %d to %d caps the wasted reattempts; ",
		stepID, step.RetryPolicy.MaxRetries, target)
	if changedTransition {
		motivation += fmt.Sprintf("routing exhausted retries to %q via on_fail surfaces the failure to the recovery path instead of silently re-looping.", onFailTarget)
	} else {
		motivation += "the step already has a failure transition, so the budget cut alone removes the loop."
	}
	expected := fmt.Sprintf(
		"Fewer retries on %q; an exhausted attempt fails fast and routes on on_fail rather than re-entering the loop. Success rate should hold while cost and latency drop.",
		stepID)

	return finalizeRecipe(baseline, candidate, persistence.HealingCandidateRetryBudget,
		motivation, expected, persistence.HealingRiskLow, evidenceIDs)
}

// VerifierInsertionRecipe addresses a verifier_failures (or
// hallucination) regression by inserting an explicit verifier step
// between afterStep and afterStep's current on_success target. The new
// step runs the verifierRole, inherits the original on_success target,
// and afterStep is rewired to transition into the verifier first:
//
//	afterStep --(on_success)--> verifier --(on_success)--> <original target>
//
// This is the LLD's "verifier failures after a writer step → propose
// explicit verifier step before reviewer" recipe. Structural only: it
// adds a step and re-wires one transition. The inserted step carries a
// minimal non-prompt-rewriting placeholder prompt (a fixed, generic
// instruction) — the recipe does NOT author or rewrite any existing
// prompt body.
//
// Returns ErrRecipeStepNotFound when afterStep is absent, and
// ErrRecipeNoChange when afterStep has no on_success target to gate
// (nothing to insert before).
func VerifierInsertionRecipe(baseline *registry.Workflow, afterStep, verifierRole, verifierStepID string, evidenceIDs []string) (*RecipeResult, error) {
	if baseline == nil {
		return nil, ErrRecipeNilWorkflow
	}
	anchor, ok := baseline.Steps[afterStep]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRecipeStepNotFound, afterStep)
	}
	if anchor.OnSuccess == "" {
		return nil, fmt.Errorf("%w: step %q has no on_success target to gate", ErrRecipeNoChange, afterStep)
	}
	if verifierStepID == "" {
		verifierStepID = afterStep + "_verify"
	}
	if _, clash := baseline.Steps[verifierStepID]; clash {
		return nil, fmt.Errorf("workflowhealing: verifier step id %q already exists", verifierStepID)
	}
	if verifierRole == "" {
		return nil, fmt.Errorf("workflowhealing: verifierRole is required")
	}

	candidate := cloneWorkflow(baseline)
	originalTarget := anchor.OnSuccess

	// The inserted verifier inherits the anchor's success target and a
	// fixed structural prompt. This prompt is a CONSTANT generic
	// instruction — it is not derived from, nor does it rewrite, any
	// existing step's prompt body.
	verifier := registry.WorkflowStep{
		Type:      "agent",
		Role:      verifierRole,
		Prompt:    "Verify the prior step's output is correct, grounded in the available evidence, and free of unsupported claims before allowing the workflow to proceed. Fail the step if verification does not pass.",
		OnSuccess: originalTarget,
		OnFail:    anchor.OnFail,
	}
	candidate.Steps[verifierStepID] = verifier

	// Rewire the anchor to gate through the verifier first.
	cAnchor := candidate.Steps[afterStep]
	cAnchor.OnSuccess = verifierStepID
	candidate.Steps[afterStep] = cAnchor

	motivation := fmt.Sprintf(
		"Verifier/hallucination failures were observed downstream of step %q. Inserting an explicit %q verifier step between %q and %q gates the output at a checkpoint, catching unsupported or incorrect results before they propagate.",
		afterStep, verifierRole, afterStep, originalTarget)
	expected := fmt.Sprintf(
		"Output of %q is verified before reaching %q; hallucination / verifier-failure rate should drop. Adds one agent step (latency + cost) per run.",
		afterStep, originalTarget)

	// Inserting a step + an extra LLM call is a wider blast radius than
	// a budget tweak — medium risk.
	return finalizeRecipe(baseline, candidate, persistence.HealingCandidateVerifierInsertion,
		motivation, expected, persistence.HealingRiskMedium, evidenceIDs)
}

// finalizeRecipe serializes the candidate genome to WORKFLOW.md,
// validates it, computes both genome hashes, and assembles the
// RecipeResult. A validation failure is recorded in the result
// (Valid=false + findings) rather than returned as an error — the
// operator should see why a structurally-derived candidate was
// rejected. A serialization failure IS a Go error (the recipe built a
// genome the marshaller can't render — a bug, not operator content).
func finalizeRecipe(baseline, candidate *registry.Workflow, class persistence.HealingCandidateClass, motivation, expected string, risk persistence.HealingRiskLevel, evidenceIDs []string) (*RecipeResult, error) {
	md, err := registry.MarshalWorkflowMarkdown(candidate)
	if err != nil {
		return nil, fmt.Errorf("workflowhealing: serialize candidate genome: %w", err)
	}
	filename := candidate.ID + ".md"

	res := &RecipeResult{
		CandidateClass:       class,
		CandidateWorkflow:    candidate,
		BaselineGenomeHash:   GenomeHash(baseline),
		CandidateGenomeHash:  GenomeHash(candidate),
		ProposalDiff:         string(md),
		Motivation:           motivation,
		ExpectedEffect:       expected,
		RiskLevel:            risk,
		EvidenceExecutionIDs: append([]string(nil), evidenceIDs...),
	}

	report := registry.ValidateWorkflowMarkdown(md, filename)
	if report.HasErrors() {
		res.Valid = false
		for _, f := range report.Findings {
			if f.Severity == registry.SeverityError {
				res.ValidationFindings = append(res.ValidationFindings, f.Code+": "+f.Message)
			}
		}
		return res, nil
	}
	res.Valid = true
	return res, nil
}

// cloneWorkflow returns a deep-enough copy of wf for a recipe to
// mutate structural fields without aliasing the caller's genome. The
// Steps and Terminals maps are copied (WorkflowStep is a value type
// with no pointer-shared mutable state the recipes touch — RetryPolicy
// is a value struct, transitions are strings). Slice fields the
// recipes don't touch (CleanupArtifacts) are shared, which is safe
// because recipes never mutate them.
func cloneWorkflow(wf *registry.Workflow) *registry.Workflow {
	out := *wf
	if wf.Steps != nil {
		out.Steps = make(map[string]registry.WorkflowStep, len(wf.Steps))
		for k, v := range wf.Steps {
			out.Steps[k] = v
		}
	}
	if wf.Terminals != nil {
		out.Terminals = make(map[string]registry.WorkflowTerminal, len(wf.Terminals))
		for k, v := range wf.Terminals {
			out.Terminals[k] = v
		}
	}
	return &out
}
