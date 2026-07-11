package projectwizard

import (
	"strconv"
	"strings"
	"unicode"
)

// anchorConfidenceThreshold is the anchorScore above which the server
// refuses tier-3 output and forces the cheaper template-anchored path
// instead (design §5.1). Chosen empirically for v1: two or three
// shared distinguishing words (project domain + a couple of nouns)
// between the operator's description and a prior's blurb reliably
// clears it, while a generic one-word overlap ("news", "report")
// does not. Documented here rather than tuned via a config knob
// because the heuristic itself (see anchorScore) is the thing that
// should evolve, not just this number.
const anchorConfidenceThreshold = 0.30

// tier3ConfirmSentinel is embedded (not necessarily verbatim shown to
// the operator, but present) in the plain-language message the server
// sends when it fails closed after a corrective retry still emitted
// tier-3. detectTier3Unlock looks for the assistant's PRIOR turn
// carrying this sentinel plus an affirmative reply this turn to grant
// the per-session tier-3 unlock (design §5.1: "an explicit user
// confirmation in the transcript unlocks tier-3 for the session").
const tier3ConfirmSentinel = "confirm a from-scratch automation"

// anchorScore returns the best-fitting template slug from priors for
// the operator's free-text description, plus a confidence score in
// [0,1].
//
// Heuristic (v1, no ML/embeddings): tokenize the description and each
// prior's DisplayName+Description+Domain into lowercase word sets
// (splitting on non-alphanumerics, dropping tokens shorter than 3
// characters as stopword-ish noise), then score by Jaccard overlap
// (|intersection| / |union|) between the two sets. The prior with the
// highest score wins; ties keep the first-seen (priors is generally
// gallery-order, stable). Returns ("", 0) when there are no priors or
// the description tokenizes to nothing.
//
// This is deliberately crude — it exists to catch the common case
// ("build me a daily AI news digest" when a template called exactly
// that already ships) so the composer defaults to the cheaper,
// better-tested tier-1/2 path instead of re-synthesizing a bundle the
// gallery already solved. A future revision can swap in embeddings
// without changing the call signature.
func anchorScore(desc string, priors []TemplatePrior) (slug string, score float64) {
	descTokens := tokenize(desc)
	if len(descTokens) == 0 || len(priors) == 0 {
		return "", 0
	}
	bestSlug := ""
	bestScore := -1.0
	for _, p := range priors {
		priorTokens := tokenize(p.DisplayName + " " + p.Description + " " + p.Domain)
		s := tokenOverlapScore(descTokens, priorTokens)
		if s > bestScore {
			bestScore = s
			bestSlug = p.Slug
		}
	}
	if bestScore < 0 {
		return "", 0
	}
	return bestSlug, bestScore
}

// tokenize lowercases and splits s on non-alphanumeric boundaries,
// dropping short (<3 char) tokens as noise (stopwords like "a", "to",
// "an", "of" would otherwise dominate small-description overlap
// scores).
func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 3 {
			out[strings.ToLower(cur.String())] = true
		}
		cur.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// tokenOverlapScore is the Jaccard index of two token sets.
func tokenOverlapScore(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// tierLabel derives the Prometheus tier label from the final envelope
// that is about to be persisted/returned for a turn. Historical
// (pre-composer) turns never set Tier explicitly; the legacy
// discriminator (Composition present → tier 2, else tier 1) is
// preserved for those so the metric doesn't regress to an "unknown"
// bucket for every existing wizard turn.
func tierLabel(envelope *Envelope) string {
	if envelope == nil {
		return tierLabelUnknown
	}
	if envelope.Tier != 0 {
		return strconv.Itoa(envelope.Tier)
	}
	if envelope.Composition != nil {
		return "2"
	}
	return "1"
}

// tierLabelUnknown labels turns rejected before an envelope was ever
// parsed (turn-cap, session-cap, LLM transport error, …) — there is no
// tier to report because the LLM was never reached (or its answer
// never decoded).
const tierLabelUnknown = "n/a"

// affirmativeReply reports whether the operator's message reads as an
// explicit "yes, go ahead" — the trigger for detectTier3Unlock. Kept
// to a short, conservative allowlist: the unlock overrides a security
// gate, so a false positive (treating an ambiguous reply as
// confirmation) is worse than a false negative (asking the operator
// to be more explicit).
func affirmativeReply(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	m = strings.Trim(m, ".!? ")
	switch m {
	case "yes", "yes.", "yep", "yeah", "correct", "confirm", "confirmed",
		"go ahead", "do it", "build it", "build it from scratch",
		"yes, build it from scratch", "yes build it from scratch",
		"from scratch", "yes please", "proceed":
		return true
	}
	return false
}

// detectTier3Unlock reports whether THIS user turn should unlock
// tier-3 for the rest of the session: the immediately preceding
// assistant turn asked the operator to confirm a from-scratch
// automation (carries tier3ConfirmSentinel) and this turn's message
// is an explicit affirmative (design §5.1 — "an explicit user
// confirmation in the transcript unlocks tier-3 for the session").
func detectTier3Unlock(transcript []Turn, userMessage string) bool {
	if !affirmativeReply(userMessage) {
		return false
	}
	for i := len(transcript) - 1; i >= 0; i-- {
		t := transcript[i]
		if t.Role != "assistant" {
			continue
		}
		return strings.Contains(t.Content, tier3ConfirmSentinel) ||
			(t.Envelope != nil && strings.Contains(t.Envelope.Message, tier3ConfirmSentinel))
	}
	return false
}
