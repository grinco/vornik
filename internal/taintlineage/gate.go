package taintlineage

import "strings"

// Mode is the per-project taint enforcement tri-state (D4), mirroring
// memoryfirewall.EnforcementMode's shape.
type Mode string

const (
	// ModeOff — no gate query, no park/refuse. Audit is still recorded on the
	// row by the executor (audit is always-on, §9 invariant 2).
	ModeOff Mode = "off"
	// ModeAdvisory (default) — taint is flagged everywhere, but no task is
	// parked/refused (park=false always).
	ModeAdvisory Mode = "advisory"
	// ModeEnforce — a tainted write parks (forge) or is refused (query_api),
	// failing closed on an incomplete walk (D6) and on Unknown tools (D8).
	ModeEnforce Mode = "enforce"
)

// NormalizeMode validates an operator-supplied mode string. Unlike the
// firewall's coerce-to-advisory helper, the DAEMON loader path wants a hard
// error on an invalid value (§7 — matching how agent_writes validates its
// tri-state); ok=false signals that. Empty ≡ advisory (the default).
func NormalizeMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "advisory":
		return ModeAdvisory, true
	case "off":
		return ModeOff, true
	case "enforce":
		return ModeEnforce, true
	default:
		return ModeAdvisory, false
	}
}

// EffectiveMode resolves the effective mode for a project: a non-empty project
// override wins; otherwise the daemon default. Invalid strings coerce to the
// closest valid value (advisory) — the daemon default was already hard-validated
// at load, and an operator typo in a project override should fail safe, not
// crash the daemon (§7).
func EffectiveMode(projectOverride, daemonDefault string) Mode {
	if strings.TrimSpace(projectOverride) != "" {
		m, _ := NormalizeMode(projectOverride)
		return m
	}
	m, _ := NormalizeMode(daemonDefault)
	return m
}

// Decision is the resolved taint gate outcome for one write (§4.4).
type Decision struct {
	Mode           Mode
	Tainted        bool
	RequiresReview bool // High present, OR (enforce) Unknown present, OR (enforce) walk incomplete
	WalkComplete   bool
	Park           bool // enforce && (walk incomplete || (content review && no matching latch))
	SourceSetHash  string
	SourceCount    int      // FULL pre-cap distinct count (for the checkpoint / metric)
	ShownCount     int      // len(rollup.Sources) — surfaced truncation (F-cap)
	Sources        []Source // lineage-scoped display list
}

// Decide applies the canonical D8 park formula. Pure — the caller supplies the
// already-fetched rollup and the recorded latch hashes for this task. This is
// the single tested home of the park/refuse decision + the D7 latch match
// (F1): the latch suppresses ONLY the content review, never the incomplete-walk
// park.
//
//	contentReview = RequiresReview || HasUnknown      // suppressible by the latch
//	walkReview    = !WalkComplete                     // NEVER suppressible (D6)
//	park          = walkReview || (contentReview && !latchMatches)
func Decide(mode Mode, roll TaskTaint, latchHashes []string) Decision {
	d := Decision{
		Mode:          mode,
		Tainted:       roll.Tainted,
		WalkComplete:  roll.WalkComplete,
		SourceSetHash: roll.SourceSetHash,
		SourceCount:   roll.TotalSources,
		ShownCount:    len(roll.Sources),
		Sources:       roll.Sources,
	}
	switch mode {
	case ModeOff:
		// No enforcement. (Callers short-circuit before even querying in off;
		// this branch keeps Decide total.)
		return d
	case ModeEnforce:
		contentReview := roll.RequiresReview || roll.HasUnknown
		walkReview := !roll.WalkComplete
		d.RequiresReview = contentReview || walkReview
		latched := latchMatches(roll.SourceSetHash, latchHashes)
		d.Park = walkReview || (contentReview && !latched)
		return d
	default: // advisory
		// Flag only; never park. RequiresReview reflects the STORED semantics
		// (High present) so advisory metrics stay quiet on Unknown-only.
		d.RequiresReview = roll.RequiresReview
		d.Park = false
		return d
	}
}

// latchMatches reports whether any recorded latch hash equals the current
// lineage source-set hash. An empty current hash (no sources) never matches a
// recorded latch — there is nothing to have reviewed.
func latchMatches(current string, recorded []string) bool {
	if current == "" {
		return false
	}
	for _, h := range recorded {
		if h == current {
			return true
		}
	}
	return false
}
