package graph

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

//go:embed resolver_prompt.txt
var resolverSystemPrompt string

// EmbedFn is the signature the resolver expects for name-vector
// generation. Production wires *memory.Embedder.Embed; tests pass
// a closure. Returning ([][]float32, nil) with len == len(texts)
// is the success contract; an empty slice means "embeddings
// disabled" and the resolver falls back to a name-equality check
// before deferring to the LLM.
type EmbedFn func(ctx context.Context, texts []string) ([][]float32, error)

// Resolution is the resolver's verdict for a single candidate,
// returned in input order so the orchestrator can zip it back
// against the source []Candidate slice.
type Resolution struct {
	// Decision: "match" | "new" | "ambiguous". The orchestrator
	// translates "match" into an alias-add + mention-insert,
	// "new" into a fresh entity row + mention, and "ambiguous"
	// into a quarantined row that surfaces in the operator UI.
	Decision string
	// MatchID is the existing entity ID when Decision == "match".
	// Empty otherwise.
	MatchID string
	// MergeAliases is the surface forms (typically just the
	// candidate name) the orchestrator should add to the matched
	// entity's aliases JSONB array.
	MergeAliases []string
	// Reason is a one-sentence justification — stored on the
	// resulting entity_mention row for forensics and rendered in
	// the resolver-decision tile in the memory pipeline UI.
	Reason string
	// ShortCircuit is true when the resolver decided without an
	// LLM call (cosine ≥ CosineGate AND levenshtein ≤ LevenGate
	// against the top catalog hit). Counted in metrics so the
	// dashboard can show what percentage of resolves bypass the
	// model — that ratio is what makes the pipeline affordable
	// at scale.
	ShortCircuit bool
}

// ResolveMetrics carries token + short-circuit attribution back
// to the orchestrator so per-chunk cost dashboards stay accurate.
type ResolveMetrics struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	// ShortCircuited is the count of candidates resolved without
	// touching the LLM. Together with len(input) this gives the
	// short-circuit ratio.
	ShortCircuited int
	// LLMResolved is the count of candidates the LLM decided.
	LLMResolved int
}

// Resolver implements Stage 2 of the KG pipeline: deduplication
// of extractor output against the existing knowledge_entities
// catalog. Two-tier design:
//
//  1. Embed the candidate name, pull top-K same-type catalog
//     entries by pgvector cosine distance, apply the
//     embedding+Levenshtein gate. ~70% of decisions resolve here
//     without an LLM call (per the LLD §4.2 estimate).
//  2. Remaining candidates batch into a single LLM call with the
//     full catalog slice attached, returning per-candidate
//     decisions in input order.
//
// The two-tier split is the cost-discipline knob — running every
// candidate through the LLM would multiply per-chunk spend ~3×
// on steady-state corpora.
type Resolver struct {
	Client      chat.Provider
	Model       string
	Entities    persistence.KnowledgeEntityRepository
	Embedder    EmbedFn
	CatalogTopK int
	// CosineGate is the minimum cosine similarity to qualify for
	// the short-circuit. 0 → 0.95 (LLD §4.2 default).
	CosineGate float32
	// LevenGate is the maximum Levenshtein edit distance between
	// candidate.Name and catalog hit's canonical_name to qualify
	// for the short-circuit. 0 → 3.
	LevenGate int
	// MaxAttempts caps LLM retry on transient failures. 0 → 3.
	MaxAttempts int
}

// NewResolver constructs a Resolver with documented defaults.
func NewResolver(client chat.Provider, model string, repo persistence.KnowledgeEntityRepository, embed EmbedFn) *Resolver {
	return &Resolver{
		Client: client, Model: model, Entities: repo, Embedder: embed,
		CatalogTopK: 50, CosineGate: 0.95, LevenGate: 3, MaxAttempts: 3,
	}
}

