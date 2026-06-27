package graph

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

//go:embed relationship_prompt.txt
var relationshipSystemPrompt string

// ResolvedEntity is the resolver output the orchestrator hands
// to the relationship stage: an entity id (already canonical in
// the DB) plus enough surface context for the LLM to reason over.
// Type lets the prompt steer the model toward type-appropriate
// predicates (PRICE pairs trigger QUOTED_PRICE, etc.).
type ResolvedEntity struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	CanonicalName string `json:"canonical_name"`
}

// EdgeProposal is the relationship stage's output — one row per
// proposed edge, NOT YET WRITTEN to knowledge_edges. The validator
// stage scores each proposal's evidence; the orchestrator decides
// which proposals upsert and which get dropped.
type EdgeProposal struct {
	From       string          `json:"from"`
	To         string          `json:"to"`
	Predicate  string          `json:"predicate"`
	Properties json.RawMessage `json:"properties,omitempty"`
	Evidence   string          `json:"evidence"`
}

// RelationshipMetrics carries token + count attribution back to
// the orchestrator. ProposedDropped surfaces the aggregate drop
// count; DropsByReason breaks it down by validation rule so
// dashboards can pinpoint which check is firing most. Sum of
// DropsByReason values equals ProposedDropped.
//
// Reason keys are the DropReason* constants below — stable
// identifiers so the Prometheus label set stays bounded.
type RelationshipMetrics struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	ProposedKept     int
	ProposedDropped  int
	DropsByReason    map[string]int
}

// DropReason* are the stable identifiers under which
// validateProposals reports per-rule drop counts. Operators
// add them as Prometheus label values, so the set is closed
// and lower-cased + underscored for grafana-friendly queries.
//
// Audit context (2026-05-25): the live KG showed 48% of
// entities isolated. Quantifying which drop reason dominates
// is the prerequisite for further tuning beyond the 2026-05-25
// evidence-substring normalisation fix.
const (
	DropReasonEmptyEndpoint      = "empty_endpoint"
	DropReasonSelfLoop           = "self_loop"
	DropReasonUnknownFrom        = "unknown_from"
	DropReasonUnknownTo          = "unknown_to"
	DropReasonUnknownPredicate   = "unknown_predicate"
	DropReasonEmptyEvidence      = "empty_evidence"
	DropReasonEvidenceNotInChunk = "evidence_not_in_chunk"
	DropReasonDuplicateTriple    = "duplicate_triple"
)

// RelationshipExtractor implements Stage 3 of the KG pipeline:
// derive edges between resolved entities present in a chunk.
//
// This stage carries the heaviest reasoning load — the LLD §4.4a
// recommends a 120b-class model here while the other stages
// happily run on 20b. The Model field is therefore typically
// distinct from the extractor/resolver model.
type RelationshipExtractor struct {
	Client      chat.Provider
	Model       string
	MaxAttempts int
	// MaxChunkBytes mirrors the extractor — bound the user
	// message so a pathological chunk can't blow context. 0 → 8 KiB.
	MaxChunkBytes int
}

// NewRelationshipExtractor returns a RelationshipExtractor with
// safe defaults.
func NewRelationshipExtractor(client chat.Provider, model string) *RelationshipExtractor {
	return &RelationshipExtractor{Client: client, Model: model, MaxAttempts: 3, MaxChunkBytes: 8 * 1024}
}

