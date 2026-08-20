package api

// Near-duplicate detection for the knowledge-skill store (LLD
// 2026-07-07-knowledge-skill-store-design §12.2).
//
// The catalogue drifted into six clusters of overlapping skills because
// nothing at propose time could answer "do we already have this":
// skill_search is a lowercase substring match over name+description, and
// Upsert's natural key is an exact (project, repo_scope, name) triple. This
// file is the missing similarity pass.
//
// Primary metric is SEMANTIC (cosine over stored embeddings), with a lexical
// fallback for when the embedder is unavailable or a row is not yet backfilled.
//
// It started out purely lexical; that was implemented, measured, and rejected
// (0.061 where 0.5 was required). The lexical code below survives as the
// degraded path, not as the design.
//
// Calibration also corrected what this file is FOR. The original premise was
// that the six §12.0 "clusters" were duplicate-pairs, so the metric had to flag
// them. Measured, they are not: skills sharing "WHEN reviewing…" phrasing score
// 0.498–0.660, indistinguishable from unrelated skills at 0.399–0.544, while
// genuine duplicates (the same knowledge reworded) score 0.740–0.917. So this
// file detects DUPLICATED KNOWLEDGE. "Four skills that all trigger on
// reviewing" is a catalogue-curation problem it deliberately does not attempt —
// no similarity metric can separate that from coincidence.
//
// The embedding is stored as JSON-encoded TEXT and cosine runs here in Go —
// §1 forbids a pgvector COLUMN in this table (the sqlite lane), which is not
// the same as forbidding a vector. tags/roles already use that pattern.

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"vornik.io/vornik/internal/persistence"
)

// skillDupeThreshold is the COSINE similarity at or above which a candidate is
// reported as a near-duplicate.
//
// Calibrated 2026-08-07 against real catalogue rows (LLD §12.2):
//
//	true duplicates (same knowledge, different words)   0.740 – 0.917
//	skills that merely share trigger phrasing           0.498 – 0.660
//	unrelated skills                                    0.399 – 0.544
//
// 0.70 sits in the gap between the first tier and everything else. Note the
// second and third tiers OVERLAP: "four skills that all trigger on reviewing"
// is not a similarity problem and this metric cannot separate it from
// coincidence. That is catalogue curation, not deduplication — see §12.0.
//
// Raw cosine over short English text has a high baseline (~0.4 even for
// unrelated pairs), so this number is not "40% similar"; it is a point on a
// measured distribution. Re-run the calibration before changing it, and if a
// tier boundary moves, fix the metric rather than sliding this to fit (§12.5).
const skillDupeThreshold = 0.70

// skillDupeLexicalThreshold applies ONLY to the degraded lexical fallback path
// (embedder unconfigured or down, or a row not yet backfilled).
//
// It is deliberately NOT the cosine threshold. Measured against the §12.0
// clusters, lexical scores intra-cluster pairs at 0.061–0.130 and unrelated
// controls at 0.019–0.032 — real separation, but nothing like cosine's range.
// Reusing 0.4 here would make the fallback a no-op that silently flags nothing;
// this value catches the strongest lexical signals while staying above the
// observed control ceiling. The fallback is a safety net, not a substitute:
// §12.5 forbids treating a thin-headroom lexical threshold as the real guard.
const skillDupeLexicalThreshold = 0.05

// skillMatchReason explains why a candidate was flagged, so the propose
// response tells the author what kind of collision they hit rather than
// handing them a bare score.
type skillMatchReason string

const (
	reasonSimilarEmbedding   skillMatchReason = "similar-embedding"
	reasonSimilarDescription skillMatchReason = "similar-description"
	reasonSimilarHeadings    skillMatchReason = "similar-headings"
	reasonNameOtherScope     skillMatchReason = "name-collision-other-scope"
)

// skillMatch is one near-duplicate hit.
type skillMatch struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	RepoScope   string           `json:"repo_scope,omitempty"`
	Maturity    string           `json:"maturity"`
	Description string           `json:"description"`
	Score       float64          `json:"score"`
	Reason      skillMatchReason `json:"reason"`
}

// skillStopwords are dropped before comparison. Kept deliberately small: the
// trigger phrasing of a skill description ("when reviewing a diff that…") is
// mostly function words, and stripping too many collapses distinct triggers
// into the same token set.
var skillStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "any": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "before": {}, "but": {}, "by": {}, "can": {}, "do": {},
	"else": {}, "for": {}, "from": {}, "has": {}, "have": {}, "in": {},
	"into": {}, "is": {}, "it": {}, "its": {}, "of": {}, "on": {}, "or": {},
	"that": {}, "the": {}, "their": {}, "them": {}, "then": {}, "there": {},
	"these": {}, "they": {}, "this": {}, "to": {}, "use": {}, "using": {},
	"via": {}, "was": {}, "were": {}, "what": {}, "when": {}, "where": {},
	"which": {}, "while": {}, "who": {}, "will": {}, "with": {}, "you": {},
	"your": {},
}

// tokenizeSkillText lowercases, splits on non-alphanumerics, and drops
// stopwords and 1-character fragments.
func tokenizeSkillText(s string) map[string]struct{} {
	out := make(map[string]struct{})
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		if _, skip := skillStopwords[f]; skip {
			continue
		}
		out[f] = struct{}{}
	}
	return out
}