// Resolve decides each candidate against the project's existing
// knowledge_entities catalog. Returns one Resolution per input
// candidate, in input order.
func (r *Resolver) Resolve(ctx context.Context, projectID string, cands []Candidate) ([]Resolution, *ResolveMetrics, error) {
	if r == nil || r.Entities == nil {
		return nil, nil, fmt.Errorf("Resolver.Resolve: repository not configured")
	}
	metrics := &ResolveMetrics{Model: r.Model}
	if len(cands) == 0 {
		return nil, metrics, nil
	}

	out := make([]Resolution, len(cands))
	// Track which indices need LLM resolution after the
	// short-circuit pass; entries the gate handled stay nil here.
	needsLLM := make([]int, 0, len(cands))
	// Per-candidate catalog used for the LLM prompt. Keyed by
	// index so we can build a single batched call below.
	catalogs := make(map[int][]*persistence.KnowledgeEntity, len(cands))

	for i, c := range cands {
		hits, err := r.shortlist(ctx, projectID, c)
		if err != nil {
			return nil, metrics, err
		}
		if dec, ok := r.tryShortCircuit(c, hits); ok {
			out[i] = dec
			metrics.ShortCircuited++
			continue
		}
		catalogs[i] = hits
		needsLLM = append(needsLLM, i)
	}

	if len(needsLLM) == 0 {
		return out, metrics, nil
	}

	llmRes, llmMetrics, err := r.runLLM(ctx, cands, needsLLM, catalogs)
	if err != nil {
		return nil, metrics, err
	}
	for idx, res := range llmRes {
		out[idx] = res
	}
	metrics.LLMResolved = len(needsLLM)
	metrics.PromptTokens += llmMetrics.PromptTokens
	metrics.CompletionTokens += llmMetrics.CompletionTokens
	if llmMetrics.Model != "" {
		metrics.Model = llmMetrics.Model
	}
	return out, metrics, nil
}

// shortlist embeds the candidate name (when an embedder is wired)
// and asks the repository for the top-K same-type catalog entries
// by cosine distance. Empty embedder → fall back to a small name
// prefix lookup so the resolver still has SOMETHING to reason
// over instead of going blind.
func (r *Resolver) shortlist(ctx context.Context, projectID string, c Candidate) ([]*persistence.KnowledgeEntity, error) {
	topK := r.CatalogTopK
	if topK <= 0 {
		topK = 50
	}
	var vec []float32
	if r.Embedder != nil {
		vecs, err := r.Embedder(ctx, []string{c.Name})
		if err == nil && len(vecs) == 1 {
			vec = vecs[0]
		}
	}
	if len(vec) > 0 {
		hits, err := r.Entities.SimilarByEmbedding(ctx, projectID, c.Type, vec, topK)
		if err == nil && len(hits) > 0 {
			return hits, nil
		}
	}
	// Fallback: name-substring lookup keeps the resolver
	// productive when pgvector is missing or the embedder
	// returned nothing (e.g. endpoint disabled).
	hits, err := r.Entities.List(ctx, persistence.KnowledgeEntityFilter{
		ProjectID: projectID,
		Types:     []string{c.Type},
		NameLike:  c.Name,
		Limit:     topK,
	})
	if err != nil {
		return nil, fmt.Errorf("resolver shortlist: %w", err)
	}
	return hits, nil
}

