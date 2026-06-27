package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/secrets"
)

// GateName is one of the declared quality-gate names from
// https://docs.vornik.io §4. The
// quarantine table records the gate name on every rejected chunk
// so operators can answer "what kept this out of memory?".
type GateName string

const (
	// Provenance / validity (refuse rather than quarantine — the
	// caller is malformed).
	GateSchemaMatch        GateName = "schema_match"
	GateProvenanceComplete GateName = "provenance_complete"
	GateClassKnown         GateName = "class_known"

	// Safety (redact-or-quarantine).
	GateSecretScan      GateName = "secret_scan"
	GatePolicyMatch     GateName = "policy_match"
	GatePromptInjection GateName = "prompt_injection"

	// Completeness.
	GateMinContent      GateName = "min_content"
	GateTruncationCheck GateName = "truncation_check"

	// Uniqueness.
	GateDedupHash        GateName = "dedup_hash"
	GateNearDupSupersede GateName = "near_dup_supersede"

	// Timeliness.
	GateTTLSet GateName = "ttl_set"

	// Accuracy / consistency (Phase 4 — wired but no-op when
	// validator role isn't configured).
	GateCitationCheck        GateName = "citation_check"
	GateClaimAuditOverlap    GateName = "claim_audit_overlap"
	GateCrossChunkContradict GateName = "cross_chunk_contradiction"
	GateClassMatch           GateName = "class_match"
)

// IngestCandidate is the input shape every gate consumes. It carries
// just enough metadata for deterministic gating; LLM-driven gates
// (Phase 4) layer richer analysis on top.
type IngestCandidate struct {
	ProjectID          string
	SourceArtifactID   string
	SourceName         string // artifact filename
	ProducerRole       string
	IngestExecutionID  string
	Content            string
	ContentHash        string       // sha256 of Content; populated by gates if empty
	ProposedClass      ContentClass // hint from producer; classifier may override
	ProposedConfidence float32

	// ClaimAuditResults is pre-populated by the pipeline before
	// gates run: extracted claims paired with their tool_audit_log
	// presence verdict. ClaimAuditOverlapGate is a pure read of
	// this slice. Empty (or nil) is the "no claims to verify"
	// signal — gate auto-allows pure-prose content.
	ClaimAuditResults []ClaimMatch

	// TTLOverride, when non-nil, replaces the class-policy default
	// TTL when stamping `expires_at` on admitted chunks. Used by
	// IngestCompanionNote to honour the caller-supplied `ttl_days`
	// arg on the remember() MCP tool (LLD-22). Nil leaves the
	// existing per-class default in place. Zero duration means
	// "no expiry" (matches the policy convention).
	TTLOverride *time.Duration

	// RepoScope partitions this deposit within the project's RAG
	// (migration 75). Empty = uncategorized; "*" = cross-cutting;
	// any other string = a repo token. Stamped on the resulting
	// chunk row (and on the quarantine row when the candidate is
	// refused) so scoped recall can filter without LIKE-scanning
	// source_name. Threaded through IngestArtifactOptions.
	RepoScope string
}

// GateOutcome is what a gate decides about one candidate.
type GateOutcome struct {
	Action     GateAction
	Gate       GateName
	Detail     string // human-readable reason; lands on quarantine row when Action == Quarantine
	NewContent string // when Action == Allow and the gate scrubbed content (e.g. secret redact), this is the post-scrub form
	// ShadowSignal flags an Allow outcome that should land in
	// lifecycle_state='shadow' instead of 'published'. Set by
	// ClaimAuditOverlapGate on partial-overlap; consumed by Phase
	// 19 (shadow lifecycle wiring). Phase 17 records the signal
	// but admit semantics remain unchanged.
	ShadowSignal bool
}

// GateAction is the per-gate decision.
type GateAction int

const (
	// GateAllow — gate passed; proceed to next stage.
	GateAllow GateAction = iota
	// GateRedact — gate found policy-relevant content; proceed
	// with NewContent substituted in.
	GateRedact
	// GateQuarantine — gate failed; route to project_memory_quarantine.
	GateQuarantine
	// GateReject — caller is malformed; refuse without storing
	// in quarantine. Used for schema_match / provenance_complete
	// where the row should never have been enqueued.
	GateReject
)

