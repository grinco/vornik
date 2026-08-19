package executor

import "vornik.io/vornik/internal/promptblock"

// Registry of the static capability-guidance blocks injected into agent system
// prompts (LLD 09 §13).
//
// WHY A REGISTRY AND NOT THREE LOOSE CONSTANTS. The aggregate byte budget is the
// bound that actually binds (§13.3(2)) — N blocks each inside its own ceiling still
// compose one large prompt. A test that sums three hand-named constants does not
// enforce that: a fourth block added without touching the test passes CI while the
// real prompt grows. The budget test sums THIS slice, and
// TestInjectedBlockRegistry_IsComplete reads the package source to assert every
// declared block constant appears here, so a new block cannot be both injected and
// unbudgeted.
//
// WHERE THE CLASS LIVES. §13.3(4)'s advisory/invariant declaration is a fact in code
// rather than a sentence in a design doc, because it decides what an operator may
// suppress. It sits in internal/promptblock rather than here: the suppression knob is
// a swarm config field, so internal/registry validates an operator's list at load
// time and cannot import this package. Names + classes there, text + seam here, with
// TestInjectedBlockRegistry_MatchesPromptblockDeclaration requiring the two sets to
// match in both directions.
//
// injectedBlock is one registered block. Text is the literal appended to the prompt;
// Const is the identifier declaring it, which the completeness law matches against;
// Name is promptblock's identifier for it, and the string an operator writes in
// suppressedGuidanceBlocks.
type injectedBlock struct {
	Name  string
	Const string
	Text  string
}

// injectedBlocks is every STATIC guidance block this binary can inject.
//
// The learned-skill index is deliberately absent: it is rendered per execution from
// operator-approved skills rather than declared as a constant, so it has no fixed
// size to budget. Its cost is bounded by the skill store, not by this ceiling.
var injectedBlocks = []injectedBlock{
	{
		Name:  promptblock.CanonicalContext,
		Const: "canonicalContextSystemPromptBlock",
		Text:  canonicalContextSystemPromptBlock,
	},
	{
		Name:  promptblock.ToolBudget,
		Const: "toolGrantSystemPromptBlock",
		Text:  toolGrantSystemPromptBlock,
	},
	{
		Name:  promptblock.ReportingIntegrity,
		Const: "claimVerificationSystemPromptBlock",
		Text:  claimVerificationSystemPromptBlock,
	},
	{
		Name:  promptblock.WorkspaceGit,
		Const: "workspaceGitSystemPromptBlock",
		Text:  workspaceGitSystemPromptBlock,
	},
}

// injectedBlocksAggregateBytes is the total size of every static block, i.e. the
// worst case a single prompt can carry from this registry.
//
// ENFORCED AT BUILD TIME, DELIBERATELY NOT AT RUNTIME. Composition does not reject an
// over-budget block set, because the only thing it could do about one is drop a block
// — silently withholding guidance an agent needs, mid-execution, to save bytes. That
// trades a known cost for an unknown behaviour change, and it would make the reporting-
// integrity invariant conditional on how many other blocks happened to be present. So
// the ceiling is a CI gate over authored constants (whose sizes are fixed at compile
// time and therefore fully knowable there), and the runtime always emits what it was
// built with.
func injectedBlocksAggregateBytes() int {
	total := 0
	for _, b := range injectedBlocks {
		total += len(b.Text)
	}
	return total
}
