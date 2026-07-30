package dispatcher

import "strings"

// Scope resolution for `remember` — SLICE 2 of the chat memory-write design §5.2.
//
// Revision 1 routed by SUBJECT: facts about a person to the operator profile, facts about
// the work to project memory. Review round 1 rejected that — it required perfect
// classification on every write ("Alice prefers dark mode" is both a personal preference and
// a work configuration), and misrouting personal data into shared scope cannot be
// retroactively contained. It also contradicted the DM rule two sections later.
//
// So scope comes from INSTRUCTION, never from content, and the default is the narrower one.
//
// Revision 3 withdrew the claim that "the model never infers shared scope" — that was a
// behavioural assertion with no enforcement, and a prompt injection or model regression can
// still emit a shared-scope call. What is claimed instead: routing is model-gated, and the
// confirmation step (§5.3, slice 3) is the containment. A wrongly-proposed shared write
// becomes a confirmation prompt, not a leak.
//
// This file is therefore fail-safe rather than authoritative: it decides what the model
// ASKED for, and anything it cannot recognise resolves to personal.
type memoryScope string

const (
	// memoryScopePersonal writes to the operator profile: per-person, already scoped, and
	// the default for everything.
	memoryScopePersonal memoryScope = "personal"
	// memoryScopeShared writes to project memory, readable by every participant and by
	// every task in the project. Only ever reached by an explicit signal.
	memoryScopeShared memoryScope = "shared"
)

// sharedScopeSignals is the recognised vocabulary for asking for shared scope (§5.2).
//
// Enumerated rather than pattern-matched on purpose: a fuzzy match is how "for me" would
// eventually be read as "for everyone". Anything not on this list — including silence, and
// including plausible-but-unlisted phrasings — is personal.
var sharedScopeSignals = map[string]bool{
	"shared":          true,
	"team":            true,
	"project":         true,
	"everyone":        true,
	"for the team":    true,
	"for everyone":    true,
	"for us":          true,
	"for the project": true,
}

// resolveMemoryScope maps the tool's scope argument to a scope.
//
// FAIL-SAFE: unset, unrecognised, or malformed all resolve to personal. The asymmetry is
// deliberate — a wrongly-personal write is a mild annoyance the user can repeat with clearer
// wording, while a wrongly-shared write is a confidentiality breach that cannot be
// retroactively contained (§9 one-way door 3).
func resolveMemoryScope(raw string) memoryScope {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return memoryScopePersonal
	}
	if sharedScopeSignals[s] {
		return memoryScopeShared
	}
	return memoryScopePersonal
}