// GateConfig tunes runtime gate behaviour. Zero values pick safe
// defaults (gate enabled, default thresholds).
type GateConfig struct {
	// MinContentChars rejects chunks with fewer chars. Default 64.
	MinContentChars int
	// MinContentWords quarantines chunks with fewer words.
	// Default 10.
	MinContentWords int
	// TruncationToleranceFraction allows ±N% byte drift between
	// the candidate Content size and the source artifact's recorded
	// byte length. Default 0.05 (5%). Only applied when the gate is
	// enabled for the candidate's class via TruncationCheckClasses.
	TruncationToleranceFraction float64
	// TruncationCheckClasses gates the truncation check by content
	// class. Empty (default) means the gate is DISABLED — it allows
	// every candidate through. The original ±5% byte-drift check
	// was designed for verbatim pass-through ingestion (raw HTML,
	// extracted-document chunks) where size drift signals real
	// truncation; summarising producers (writer / researcher /
	// scout) legitimately produce content that's much smaller (a
	// summary) or much larger (enrichment + analysis) than the
	// source artifact, so the size comparison is meaningless for
	// them. The 2026-05-19→20 quarantine spike (57 false-positive
	// rows in two days, all research-class summaries) drove this
	// off-by-default change. Operators with a true verbatim
	// ingestion path opt in by listing the relevant classes here.
	TruncationCheckClasses []ContentClass
	// ClaimAuditMinMatchRatio is the minimum matched/total claim
	// ratio that keeps a candidate out of quarantine. Default 0.0
	// means "never quarantine on claim mismatch — demote to shadow
	// lifecycle instead", which is the right behaviour for the
	// claim-audit gate's most common failure mode: URL/path
	// normalisation drift between a writer's narrative output and
	// the tool's recorded args (which the gate counted as zero
	// matches and quarantined as "hallucinated"). Stricter
	// projects can set 0.5 ("at least half grounded") or 1.0
	// ("every claim grounded or quarantine"). Partial overlap
	// (matched < total but >= ratio) still flips ShadowSignal so
	// the chunk lands in shadow lifecycle.
	ClaimAuditMinMatchRatio float64
	// SecretsDetector is the same one the indexer uses. Wired by
	// the pipeline.
	SecretsDetector secrets.Detector
	// SecretsActions per-checkpoint policy (matches indexer).
	SecretsActions map[string]secrets.Action

	// PromptInjectionAction controls the prompt-injection gate:
	//   "" / "off"   — gate disabled (default; existing behaviour)
	//   "detect"     — detect-only: allow but record the signal in the
	//                  gate trail + metric
	//   "quarantine" — route detected content to quarantine for review
	// Quarantine is the safe "catch" posture (reversible; an operator
	// releases). Detect-only lets a project measure the false-positive
	// rate before flipping to quarantine.
	PromptInjectionAction string
}

// Prompt-injection gate action literals.
const (
	InjectionActionOff        = "off"
	InjectionActionDetect     = "detect"
	InjectionActionQuarantine = "quarantine"
)

// DefaultGateConfig returns the vornik-shipped baseline.
func DefaultGateConfig() GateConfig {
	return GateConfig{
		MinContentChars:             64,
		MinContentWords:             10,
		TruncationToleranceFraction: 0.05,
		// TruncationCheckClasses left nil/empty so the gate is OFF
		// by default — see field doc.
		// ClaimAuditMinMatchRatio defaults to 0 so the gate only
		// shadow-flags rather than quarantining — see field doc.
	}
}

// EnsureContentHash populates ContentHash if missing. Idempotent.
// Used both as a gate side-effect (dedup needs the hash) and by
// callers staging candidates before the pipeline runs.
func EnsureContentHash(c *IngestCandidate) {
	if c == nil || c.ContentHash != "" {
		return
	}
	sum := sha256.Sum256([]byte(c.Content))
	c.ContentHash = hex.EncodeToString(sum[:])
}

// ============================================================
// Validity / provenance gates (run first; refuse rather than
// quarantine because the caller violated the contract).
// ============================================================

