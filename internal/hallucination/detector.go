package hallucination

// Rule is the unit of detection. Each rule is a pure function
// over (text, grounding) so the detector itself is trivially
// composable: register a rule, and Scan fans out.
//
// Signals returned by all rules are concatenated. The detector
// makes no claim about ordering — the UI is expected to sort
// by Severity for display, and the executor cares only about
// "any High?" not "which High came first?".
type Rule func(text string, gc *GroundingContext) []Signal

// Detector composes rules and runs them. Construction goes
// through New() rather than struct literal so callers can swap
// in a different rule set (e.g. the dispatcher uses a slimmer
// list than the executor) without each callsite naming every
// rule explicitly.
type Detector struct {
	rules []Rule
}

// NewDefault returns the standard detector wired with the
// rules suitable for both the executor and the dispatcher.
// Specific surfaces can build a narrower detector via New() if
// their grounding context is too thin to support some rules.
func NewDefault() *Detector {
	return New([]Rule{
		urlNotFetchedRule,
		taskIDNotFoundRule,
		projectIDNotFoundRule,
		artifactNotProducedRule,
		numericClaimMismatchRule,
		taIndicatorClaimRule,
		hallucinatedToolFormatRule,
		schemaLeakageRule,
	})
}

// New constructs a detector with an explicit rule list. Rules
// are evaluated in slice order; ordering is observable only to
// the extent that the returned Signal slice preserves it (the
// UI doesn't rely on it).
func New(rules []Rule) *Detector {
	return &Detector{rules: rules}
}

// Scan runs every rule against text and returns the merged
// signals. Nil context is handled gracefully — returns nil.
//
// Empty text is NOT short-circuited: some rules
// (hallucinated_tool_format) operate purely over the grounding
// context's tool-audit set and need to fire even when the
// agent's prose is blank — degenerate-loop tasks frequently
// produce empty final messages because the model burned every
// iteration on hallucinated tool calls. Existing prose-scanning
// rules degrade naturally (no text → no matches → no signal).
func (d *Detector) Scan(text string, gc *GroundingContext) []Signal {
	if d == nil || len(d.rules) == 0 || gc == nil {
		return nil
	}
	var all []Signal
	for _, r := range d.rules {
		if sigs := r(text, gc); len(sigs) > 0 {
			all = append(all, sigs...)
		}
	}
	return all
}

// ShouldBlock answers "given these signals, should the producer
// step fail and retry?". Currently equivalent to "any High
// severity is present", but kept as a method on Detector so a
// future per-rule severity threshold map can land here without
// touching callers.
func (d *Detector) ShouldBlock(signals []Signal) bool {
	for _, s := range signals {
		if s.Severity.Block() {
			return true
		}
	}
	return false
}
