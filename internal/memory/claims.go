package memory

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ClaimCategory tags one extracted claim by what kind of fact it
// asserts about the world. The audit-overlap gate (Phase 17 of
// memory hardening, Pillar 1 of the LLD in
// https://docs.vornik.io) treats
// every category equally — a backtick command and a file path are
// each one claim, scored 0/1 against tool_audit_log presence.
type ClaimCategory string

const (
	// ClaimBacktickCommand matches shell-style commands inside
	// backticks: `go test ./...`, `npm install`, `git commit -am`.
	// The most load-bearing category — a chunk that says "I ran
	// `make install`" but no such tool invocation exists in the
	// audit log is the canonical hallucination shape.
	ClaimBacktickCommand ClaimCategory = "command"

	// ClaimFilePath matches token-shaped paths that end with a
	// known source/code file extension (.go, .md, .yaml, …).
	// Bare directory references aren't claims because we have
	// no way to verify them through tool_audit.
	ClaimFilePath ClaimCategory = "file_path"

	// ClaimURL matches HTTP/HTTPS URLs. Verified against
	// WebFetch / curl-style tool calls in the audit.
	ClaimURL ClaimCategory = "url"

	// ClaimGitSHA matches 7-40 hex chars when the surrounding
	// content has commit context (the words "commit", "sha",
	// "merge", "rebase"). Plain hex tokens are too noisy
	// (auto-generated IDs, hashes, ULIDs, etc).
	ClaimGitSHA ClaimCategory = "git_sha"

	// ClaimEntityID matches vornik's internal ID shapes
	// (task_…, execution_…, chunk_…, epoch_…, artifact_…).
	// Verified against any tool_audit row that mentions the ID.
	ClaimEntityID ClaimCategory = "entity_id"
)

// Claim is one extracted fact asserted by an ingest candidate.
// Pairs with ClaimMatch when the audit lookup completes.
type Claim struct {
	Category ClaimCategory
	Value    string
}

// ClaimMatch is a Claim plus whether it was found in the
// per-execution tool_audit_log slice. AuditRowID is set only on
// Found=true and only on the first matching row — the gate is
// boolean-grounded, not weighted.
type ClaimMatch struct {
	Claim
	Found      bool
	AuditRowID string
	// MatchScore captures HOW the match was made: 1.0 for an exact
	// substring hit, between 0 and 1 for a fuzzy (token-Jaccard) hit,
	// 0 when not matched. Carried for observability — the gate today
	// reads Found only, but the inspector surfaces the score so
	// operators can see whether the verdict was sharp or soft.
	MatchScore float64
}

// DefaultSoftClaimThreshold is the minimum token-Jaccard between a
// claim's value and an audit row's text that counts as a fuzzy match.
// Calibrated to catch "ran `make install`" vs structured JSON like
// `{"command":"make","args":["install"]}` while rejecting unrelated
// rows that happen to share a noun or two.
const DefaultSoftClaimThreshold = 0.4

// SoftMatchClaim reports whether the claim's value is supported by
// the candidate audit text. First tries the cheap path (case-folded
// substring); falls back to token-Jaccard above `threshold` so
// paraphrased or restructured commands ("ran make install" vs
// {"command":"make install"}) still count as grounded.
//
// threshold ≤ 0 → DefaultSoftClaimThreshold. threshold ≥ 1 disables
// the fuzzy path (substring-only).
//
// Returns (matched, score). Score is 1.0 on a substring hit so callers
// can distinguish sharp from soft verdicts in audit / inspector views.
func SoftMatchClaim(claimValue, auditText string, threshold float64) (bool, float64) {
	v := strings.TrimSpace(claimValue)
	if v == "" || auditText == "" {
		return false, 0
	}
	if threshold <= 0 {
		threshold = DefaultSoftClaimThreshold
	}
	vLower := strings.ToLower(v)
	aLower := strings.ToLower(auditText)
	if strings.Contains(aLower, vLower) {
		return true, 1.0
	}
	if threshold >= 1 {
		return false, 0
	}
	score := jaccard(tokenSet(v), tokenSet(auditText))
	if score >= threshold {
		return true, score
	}
	return false, 0
}

// AuditLookupFunc resolves a slice of claims against
// tool_audit_log scoped to one execution. Returns one ClaimMatch
// per input Claim in the same order. Implementations should treat
// audit input/output text as opaque — claim presence is "the
// claim's value substring appears in tool_input OR tool_output of
// at least one row with the given execution_id".
type AuditLookupFunc func(ctx context.Context, executionID string, claims []Claim) ([]ClaimMatch, error)