// Extract produces edge proposals from a chunk + resolved entity
// list. Returns nil with no error when the chunk has < 2 entities
// (no pairs possible) or when the model returns an empty array
// (chunk asserts no relationships). The orchestrator turns
// proposals into knowledge_edges rows after the validator scores
// each one.
func (re *RelationshipExtractor) Extract(ctx context.Context, content string, entities []ResolvedEntity) ([]EdgeProposal, *RelationshipMetrics, error) {
	if re == nil || re.Client == nil {
		return nil, nil, fmt.Errorf("RelationshipExtractor.Extract: client not configured")
	}
	metrics := &RelationshipMetrics{Model: re.Model}
	if len(entities) < 2 {
		return nil, metrics, nil
	}

	cap := re.MaxChunkBytes
	if cap <= 0 {
		cap = 8 * 1024
	}
	body := content
	if len(body) > cap {
		body = truncateUTF8Bytes(body, cap)
	}

	entitiesJSON, err := json.Marshal(entities)
	if err != nil {
		return nil, metrics, fmt.Errorf("marshal entities: %w", err)
	}
	user := "ENTITIES:\n" + string(entitiesJSON) + "\n\nCHUNK:\n" + body

	maxAttempts := re.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	client := pickModel(re.Client, re.Model)

	resp, err := completeWithRetry(ctx, client, []chat.Message{
		{Role: "system", Content: relationshipSystemPrompt},
		{Role: "user", Content: user},
	}, maxAttempts)
	if err != nil {
		return nil, metrics, fmt.Errorf("relationship LLM call failed: %w", err)
	}
	metrics.PromptTokens = resp.Usage.PromptTokens
	metrics.CompletionTokens = resp.Usage.CompletionTokens
	if resp.Model != "" {
		metrics.Model = resp.Model
	}
	if len(resp.Choices) == 0 {
		return nil, metrics, fmt.Errorf("relationship returned no choices")
	}
	raw := stripJSONFence(resp.Choices[0].Message.Content)
	parsed, err := parseProposals(raw)
	if err != nil {
		return nil, metrics, fmt.Errorf("relationship JSON parse: %w", err)
	}
	kept, dropped, byReason := validateProposals(parsed, entities, body)
	metrics.ProposedKept = len(kept)
	metrics.ProposedDropped = dropped
	metrics.DropsByReason = byReason
	return kept, metrics, nil
}