// jaccard is |A∩B| / |A∪B|. Two empty sets score 0, not 1 — an empty
// description must never be reported as a perfect match for another empty one.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// skillHeadings extracts normalised Markdown H1/H2 headings from a body.
//
// Normalisation strips the leading hashes and any section numbering, so
// "## §12.1 Root cause", "## 12.1 Root cause" and "## Root cause" all reduce to
// the same token set. Both the §-prefixed and bare-numeric forms occur in this
// corpus, so handling only one would silently miss half the matches.
func skillHeadings(body string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		text := strings.TrimLeft(trimmed, "#")
		text = strings.TrimSpace(text)
		text = strings.TrimPrefix(text, "§")
		// Drop a leading section number ("12.1", "3.", "4").
		if i := strings.IndexFunc(text, func(r rune) bool {
			return !unicode.IsDigit(r) && r != '.' && r != '§'
		}); i > 0 {
			text = strings.TrimSpace(text[i:])
		}
		for tok := range tokenizeSkillText(text) {
			out[tok] = struct{}{}
		}
	}
	return out
}

// cosine returns the cosine similarity of two embeddings, or -1 when they are
// not comparable (different dimensions, or either is empty). -1 rather than 0
// so callers can distinguish "not comparable" from "orthogonal".
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// skillsAreEmbeddingComparable reports whether two rows can be compared by
// cosine. Both must carry a vector AND the SAME embedding model: vectors from
// different models occupy different spaces, and comparing them produces a
// confident number that means nothing — worse than no score at all.
func skillsAreEmbeddingComparable(a, b *persistence.Skill) bool {
	return len(a.Embedding) > 0 && len(b.Embedding) > 0 &&
		a.EmbeddingModel != "" && a.EmbeddingModel == b.EmbeddingModel
}

// skillEmbeddingText is the text a skill's embedding is computed over. Name and
// description carry the trigger ("when do I use this"), which is what
// near-duplicate detection is actually comparing; the body is excluded so that
// two skills with the same trigger but different depth still match.
func skillEmbeddingText(s *persistence.Skill) string {
	return strings.TrimSpace(s.Name + "\n" + s.Description)
}

// scoreSkillPair returns the description-axis and heading-axis Jaccard scores
// for two skills. Name is folded into the description axis: a shared kebab-case
// slug is strong evidence and would otherwise be ignored entirely.
func scoreSkillPair(aName, aDesc, aBody, bName, bDesc, bBody string) (descScore, headScore float64) {
	descScore = jaccard(
		tokenizeSkillText(aName+" "+aDesc),
		tokenizeSkillText(bName+" "+bDesc),
	)
	headScore = jaccard(skillHeadings(aBody), skillHeadings(bBody))
	return descScore, headScore
}

// findSkillDuplicates scores a proposed skill against the full candidate set
// and returns the hits, strongest first.
//
// INVARIANT (LLD §12.2): `existing` MUST be the unscoped candidate set — every
// maturity including retired, every repo_scope in the project, AND every other
// project's is_global rows. Do NOT narrow it with skill_search's scope filter:
// that filter is the bug this whole slice exists to work around, since the
// injector applies no scope at all. A candidate hidden from the author here is
// a duplicate waiting to be authored.
func findSkillDuplicates(candidate *persistence.Skill, existing []*persistence.Skill) []skillMatch {
	var out []skillMatch
	for _, ex := range existing {
		if ex == nil || ex.ID == candidate.ID {
			continue
		}
		// An exact natural-key match is the SAME skill being versioned, which
		// §12.2 routes to the in-place archive-then-update path — not a
		// near-duplicate to disambiguate. Blocking it offered only dispositions
		// that describe it untruthfully (`supersedes` asserts a different
		// skill; `confirm_distinct` asserts distinctness), and answering with
		// the first destroyed an operator-approved skill on 2026-08-20.
		if sameSkillIdentity(candidate, ex) {
			continue
		}
		// An identical name under a different scope is a collision at any
		// score: it produces two rows the injector cannot tell apart.
		if strings.EqualFold(ex.Name, candidate.Name) && ex.RepoScope != candidate.RepoScope {
			out = append(out, newSkillMatch(ex, 1, reasonNameOtherScope))
			continue
		}
		// Preferred path: semantic. The §12.0 clusters overlap in meaning, not
		// in vocabulary ("reviewing infrastructure changes" vs "reviewing a
		// diff that claims tests"), which is why the lexical metric this
		// replaced scored 0.061 against a required 0.5.
		if skillsAreEmbeddingComparable(candidate, ex) {
			if c := cosine(candidate.Embedding, ex.Embedding); c >= skillDupeThreshold {
				out = append(out, newSkillMatch(ex, c, reasonSimilarEmbedding))
			}
			continue
		}

		// Fallback: lexical. Reached when the embedder is unconfigured or down
		// (Embed returns nil,nil by contract), or when a row is not yet
		// backfilled. Weaker, but an embedder outage must never block
		// authoring — and the exact-name check above still fires regardless.
		descScore, headScore := scoreSkillPair(
			candidate.Name, candidate.Description, candidate.Body,
			ex.Name, ex.Description, ex.Body,
		)
		switch {
		case descScore >= skillDupeLexicalThreshold && descScore >= headScore:
			out = append(out, newSkillMatch(ex, descScore, reasonSimilarDescription))
		case headScore >= skillDupeLexicalThreshold:
			out = append(out, newSkillMatch(ex, headScore, reasonSimilarHeadings))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// sameSkillIdentity reports whether two rows share the natural key
// (project, repo_scope, name) that UNIQUE constrains — i.e. they are one
// skill, not two.
func sameSkillIdentity(a, b *persistence.Skill) bool {
	return a.ProjectID == b.ProjectID &&
		a.RepoScope == b.RepoScope &&
		strings.EqualFold(a.Name, b.Name)
}

func newSkillMatch(s *persistence.Skill, score float64, reason skillMatchReason) skillMatch {
	return skillMatch{
		ID:          s.ID,
		Name:        s.Name,
		RepoScope:   s.RepoScope,
		Maturity:    s.Maturity,
		Description: s.Description,
		Score:       score,
		Reason:      reason,
	}
}