// SchemaMatchGate refuses candidates with missing required fields.
// The pipeline calls this before any other gate so structurally bad
// rows never see classification, embedding, or storage.
func SchemaMatchGate(c *IngestCandidate) GateOutcome {
	if c == nil {
		return GateOutcome{Action: GateReject, Gate: GateSchemaMatch, Detail: "nil candidate"}
	}
	if c.ProjectID == "" {
		return GateOutcome{Action: GateReject, Gate: GateSchemaMatch, Detail: "project_id required"}
	}
	if c.SourceArtifactID == "" {
		return GateOutcome{Action: GateReject, Gate: GateSchemaMatch, Detail: "source_artifact_id required"}
	}
	if c.ProducerRole == "" {
		return GateOutcome{Action: GateReject, Gate: GateSchemaMatch, Detail: "producer_role required"}
	}
	if c.Content == "" {
		return GateOutcome{Action: GateReject, Gate: GateSchemaMatch, Detail: "content empty"}
	}
	return GateOutcome{Action: GateAllow, Gate: GateSchemaMatch}
}

// ProvenanceCompleteGate refuses candidates that lack the minimum
// provenance triple needed for snapshot/rollback to mean anything.
// IngestExecutionID is allowed empty — some producers (CLI imports,
// chat uploads) don't have one.
//
// LLD 22 carve-out: companion-origin candidates (ProducerRole prefixed
// with `companion:`) are inline notes deposited via the `remember` MCP
// tool. They have no upstream artifact row to point to — the
// SourceName itself carries the lineage. Accept them as long as
// ProducerRole is present and well-formed.
func ProvenanceCompleteGate(c *IngestCandidate) GateOutcome {
	if c.ProducerRole == "" {
		return GateOutcome{
			Action: GateReject,
			Gate:   GateProvenanceComplete,
			Detail: "missing producer_role",
		}
	}
	if isCompanionProducer(c.ProducerRole) {
		// Companion deposits skip the artifact-ID requirement.
		// Their provenance lives in producer_role + source_name.
		return GateOutcome{Action: GateAllow, Gate: GateProvenanceComplete}
	}
	if c.SourceArtifactID == "" {
		return GateOutcome{
			Action: GateReject,
			Gate:   GateProvenanceComplete,
			Detail: "missing source_artifact_id",
		}
	}
	return GateOutcome{Action: GateAllow, Gate: GateProvenanceComplete}
}