// parseProposals tolerates bare arrays and the common
// {"edges":[...]} / {"relationships":[...]} object wrappings.
func parseProposals(raw string) ([]EdgeProposal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw[0] == '[' {
		var arr []EdgeProposal
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var wrap struct {
		Edges         []EdgeProposal `json:"edges"`
		Relationships []EdgeProposal `json:"relationships"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Edges) > 0 {
		return wrap.Edges, nil
	}
	return wrap.Relationships, nil
}

// validateProposals enforces:
//   - from + to are non-empty and present in the entity list
//   - from != to (self-loops dropped — no useful semantics)
//   - predicate is in the closed set
//   - evidence is a verbatim substring of the chunk (the cheap
//     pre-validator check; the full faithfulness validator stage
//     scores each remaining edge with an LLM). Cosmetic-only
//     differences (smart quotes, NFD diacritics, collapsed
//     whitespace, dash-family variants) match via evidenceInChunk's
//     normalisation fallback so the LLM's typical paraphrase
//     doesn't drop a structurally-correct edge.
//   - symmetric predicates are normalised so (from, to) is
//     lexicographically ordered — collapses RELATES_TO duplicates
//
// Returns kept proposals, the aggregate dropped count, and a
// per-reason breakdown so dashboards can spot which rule fires
// most. byReason values sum to dropped; the map is always
// non-nil (empty when no drops).
func validateProposals(in []EdgeProposal, entities []ResolvedEntity, content string) ([]EdgeProposal, int, map[string]int) {
	byReason := map[string]int{}
	if len(in) == 0 {
		return nil, 0, byReason
	}
	known := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		if e.ID != "" {
			known[e.ID] = struct{}{}
		}
	}
	out := make([]EdgeProposal, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	dropped := 0
	dropWithReason := func(reason string) {
		dropped++
		byReason[reason]++
	}
	for _, p := range in {
		p.From = strings.TrimSpace(p.From)
		p.To = strings.TrimSpace(p.To)
		p.Predicate = strings.ToUpper(strings.TrimSpace(p.Predicate))
		p.Evidence = strings.TrimSpace(p.Evidence)

		switch {
		case p.From == "" || p.To == "":
			dropWithReason(DropReasonEmptyEndpoint)
			continue
		case p.From == p.To:
			dropWithReason(DropReasonSelfLoop)
			continue
		}
		if _, ok := known[p.From]; !ok {
			dropWithReason(DropReasonUnknownFrom)
			continue
		}
		if _, ok := known[p.To]; !ok {
			dropWithReason(DropReasonUnknownTo)
			continue
		}
		if !isKnownPredicate(p.Predicate) {
			dropWithReason(DropReasonUnknownPredicate)
			continue
		}
		if p.Evidence == "" {
			dropWithReason(DropReasonEmptyEvidence)
			continue
		}
		if !evidenceInChunk(content, p.Evidence) {
			dropWithReason(DropReasonEvidenceNotInChunk)
			continue
		}
		if isSymmetricPredicate(p.Predicate) && p.From > p.To {
			p.From, p.To = p.To, p.From
		}
		key := p.From + "|" + p.Predicate + "|" + p.To
		if _, dup := seen[key]; dup {
			dropWithReason(DropReasonDuplicateTriple)
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out, dropped, byReason
}

// isKnownPredicate enforces the closed predicate vocabulary.
// Unknown predicates are dropped at this layer rather than
// allowed through — the schema doesn't enforce them, but
// dashboards/queries assume the closed set.
func isKnownPredicate(p string) bool {
	switch p {
	case persistence.PredicateMentionedIn,
		persistence.PredicateRelatesTo,
		persistence.PredicateQuotedPrice,
		persistence.PredicateChosenOver,
		persistence.PredicateMeasuredAs,
		persistence.PredicateDependsOn,
		persistence.PredicateSupersededBy,
		persistence.PredicateLocatedAt,
		persistence.PredicateOwnedBy,
		persistence.PredicateHasDeadline:
		return true
	}
	return false
}

// isSymmetricPredicate identifies predicates whose semantics
// don't depend on direction. RELATES_TO and MENTIONED_IN are
// the obvious candidates; the others (CHOSEN_OVER, OWNED_BY)
// are directional and must be preserved as-emitted.
func isSymmetricPredicate(p string) bool {
	switch p {
	case persistence.PredicateRelatesTo, persistence.PredicateMentionedIn:
		return true
	}
	return false
}

// evidenceInChunk reports whether evidence appears as a substring
// of content. Falls back to a normalised comparison when the
// strict substring check fails so the validator doesn't drop
// edges on cosmetic differences the LLM introduces routinely:
//
//   - Unicode smart quotes vs straight quotes (chunk has "X",
//     LLM emits "X" with U+201C / U+201D — same characters
//     visually, different code points).
//   - NFC vs NFD normalisation differences (chunk has é as a
//     single code point, LLM emits e + combining acute).
//   - Whitespace runs (chunk has two consecutive spaces or a
//     line break inside a quoted span; LLM collapses it to one
//     space).
//
// Strict-substring is tried first so the cheap path keeps
// dominating the common case. The fallback normalises BOTH
// sides identically so true cosmetic-only differences match
// while genuinely-different evidence still drops.
//
// Audit context (2026-05-25): 48% of knowledge_entities on the
// live DB were isolated (zero edges); per-chunk forensics showed
// the relationship LLM routinely emitted evidence quotes whose
// only difference from the chunk was the quote-character family.
// This helper closes that drop path without loosening the
// faithfulness contract.
func evidenceInChunk(content, evidence string) bool {
	if evidence == "" {
		return false
	}
	if strings.Contains(content, evidence) {
		return true
	}
	nc := normaliseForMatch(content)
	ne := normaliseForMatch(evidence)
	if ne == "" {
		return false
	}
	return strings.Contains(nc, ne)
}

// normaliseForMatch applies the cosmetic-difference normalisations
// described on evidenceInChunk. Idempotent — applying twice
// produces the same result as applying once.
//
// Cheap to call; runs in proportion to the input length. Both
// the chunk + the evidence are normalised on every check (no
// caching) because the chunk text varies per call and caching
// would otherwise be a hot-path memoisation surface the worker
// doesn't need.
func normaliseForMatch(s string) string {
	if s == "" {
		return s
	}
	// NFC composes decomposed characters first so e + combining
	// acute matches the precomposed é.
	s = norm.NFC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		// Quote canonicalisation. Both single- and double-quote
		// families collapse to their ASCII counterparts. The set
		// covers the variants the relationship LLM has been
		// observed to emit (curly + low-9 + prime variants).
		case '‘', '’', '‚', '‛', '′':
			r = '\''
		case '“', '”', '„', '‟', '″':
			r = '"'
		// Common dash variants → ASCII hyphen so quoted spans
		// containing em-dash / en-dash match either spelling.
		case '–', '—', '−':
			r = '-'
		// Ellipsis → three dots.
		case '…':
			b.WriteString("...")
			prevSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if prevSpace {
				continue
			}
			b.WriteRune(' ')
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