// regex bank. Compiled once at package load. Each pattern is
// deliberately conservative — we'd rather miss a real claim
// (false negative → admit) than fabricate one (false positive →
// quarantine of legitimate prose). The gate's three-tier verdict
// already softens partial misses to shadow.
var (
	reBacktickClaim = regexp.MustCompile("`([^`\n]{2,200})`")
	reFilePathClaim = regexp.MustCompile(`(?i)\b([\w./_-]+\.(?:go|md|ya?ml|json|sh|sql|html?|js|tsx?|py|toml|env|conf|ini|proto|mod|sum))\b`)
	reURLClaim      = regexp.MustCompile(`https?://[^\s)<>"'\]]+`)
	reGitSHAClaim   = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	reEntityIDClaim = regexp.MustCompile(`\b(?:task|execution|chunk|epoch|artifact|step|exec)_[a-zA-Z0-9_]{8,}\b`)

	// commit-context cues that make bare hex worth treating as a SHA.
	commitContextCues = []string{"commit", "sha", "merge", "rebase", "cherry-pick", "checkout", "git log", "git show"}
)

// ExtractClaims pulls deterministic claims out of a chunk's
// content using the five-category regex bank. Returns claims in
// stable order (category-then-value asc) so the per-execution
// audit lookup is deterministic across reruns. Empty content
// returns nil.
//
// Exported for tests and the pipeline inspector. Production code
// goes through Pipeline.IngestArtifact, which calls this and the
// AuditLookup callback before the gate stack runs.
func ExtractClaims(content string) []Claim {
	if content == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []Claim
	add := func(cat ClaimCategory, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		key := string(cat) + "|" + val
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Claim{Category: cat, Value: val})
	}
	for _, m := range reBacktickClaim.FindAllStringSubmatch(content, -1) {
		if len(m) >= 2 {
			add(ClaimBacktickCommand, m[1])
		}
	}
	for _, m := range reFilePathClaim.FindAllStringSubmatch(content, -1) {
		if len(m) >= 2 {
			add(ClaimFilePath, m[1])
		}
	}
	for _, m := range reURLClaim.FindAllString(content, -1) {
		add(ClaimURL, strings.TrimRight(m, ".,;:"))
	}
	if hasCommitContext(content) {
		for _, m := range reGitSHAClaim.FindAllString(content, -1) {
			add(ClaimGitSHA, m)
		}
	}
	for _, m := range reEntityIDClaim.FindAllString(content, -1) {
		add(ClaimEntityID, m)
	}
	return out
}

func hasCommitContext(s string) bool {
	lower := strings.ToLower(s)
	for _, cue := range commitContextCues {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

// ClaimAuditOverlapGate scores how many of a candidate's
// extracted claims appear in tool_audit_log for the same
// execution_id. The pipeline must pre-populate ClaimAuditResults
// (see Pipeline.IngestArtifact) — the gate itself is pure so it
// stays unit-testable without a database.
//
// Verdicts (Phase 17 of memory hardening, see
// https://docs.vornik.io §4.3,
// retuned 2026-05-21 after the research-class quarantine spike):
//
//   - len(ClaimAuditResults) == 0  → Allow ("nothing to verify")
//   - matched/total < cfg.Min      → Quarantine
//   - matched == total             → Allow
//   - matched/total >= cfg.Min     → Allow with ShadowSignal=true
//
// Default cfg.ClaimAuditMinMatchRatio is 0, so the only quarantine
// path is "negative ratio" — i.e. effectively disabled. Any
// candidate with at least one extracted claim that didn't match
// flips ShadowSignal instead, deferring the call to the shadow
// lifecycle (where operators can review without blocking ingest).
// Stricter projects bump the ratio in their GateConfig.
//
// Phase 19 consumes ShadowSignal and routes the otherwise-admitted
// chunk to lifecycle_state='shadow'.
func ClaimAuditOverlapGate(c *IngestCandidate, cfg GateConfig) GateOutcome {
	if c == nil || len(c.ClaimAuditResults) == 0 {
		return GateOutcome{Action: GateAllow, Gate: GateClaimAuditOverlap}
	}
	total := len(c.ClaimAuditResults)
	matched := 0
	for _, r := range c.ClaimAuditResults {
		if r.Found {
			matched++
		}
	}
	ratio := float64(matched) / float64(total)
	if ratio < cfg.ClaimAuditMinMatchRatio {
		return GateOutcome{
			Action: GateQuarantine,
			Gate:   GateClaimAuditOverlap,
			Detail: fmt.Sprintf("%d/%d claims grounded (below min %.0f%%)",
				matched, total, cfg.ClaimAuditMinMatchRatio*100),
		}
	}
	if matched == total {
		return GateOutcome{
			Action: GateAllow,
			Gate:   GateClaimAuditOverlap,
			Detail: fmt.Sprintf("%d/%d claims grounded", matched, total),
		}
	}
	return GateOutcome{
		Action:       GateAllow,
		Gate:         GateClaimAuditOverlap,
		Detail:       fmt.Sprintf("partial_audit: %d/%d", matched, total),
		ShadowSignal: true,
	}
}
