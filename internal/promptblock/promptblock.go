// Package promptblock declares the daemon-authored guidance blocks that can be
// injected into an agent's system prompt, and the CLASS that decides what an
// operator is allowed to switch off (LLD 09 §13.3(4), §13.3.1).
//
// WHY THIS IS ITS OWN PACKAGE. The block TEXT lives in internal/executor next to
// the composition that emits it, which is where it belongs. But the suppression
// knob is a swarm config field, so internal/registry has to validate an operator's
// list at load time — and registry cannot import executor. A leaf package holding
// only the names and their classes gives both sides one authority to read, instead
// of a second copy of the classification in a validator that could drift from the
// blocks it is classifying.
//
// The class is the load-bearing part:
//
//   - ADVISORY blocks help an agent use a capability well (canonical-context
//     discovery, tool budget). An operator who finds one redundant for their swarm
//     may suppress it. Nobody may reword it.
//   - INVARIANT blocks state a rule the daemon enforces whatever the prompt says
//     (reporting integrity: verifyRoleClaims runs on every step of every
//     deployment). Neither suppressible nor rewordable — suppressing one removes
//     the WARNING and not the RULE, leaving a deployment that misdescribes what its
//     agents are subject to.
//
// A class is a property of the BLOCK, fixed at definition. Selection — by an
// operator here, by an instinct or a future optimiser later — toggles whether a
// block is included; it never promotes or demotes one (LLD 09 §13.7 invariant 4).
package promptblock

import "sort"

// Class is a block's suppressibility classification.
type Class string

const (
	// Advisory blocks may be suppressed per swarm.
	Advisory Class = "advisory"
	// Invariant blocks may not: the rule runs regardless of the prompt.
	Invariant Class = "invariant"
)

// Block names. These are the identifiers an operator writes in a swarm's
// suppressedGuidanceBlocks list, so they are part of the config contract —
// renaming one silently breaks every config that names it.
const (
	CanonicalContext   = "canonical-context"
	ToolBudget         = "tool-budget"
	ReportingIntegrity = "reporting-integrity"
)

// classByName is the declaration. internal/executor's registry pairs each of
// these names with its text and its composition seam, and a law there requires
// the two sets to match exactly in both directions.
var classByName = map[string]Class{
	CanonicalContext:   Advisory,
	ToolBudget:         Advisory,
	ReportingIntegrity: Invariant,
}

// ClassOf returns a block's class, and whether the name is known at all.
func ClassOf(name string) (Class, bool) {
	c, ok := classByName[name]
	return c, ok
}

// Known reports whether name refers to a block this binary can inject.
func Known(name string) bool {
	_, ok := classByName[name]
	return ok
}

// Suppressible reports whether an operator may switch this block off. False for
// an unknown name as well as for an invariant one — the caller distinguishes the
// two (an unknown name is a typo or a block from another release; an invariant
// one is a request the daemon will never honour).
func Suppressible(name string) bool {
	c, ok := classByName[name]
	return ok && c == Advisory
}

// Names returns every known block name, sorted, for error messages that need to
// tell an operator what they could have written instead.
func Names() []string {
	out := make([]string, 0, len(classByName))
	for name := range classByName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SuppressibleNames returns the advisory block names, sorted.
func SuppressibleNames() []string {
	out := make([]string, 0, len(classByName))
	for name, c := range classByName {
		if c == Advisory {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