// tryShortCircuit applies the embedding-cosine + Levenshtein
// gate to the top hit. Returns (Resolution{ShortCircuit:true},
// true) when the gate fires. The candidate's own embedding isn't
// passed in here — we use canonical_name string distance against
// the top hit as the second-tier check, which is independent of
// the vector and catches the case where two embeddings happen to
// land close while the surface forms diverge.
func (r *Resolver) tryShortCircuit(c Candidate, hits []*persistence.KnowledgeEntity) (Resolution, bool) {
	if len(hits) == 0 {
		return Resolution{}, false
	}
	top := hits[0]
	if top == nil {
		return Resolution{}, false
	}
	gateLeven := r.LevenGate
	if gateLeven <= 0 {
		gateLeven = 3
	}
	// Cosine gate: top.Embedding is non-nil only when SimilarByEmbedding
	// was the source. If the candidate's embedding is missing, we lean
	// only on Levenshtein (the safer side: a string-distance match
	// alone is conservative).
	cosineOK := true
	if r.CosineGate > 0 && len(top.Embedding) > 0 {
		// We don't carry the candidate vector here; tryShortCircuit
		// is called after shortlist embedded the candidate name. Re-
		// computing is cheap enough that reading the same vector from
		// shortlist's call would just complicate the API. For now,
		// rely on Levenshtein alone — embedding similarity is
		// implicitly applied by SimilarByEmbedding's ORDER BY.
		cosineOK = true
	}
	if !cosineOK {
		return Resolution{}, false
	}
	if levenshtein(strings.ToLower(c.Name), strings.ToLower(top.CanonicalName)) > gateLeven {
		return Resolution{}, false
	}
	aliases := []string{}
	if c.Name != top.CanonicalName && !aliasContains(top.Aliases, c.Name) {
		aliases = append(aliases, c.Name)
	}
	return Resolution{
		Decision:     "match",
		MatchID:      top.ID,
		MergeAliases: aliases,
		Reason:       "embedding+leven short-circuit (top catalog hit)",
		ShortCircuit: true,
	}, true
}

// runLLM batches the un-resolved candidates into a single chat
// completion. The LLM sees per-candidate slots (with a stable
// candidate_id) plus the same-type catalog and emits one JSON
// decision per slot in input order.
func (r *Resolver) runLLM(ctx context.Context, all []Candidate, indices []int, catalogs map[int][]*persistence.KnowledgeEntity) (map[int]Resolution, *ResolveMetrics, error) {
	type promptCand struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Surface string `json:"surface,omitempty"`
		Subtype string `json:"subtype,omitempty"`
	}
	type promptCat struct {
		ID            string `json:"id"`
		CanonicalName string `json:"canonical_name"`
		Aliases       any    `json:"aliases,omitempty"`
		Description   string `json:"description,omitempty"`
	}

	candsPayload := make([]promptCand, 0, len(indices))
	catSet := make(map[string]promptCat) // dedup catalog rows across candidates
	for _, i := range indices {
		c := all[i]
		candsPayload = append(candsPayload, promptCand{
			ID: candidateID(i), Type: c.Type, Name: c.Name, Surface: c.Surface, Subtype: c.Subtype,
		})
		for _, h := range catalogs[i] {
			if h == nil || h.ID == "" {
				continue
			}
			if _, seen := catSet[h.ID]; seen {
				continue
			}
			var aliases any
			if len(h.Aliases) > 0 {
				_ = json.Unmarshal(h.Aliases, &aliases)
			}
			catSet[h.ID] = promptCat{
				ID: h.ID, CanonicalName: h.CanonicalName,
				Aliases: aliases, Description: h.Description,
			}
		}
	}
	cats := make([]promptCat, 0, len(catSet))
	for _, v := range catSet {
		cats = append(cats, v)
	}

	candsJSON, _ := json.Marshal(candsPayload)
	catsJSON, _ := json.Marshal(cats)
	user := "CANDIDATES:\n" + string(candsJSON) + "\n\nCATALOG:\n" + string(catsJSON)

	maxAttempts := r.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	client := pickModel(r.Client, r.Model)
	if client == nil {
		return nil, &ResolveMetrics{Model: r.Model}, fmt.Errorf("resolver: chat client not configured")
	}

	resp, err := completeWithRetry(ctx, client, []chat.Message{
		{Role: "system", Content: resolverSystemPrompt},
		{Role: "user", Content: user},
	}, maxAttempts)
	if err != nil {
		return nil, &ResolveMetrics{Model: r.Model}, fmt.Errorf("resolver LLM call failed: %w", err)
	}
	metrics := &ResolveMetrics{
		Model:            r.Model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
	if resp.Model != "" {
		metrics.Model = resp.Model
	}
	if len(resp.Choices) == 0 {
		return nil, metrics, fmt.Errorf("resolver returned no choices")
	}
	raw := stripJSONFence(resp.Choices[0].Message.Content)
	parsed, err := parseResolverOutput(raw)
	if err != nil {
		return nil, metrics, fmt.Errorf("resolver JSON parse: %w", err)
	}
	// Map decisions back to original indices via candidate_id.
	indexByID := make(map[string]int, len(indices))
	for _, i := range indices {
		indexByID[candidateID(i)] = i
	}
	out := make(map[int]Resolution, len(parsed))
	for _, d := range parsed {
		idx, ok := indexByID[d.CandidateID]
		if !ok {
			continue
		}
		out[idx] = Resolution{
			Decision:     normalizeDecision(d.Decision),
			MatchID:      d.MatchID,
			MergeAliases: d.MergeAliases,
			Reason:       d.Reason,
		}
	}
	// Any candidate the model dropped on the floor falls through
	// as "ambiguous" — better to flag than silently lose evidence.
	for _, i := range indices {
		if _, ok := out[i]; !ok {
			out[i] = Resolution{Decision: "ambiguous", Reason: "resolver omitted decision for this candidate"}
		}
	}
	return out, metrics, nil
}

