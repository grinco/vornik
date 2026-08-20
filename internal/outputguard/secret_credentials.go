package outputguard

import (
	"fmt"
	"regexp"
	"sync/atomic"

	"vornik.io/vornik/internal/secrets"
)

// Credential rules are the secret-class rules that detect leaked API keys and
// tokens in tool-result content on its way to the model.
//
// They are NOT written here. They are compiled from internal/secrets' pattern
// corpus, because that corpus is already the single definition of "what a
// credential looks like" for every persistence checkpoint. Before 2026-08-20
// outputguard had no credential rule at all — its secret class was exactly
// KindAdversarialURL and KindEncodedPayload — so a Google API key scraped by
// mcp__scraper__web_fetch reached the model uncleaned and was caught only
// downstream, when the model echoed it into result.json. The two packages held
// one vocabulary and only one of them had it; hand-copying the regexes here
// would have made that permanent instead of fixing it.
//
// Only STRONG (prefix-anchored) patterns are enforced. The heuristic class —
// generic_kv and entropy — is deliberately excluded: outputguard redacts what
// the MODEL sees, so a false positive corrupts the agent's working data rather
// than merely dirtying an audit row. Entropy-shaped case ids (s1_case_1) doing
// exactly that to result.json is a live incident, not a hypothetical. The
// at-rest scan on the persistence path still applies the full corpus; this is
// the narrower, higher-confidence subset for the read-back path.
//
// The finding Kind is the pattern NAME, so the redaction marker reads
// [REDACTED:google_api_key] — the same typed shape internal/secrets emits, and
// enough for an operator to tell what was removed without seeing it.

// credentialRules is swapped atomically by SetCredentialPatterns. The base
// `rules` table is read-only after init; this one is configurable at boot
// because the operator's disable/custom lists must reach it. Readers load the
// pointer once per scan, so a concurrent swap can never tear a rule set.
var credentialRules atomic.Pointer[[]rulePattern]

func init() {
	// Defaults, so a caller that never boots the daemon (tests, CE paths) still
	// gets credential coverage rather than an empty set.
	if err := SetCredentialPatterns(secrets.StrongPatterns(secrets.DefaultPatterns())); err != nil {
		panic("outputguard: default credential patterns do not compile: " + err.Error())
	}
}

// SetCredentialPatterns compiles patterns into the credential rule set and
// swaps it in atomically.
//
// Callers pass the corpus they are entitled to — the daemon passes
// secrets.StrongPatterns(secrets.EffectivePatterns(disable, custom)), the same
// corpus buildSecretsDetector compiles — so a pattern the operator disabled
// stays disabled here instead of reappearing through a second path.
//
// A pattern that does not compile is an error and the existing rule set is left
// untouched: a failed swap must never silently disarm the guard.
func SetCredentialPatterns(patterns []secrets.Pattern) error {
	compiled := make([]rulePattern, 0, len(patterns))
	for _, p := range patterns {
		if secrets.IsHeuristicType(p.Name) {
			// Defence in depth: callers are expected to filter with
			// StrongPatterns, but the exclusion is a security property of THIS
			// path and should not depend on every caller remembering it.
			continue
		}
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return fmt.Errorf("compile credential pattern %q: %w", p.Name, err)
		}
		compiled = append(compiled, rulePattern{
			kind:     Kind(p.Name),
			severity: SeverityHigh,
			class:    classSecret,
			re:       re,
		})
	}
	credentialRules.Store(&compiled)
	return nil
}

// activeCredentialRules returns the current rule set, never nil.
func activeCredentialRules() []rulePattern {
	if p := credentialRules.Load(); p != nil {
		return *p
	}
	return nil
}