// isCompanionProducer reports whether the producer role describes a
// host-LLM companion client (LLD 22) rather than a vornik agent role.
// Accepted form: "companion:<client_kind>", where client_kind matches
// `[a-z][a-z0-9-]*` (the same shape `vornikctl companion grant
// --client` enforces).
func isCompanionProducer(role string) bool {
	const prefix = "companion:"
	if len(role) <= len(prefix) || role[:len(prefix)] != prefix {
		return false
	}
	rest := role[len(prefix):]
	if rest == "" {
		return false
	}
	for i, r := range rest {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ClassKnownGate ensures the proposed class is in the vornik
// catalog. Unknown classes downgrade to ClassUnclassified with a
// warn-log (caller's responsibility) — never refused, because the
// pipeline can still ingest with the catch-all class.
func ClassKnownGate(c *IngestCandidate) GateOutcome {
	if c.ProposedClass == "" {
		// Empty is fine — the pipeline assigns via ClassifyByRole.
		return GateOutcome{Action: GateAllow, Gate: GateClassKnown}
	}
	if !IsValidClass(string(c.ProposedClass)) {
		// Don't reject; pipeline will substitute unclassified.
		return GateOutcome{
			Action: GateAllow,
			Gate:   GateClassKnown,
			Detail: fmt.Sprintf("unknown class %q downgraded to unclassified", c.ProposedClass),
		}
	}
	return GateOutcome{Action: GateAllow, Gate: GateClassKnown}
}

// ============================================================
// Safety gates (redact or quarantine).
// ============================================================

// secretRedactRejectFraction is the share of a deposit that, once
// stripped by secret redaction, flips the outcome from GateRedact to
// GateReject. LLD 22 § Risks: ">50% redaction → reject". A deposit
// that's mostly credentials shouldn't be admitted as a near-empty
// husk — it's a secret-dump and belongs nowhere in memory.
const secretRedactRejectFraction = 0.50

// redactionStripFraction reports the fraction of the original
// content (by byte length) removed by redaction. Returns 0 when the
// original is empty (caller guards that case) or when redaction grew
// the content (redaction markers can be longer than the secret they
// replace — that's not "stripping", so it counts as 0). Range [0,1].
func redactionStripFraction(original, redacted string) float64 {
	orig := len(original)
	if orig == 0 {
		return 0
	}
	stripped := orig - len(redacted)
	if stripped <= 0 {
		return 0
	}
	return float64(stripped) / float64(orig)
}

// SecretScanGate runs the secret detector and either redacts or
// quarantines per the configured action. Mirrors indexer's existing
// scanContentForSecrets so a chunk that survives the gate can never
// carry plaintext credentials.
//
// On the redact path, a deposit whose redaction strips more than
// secretRedactRejectFraction of its bytes is rejected as a
// secret-dump (LLD 22 § Risks) rather than admitted as a husk.
func SecretScanGate(c *IngestCandidate, cfg GateConfig) GateOutcome {
	if cfg.SecretsDetector == nil || c.Content == "" {
		return GateOutcome{Action: GateAllow, Gate: GateSecretScan}
	}
	findings := cfg.SecretsDetector.Scan([]byte(c.Content))
	if len(findings) == 0 {
		return GateOutcome{Action: GateAllow, Gate: GateSecretScan}
	}
	action := secrets.ResolveAction(secrets.CheckpointMemory, cfg.SecretsActions)
	switch action {
	case secrets.ActionBlock:
		return GateOutcome{
			Action: GateQuarantine,
			Gate:   GateSecretScan,
			Detail: fmt.Sprintf("%d secret finding(s); checkpoint policy=block", len(findings)),
		}
	case secrets.ActionRedact:
		redacted := string(secrets.Redact([]byte(c.Content), findings))
		// LLD 22 § "secret_scan" + § Risks: redaction is the first
		// response, but a paste that's mostly credentials would leave
		// a husk of ">>> REDACTED <<<" markers with little real
		// content. When redaction strips more than half the original
		// bytes the deposit is treated as a secret-dump and rejected
		// outright rather than admitted as a near-empty chunk. The
		// fraction is measured against the pre-redaction length; an
		// empty original (already guarded above) never reaches here.
		if redactionStripFraction(c.Content, redacted) > secretRedactRejectFraction {
			return GateOutcome{
				Action: GateReject,
				Gate:   GateSecretScan,
				Detail: fmt.Sprintf("%d secret finding(s); redaction stripped >%.0f%% of content — rejecting secret-dump",
					len(findings), secretRedactRejectFraction*100),
			}
		}
		return GateOutcome{
			Action:     GateRedact,
			Gate:       GateSecretScan,
			Detail:     fmt.Sprintf("%d secret finding(s) redacted", len(findings)),
			NewContent: redacted,
		}
	default:
		// Detect-only: log via caller, don't modify content.
		return GateOutcome{
			Action: GateAllow,
			Gate:   GateSecretScan,
			Detail: fmt.Sprintf("%d secret finding(s); detect-only", len(findings)),
		}
	}
}

// PolicyMatchGate matches the candidate against operator-declared
// deny patterns. Projects opt in via memory.deny_patterns in config.yaml,
// which flows through PipelineConfig.DenyPatterns into the hot-reloadable
// snapshot the pipeline passes here. Matching is substring (NOT regex), so
// the deny-list is ReDoS-immune by construction.
func PolicyMatchGate(c *IngestCandidate, denyPatterns []string) GateOutcome {
	if len(denyPatterns) == 0 {
		return GateOutcome{Action: GateAllow, Gate: GatePolicyMatch}
	}
	for _, pat := range denyPatterns {
		if pat == "" {
			continue
		}
		if strings.Contains(c.Content, pat) {
			return GateOutcome{
				Action: GateQuarantine,
				Gate:   GatePolicyMatch,
				Detail: fmt.Sprintf("matched deny pattern %q", pat),
			}
		}
	}
	return GateOutcome{Action: GateAllow, Gate: GatePolicyMatch}
}

// PromptInjectionGate scans candidate content for prompt-injection /
// context-manipulation signals (see DetectPromptInjection). A poisoned
// memory chunk is a stored-injection vector — written once, replayed into
// every agent that recalls it — which the secret / dedup / truncation
// gates don't catch. Behaviour is governed by cfg.PromptInjectionAction;
// the gate is OFF by default so existing deployments are unaffected.
func PromptInjectionGate(c *IngestCandidate, cfg GateConfig) GateOutcome {
	action := cfg.PromptInjectionAction
	if action == "" || action == InjectionActionOff || c.Content == "" {
		return GateOutcome{Action: GateAllow, Gate: GatePromptInjection}
	}
	hits := DetectPromptInjection(c.Content)
	if len(hits) == 0 {
		return GateOutcome{Action: GateAllow, Gate: GatePromptInjection}
	}
	detail := fmt.Sprintf("%d prompt-injection signal(s): %s", len(hits), strings.Join(hits, ", "))
	if action == InjectionActionQuarantine {
		return GateOutcome{Action: GateQuarantine, Gate: GatePromptInjection, Detail: detail}
	}
	// detect-only: allow, but surface the signal in the trail so the
	// pipeline can log/meter it without disrupting ingestion.
	return GateOutcome{Action: GateAllow, Gate: GatePromptInjection, Detail: detail, ShadowSignal: true}
}

// ============================================================
// Completeness gates.
// ============================================================

// MinContentGate refuses near-empty chunks. Sub-MinContentChars =
// reject (caller bug — content shouldn't have been queued).
// Sub-MinContentWords = quarantine (operator can release if
// they decide the brevity was intentional).
func MinContentGate(c *IngestCandidate, cfg GateConfig) GateOutcome {
	if cfg.MinContentChars <= 0 {
		cfg.MinContentChars = 64
	}
	if cfg.MinContentWords <= 0 {
		cfg.MinContentWords = 10
	}
	if len(c.Content) < cfg.MinContentChars {
		return GateOutcome{
			Action: GateReject,
			Gate:   GateMinContent,
			Detail: fmt.Sprintf("%d chars < min %d", len(c.Content), cfg.MinContentChars),
		}
	}
	if wordCount(c.Content) < cfg.MinContentWords {
		return GateOutcome{
			Action: GateQuarantine,
			Gate:   GateMinContent,
			Detail: fmt.Sprintf("%d words < min %d", wordCount(c.Content), cfg.MinContentWords),
		}
	}
	return GateOutcome{Action: GateAllow, Gate: GateMinContent}
}

// TruncationCheckGate ensures the candidate's Content matches the
// source artifact's recorded byte length within the configured
// tolerance. The gate is OFF unless the candidate's ProposedClass
// appears in cfg.TruncationCheckClasses — see the GateConfig field
// doc for why (summarising producers legitimately produce content
// that diverges from source size, and the gate's pre-2026-05-21
// always-on behaviour quarantined them en masse).
//
// When the gate is active, it uses cfg.TruncationToleranceFraction
// (default 5%). Drift outside ±tolerance quarantines the chunk.
func TruncationCheckGate(c *IngestCandidate, cfg GateConfig, sourceSizeBytes int64) GateOutcome {
	if sourceSizeBytes <= 0 {
		return GateOutcome{Action: GateAllow, Gate: GateTruncationCheck}
	}
	// Empty allowlist = gate disabled (new default). Operators
	// listing one or more classes get the check applied for those
	// classes only.
	if !classInList(c.ProposedClass, cfg.TruncationCheckClasses) {
		return GateOutcome{Action: GateAllow, Gate: GateTruncationCheck}
	}
	tolerance := cfg.TruncationToleranceFraction
	if tolerance <= 0 {
		tolerance = 0.05
	}
	expected := float64(sourceSizeBytes)
	got := float64(len(c.Content))
	if expected == 0 {
		return GateOutcome{Action: GateAllow, Gate: GateTruncationCheck}
	}
	drift := (got - expected) / expected
	if drift < -tolerance || drift > tolerance {
		return GateOutcome{
			Action: GateQuarantine,
			Gate:   GateTruncationCheck,
			Detail: fmt.Sprintf("content size %d bytes vs source %d bytes (drift %.1f%%)",
				len(c.Content), sourceSizeBytes, drift*100),
		}
	}
	return GateOutcome{Action: GateAllow, Gate: GateTruncationCheck}
}

// classInList reports whether class matches any entry in allowed.
// Empty allowed → false (gate disabled). Helper kept package-local
// because both the truncation gate and any future class-scoped
// gates want the same short-circuit shape.
func classInList(class ContentClass, allowed []ContentClass) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, c := range allowed {
		if c == class {
			return true
		}
	}
	return false
}

// ============================================================
// Uniqueness gates.
// ============================================================

// DedupHashGate is a thin lookup against the existing
// (project_id, content_hash) unique index. Returns Allow when no
// duplicate exists; Reject (silent drop) when content already lives
// in the project — same content shouldn't double-ingest. The
// pipeline calls this only after EnsureContentHash.
//
// existsFn is provided by the pipeline so this gate stays pure
// (testable without a DB).
func DedupHashGate(c *IngestCandidate, existsFn func(projectID, hash string) (bool, error)) GateOutcome {
	if existsFn == nil {
		return GateOutcome{Action: GateAllow, Gate: GateDedupHash}
	}
	exists, err := existsFn(c.ProjectID, c.ContentHash)
	if err != nil {
		// Treat dedup lookup failure as allow — better to risk a
		// duplicate than to drop a real chunk on a transient DB
		// hiccup. The actual unique index will catch it on insert.
		return GateOutcome{Action: GateAllow, Gate: GateDedupHash, Detail: "dedup lookup failed: " + err.Error()}
	}
	if exists {
		return GateOutcome{
			Action: GateReject,
			Gate:   GateDedupHash,
			Detail: "exact duplicate already in memory",
		}
	}
	return GateOutcome{Action: GateAllow, Gate: GateDedupHash}
}

// ============================================================
// Helpers.
// ============================================================

func wordCount(s string) int {
	return len(strings.Fields(s))
}

// RunStandardGates applies the deterministic gate stack in order.
// Returns the FIRST gate that doesn't return GateAllow (so caller
// gets a single decision per candidate). The pipeline runs richer
// LLM-side gates (validator, supersession) outside of this helper.
//
// Ordering matters:
//  1. SchemaMatch (refuse malformed)
//  2. ProvenanceComplete (refuse incomplete)
//  3. ClassKnown (warn but don't refuse)
//  4. SecretScan (redact-or-quarantine)
//  5. PolicyMatch (quarantine on deny pattern)
//  6. ClaimAuditOverlap (Phase 17 — quarantine on zero overlap;
//     Allow w/ ShadowSignal on partial; pre-populated by pipeline)
//  7. MinContent (refuse trivially-short, quarantine short)
//  8. TruncationCheck (quarantine on size drift)
//  9. DedupHash (silently drop duplicates)
//
// The full Allow outcome (last element of the trail) is preserved
// in the returned final, so pipeline callers see ShadowSignal even
// on a fully-Allow run.
//
// Each gate's outcome is appended to the returned trail so the
// pipeline can record per-candidate gate history for audit.
func RunStandardGates(c *IngestCandidate, cfg GateConfig, denyPatterns []string, sourceSize int64, dedupExists func(string, string) (bool, error)) (GateOutcome, []GateOutcome) {
	if c == nil {
		return GateOutcome{Action: GateReject, Gate: GateSchemaMatch, Detail: "nil candidate"}, nil
	}
	trail := make([]GateOutcome, 0, 10)
	type gateFn func() GateOutcome
	gates := []gateFn{
		func() GateOutcome { return SchemaMatchGate(c) },
		func() GateOutcome { return ProvenanceCompleteGate(c) },
		func() GateOutcome { return ClassKnownGate(c) },
		func() GateOutcome {
			out := SecretScanGate(c, cfg)
			if out.Action == GateRedact && out.NewContent != "" {
				// Apply redaction in-place so subsequent gates see
				// the cleaned content. Caller observes via trail.
				c.Content = out.NewContent
				EnsureContentHash(c)
			}
			return out
		},
		func() GateOutcome { return PolicyMatchGate(c, denyPatterns) },
		func() GateOutcome { return PromptInjectionGate(c, cfg) },
		func() GateOutcome { return ClaimAuditOverlapGate(c, cfg) },
		func() GateOutcome { return MinContentGate(c, cfg) },
		func() GateOutcome { return TruncationCheckGate(c, cfg, sourceSize) },
		func() GateOutcome {
			EnsureContentHash(c)
			return DedupHashGate(c, dedupExists)
		},
	}
	// shadowFlag latches across gates so the pipeline sees
	// ClaimAuditOverlapGate's signal even after later gates
	// return plain Allow.
	shadowFlag := false
	for _, g := range gates {
		out := g()
		trail = append(trail, out)
		if out.ShadowSignal {
			shadowFlag = true
		}
		switch out.Action {
		case GateReject, GateQuarantine:
			return out, trail
		}
	}
	return GateOutcome{Action: GateAllow, ShadowSignal: shadowFlag}, trail
}

// ErrCandidateRejected is returned by pipeline helpers when a gate
// chose GateReject. Distinct error so callers can distinguish "this
// shouldn't have been queued" from "transient DB failure".
var ErrCandidateRejected = errors.New("candidate rejected by gate")
