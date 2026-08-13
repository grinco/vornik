package executor

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
// It also makes §13.3(4)'s advisory/invariant declaration a fact in code rather than
// a sentence in a design doc — the distinction decides what an operator may suppress,
// so it needs somewhere to live that a future selector can read.
type injectedBlockClass string

const (
	// blockAdvisory helps an agent use a capability well. An operator may suppress it
	// for their swarm (the knob is not built yet; it is tracked in the backlog as
	// "Advisory-block suppression"). Nobody may reword it.
	blockAdvisory injectedBlockClass = "advisory"
	// blockInvariant states a rule the daemon enforces whatever the prompt says.
	// Neither suppressible nor rewordable: rewording it cannot change enforcement, it
	// only lets a deployment misdescribe a rule it is still subject to.
	blockInvariant injectedBlockClass = "invariant"
)

// injectedBlock is one registered block. Text is the literal appended to the prompt;
// Const is the identifier declaring it, which the completeness law matches against.
type injectedBlock struct {
	Name  string
	Const string
	Class injectedBlockClass
	Text  string
}

// injectedBlocks is every STATIC guidance block this binary can inject.
//
// The learned-skill index is deliberately absent: it is rendered per execution from
// operator-approved skills rather than declared as a constant, so it has no fixed
// size to budget. Its cost is bounded by the skill store, not by this ceiling.
var injectedBlocks = []injectedBlock{
	{
		Name:  "canonical-context",
		Const: "canonicalContextSystemPromptBlock",
		Class: blockAdvisory,
		Text:  canonicalContextSystemPromptBlock,
	},
	{
		Name:  "tool-budget",
		Const: "toolGrantSystemPromptBlock",
		Class: blockAdvisory,
		Text:  toolGrantSystemPromptBlock,
	},
	{
		Name:  "reporting-integrity",
		Const: "claimVerificationSystemPromptBlock",
		Class: blockInvariant,
		Text:  claimVerificationSystemPromptBlock,
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