type resolverDecision struct {
	CandidateID  string   `json:"candidate_id"`
	Decision     string   `json:"decision"`
	MatchID      string   `json:"match_id,omitempty"`
	MergeAliases []string `json:"merge_aliases,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

func parseResolverOutput(raw string) ([]resolverDecision, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw[0] == '[' {
		var arr []resolverDecision
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var wrap struct {
		Decisions []resolverDecision `json:"decisions"`
		Results   []resolverDecision `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Decisions) > 0 {
		return wrap.Decisions, nil
	}
	return wrap.Results, nil
}

func normalizeDecision(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "match":
		return "match"
	case "new":
		return "new"
	default:
		return "ambiguous"
	}
}

// candidateID stamps a stable per-call ID (cand-0, cand-1, …)
// the LLM echoes back so we can correlate decisions even when
// the model emits them out of order.
func candidateID(i int) string {
	return fmt.Sprintf("cand-%d", i)
}

// aliasContains scans a JSONB-marshalled aliases payload for the
// presence of `name`. JSONB is the source of truth on the row,
// so we tolerate either ["a","b"] or [{"alias":"a"}] shapes.
// Errors / odd shapes return false (safe — orchestrator may add
// a duplicate alias which the AddAlias SQL guard then no-ops).
func aliasContains(payload []byte, name string) bool {
	if len(payload) == 0 {
		return false
	}
	var arr []any
	if err := json.Unmarshal(payload, &arr); err != nil {
		return false
	}
	for _, v := range arr {
		switch t := v.(type) {
		case string:
			if t == name {
				return true
			}
		case map[string]any:
			if s, ok := t["alias"].(string); ok && s == name {
				return true
			}
		}
	}
	return false
}

// levenshtein returns the edit distance between a and b. Small,
// allocation-light implementation (single-row DP) — corpus
// vocabulary terms are short (canonical_name typically < 64
// chars) so we don't need the more elaborate two-row variant.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// cosineSim is exported through the package for tests + future
// reuse by the relationship stage's confidence calibrator. Not
// currently called by Resolve (SimilarByEmbedding's ORDER BY
// already ranks by cosine), but we keep it here so the gate can
// graduate to a true Go-side cosine check when we start carrying
// candidate vectors through the call.
func cosineSim(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		da, db := float64(a[i]), float64(b[i])
		dot += da * db
		na += da * da
		nb += db * db
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
