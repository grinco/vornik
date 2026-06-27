package graph

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// responseCachePurposeKGExtract MUST match
// memory.ResponseCachePurposeKGExtract — both packages key into the
// same llm_response_cache table under this label. The const is
// duplicated rather than imported because the graph package
// pre-dates the memory package's dependency direction.
const responseCachePurposeKGExtract = "memory_kg_extract"

// ResponseCache is the narrow interface the Extractor consumes to
// skip the upstream LLM call when an identical (model, prompt)
// triple has already been answered. Structurally compatible with
// memory.ResponseCache; the concrete *memory.responseCacheRepo
// satisfies both via Go's structural typing.
//
// Nil disables caching — Extract behaves exactly as before.
type ResponseCache interface {
	Get(ctx context.Context, key string) (content string, promptTokens, completionTokens int, hit bool, err error)
	Put(ctx context.Context, key, model, purpose, content string, promptTokens, completionTokens int) error
}

//go:embed extractor_prompt.txt
var extractorSystemPrompt string

// Candidate is a raw entity proposal from the extractor stage,
// pre-resolution. The resolver decides whether each one becomes
// a brand-new knowledge_entities row or merges with an existing
// one; the orchestrator turns Candidate.CharStart/CharEnd into
// entity_mentions rows once a canonical entity ID is known.
type Candidate struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CharStart int    `json:"char_start"`
	CharEnd   int    `json:"char_end"`
	Surface   string `json:"surface"`
	Subtype   string `json:"subtype,omitempty"`
}

// ExtractMetrics carries token + model + outcome attribution back
// to the orchestrator so per-chunk extraction cost lands on the
// same dashboards as judge / hallucination spend. Outcome makes
// "extractor returned zero" visible per-chunk so dashboards can
// distinguish the LLM saying nothing (empty_response) from the
// LLM proposing candidates that all got filtered
// (dropped_all_invalid).
type ExtractMetrics struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	Outcome          string
}

// ExtractOutcome* are the stable labels under which the pipeline
// emits the per-chunk extractor result counter. Closed set so the
// Prometheus label cardinality stays bounded.
//
// Audit context (2026-05-25): live `vornik_test` showed 67% of
// chunks producing zero entity mentions, with sampled chunks
// containing clearly extractable in-vocab content. This metric
// makes the empty-rate trend measurable so future tuning (prompt
// few-shot, larger model) has a defensible before/after.
const (
	// ExtractOutcomeEmptyResponse — the LLM returned an empty
	// array (or a parseable but zero-candidate response). The
	// dominant failure mode in the audit; tuning target.
	ExtractOutcomeEmptyResponse = "empty_response"
	// ExtractOutcomeDroppedAllInvalid — the LLM proposed
	// candidates but every one was filtered by validateCandidates
	// (unknown type / empty name / bad span). Rare; suggests the
	// model is ignoring the closed vocabulary.
	ExtractOutcomeDroppedAllInvalid = "dropped_all_invalid"
	// ExtractOutcomeProduced — at least one valid candidate
	// survived to the resolver stage. The healthy path.
	ExtractOutcomeProduced = "produced"
)

// Extractor runs Stage 1 of the KG pipeline: pure named-entity
// recognition. No resolver decisions, no edges, no DB writes —
// this stage's only job is "what's mentioned in this chunk".
//
// The prompt is closed-vocabulary on entity type so a small OSS
// model (gpt-oss:20b, gemma3:9b) can hit the schema reliably.
// Recommended Model values: "gpt-oss:20b" (default), "gemma3:27b"
// for higher recall, "haiku-4.5" if hosted Anthropic is preferred.
type Extractor struct {
	// Client is the chat provider — in production the same
	// router that backs the executor / judge.
	Client chat.Provider
	// Model is the model identifier passed via ModelOverridable
	// when the provider supports it. Empty leaves the provider's
	// own default model in place.
	Model string
	// MaxAttempts caps LLM retry attempts on transient failures.
	// 0 → 3.
	MaxAttempts int
	// MaxChunkBytes truncates extreme chunks before sending to
	// the model. 0 → 8 KiB. Keeps small-model context windows
	// safe; the chunker already targets ≤2k tokens so truncation
	// here is a defence-in-depth.
	MaxChunkBytes int
	// Cache memoises (model, system+chunk prompt) → raw response so
	// re-runs over the same chunks skip the upstream LLM call.
	// Optional — nil disables. See llm-caching-design.md Phase E.
	Cache ResponseCache
}

// NewExtractor returns an Extractor with sane defaults. Callers
// can override fields after construction.
func NewExtractor(client chat.Provider, model string) *Extractor {
	return &Extractor{Client: client, Model: model, MaxAttempts: 3, MaxChunkBytes: 8 * 1024}
}

// Extract runs the extractor on a single chunk and returns the
// validated candidate list. Validation drops candidates with
// out-of-bounds offsets, unknown types, or empty names; the
// invalid count is reported via the metrics struct so quality
// regressions surface in dashboards. A zero-length result is
// VALID (the chunk had no extractable entities) and not an error.
func (e *Extractor) Extract(ctx context.Context, content string) ([]Candidate, *ExtractMetrics, error) {
	if e == nil || e.Client == nil {
		return nil, nil, fmt.Errorf("Extractor.Extract: client not configured")
	}
	if strings.TrimSpace(content) == "" {
		return nil, &ExtractMetrics{Model: e.Model}, nil
	}

	cap := e.MaxChunkBytes
	if cap <= 0 {
		cap = 8 * 1024
	}
	body := content
	if len(body) > cap {
		body = truncateUTF8Bytes(body, cap)
	}

	maxAttempts := e.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	msgs := []chat.Message{
		{Role: "system", Content: extractorSystemPrompt},
		{Role: "user", Content: "CHUNK:\n" + body},
	}
	client := pickModel(e.Client, e.Model)

	// Phase E response cache: skip the LLM on a hit. Cached content
	// is the raw response; stripJSONFence + parseCandidates re-run
	// on hit so JSON-shape evolutions remain effective.
	cacheKey := ""
	if e.Cache != nil {
		cacheKey = responseCacheKey(e.Model, responseCachePurposeKGExtract, msgs)
		if rawCached, pTok, cTok, hit, _ := e.Cache.Get(ctx, cacheKey); hit {
			cands, perr := parseCandidates(stripJSONFence(rawCached))
			if perr == nil {
				validated := validateCandidates(cands, body)
				cachedMetrics := &ExtractMetrics{
					Model:            e.Model,
					PromptTokens:     pTok,
					CompletionTokens: cTok,
					Outcome:          classifyExtractOutcome(len(cands), len(validated)),
				}
				return validated, cachedMetrics, nil
			}
			// Cached row no longer parseable — fall through to LLM.
		}
	}

	resp, err := completeWithRetry(ctx, client, msgs, maxAttempts)
	if err != nil {
		return nil, &ExtractMetrics{Model: e.Model}, fmt.Errorf("extractor LLM call failed: %w", err)
	}
	metrics := &ExtractMetrics{
		Model:            e.Model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
	if resp.Model != "" {
		metrics.Model = resp.Model
	}
	if len(resp.Choices) == 0 {
		return nil, metrics, fmt.Errorf("extractor returned no choices")
	}
	rawResp := resp.Choices[0].Message.Content
	raw := stripJSONFence(rawResp)
	cands, err := parseCandidates(raw)
	if err != nil {
		return nil, metrics, fmt.Errorf("extractor JSON parse: %w", err)
	}
	if e.Cache != nil && cacheKey != "" {
		_ = e.Cache.Put(ctx, cacheKey, e.Model, responseCachePurposeKGExtract,
			rawResp, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	validated := validateCandidates(cands, body)
	metrics.Outcome = classifyExtractOutcome(len(cands), len(validated))
	return validated, metrics, nil
}

// classifyExtractOutcome maps the (rawCandidates, validated)
// post-extract counts to the ExtractOutcome* label. Called from
// both the LLM and cache-hit paths so the metric reflects the
// same shape regardless of which produced the result.
func classifyExtractOutcome(rawCount, validatedCount int) string {
	switch {
	case rawCount == 0:
		return ExtractOutcomeEmptyResponse
	case validatedCount == 0:
		return ExtractOutcomeDroppedAllInvalid
	default:
		return ExtractOutcomeProduced
	}
}

// responseCacheKey hashes the (model, purpose, messages) triple
// into the canonical cache key. SHA-256 hex over a NUL-delimited
// fingerprint — chat content is JSON-serialised over the wire so
// NUL is collision-safe. Structurally identical to
// memory.ResponseCacheKey; duplicated to avoid an import cycle.
func responseCacheKey(model, purpose string, messages []chat.Message) string {
	h := sha256.New()
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(purpose))
	_, _ = h.Write([]byte{0})
	for _, m := range messages {
		_, _ = h.Write([]byte(m.Role))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(m.Content))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// parseCandidates accepts either a bare JSON array or a JSON
// object with a top-level "entities" / "candidates" field —
// some smaller models wrap arrays in objects despite the prompt.
// Returns an empty slice (not nil) on parse success with no
// entities, so the caller can distinguish "model spoke and saw
// nothing" from "didn't run".
func parseCandidates(raw string) ([]Candidate, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []Candidate{}, nil
	}
	if raw[0] == '[' {
		var arr []Candidate
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var wrap struct {
		Entities   []Candidate `json:"entities"`
		Candidates []Candidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Entities) > 0 {
		return wrap.Entities, nil
	}
	return wrap.Candidates, nil
}

// validateCandidates enforces:
//   - type ∈ closed vocabulary (drops unknown)
//   - non-empty name (drops empty)
//   - char_start/char_end within chunk bounds (drops out-of-range)
//   - surface matches chunk[char_start:char_end] when both are set
//     (clamps surface to the actual substring; doesn't drop)
//
// Drops are silent — the orchestrator logs aggregate counts. The
// goal of this layer is to make sure no malformed row reaches the
// resolver / DB.
func validateCandidates(in []Candidate, content string) []Candidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]Candidate, 0, len(in))
	contentLen := len(content)
	for _, c := range in {
		if !isKnownEntityType(c.Type) {
			continue
		}
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" {
			continue
		}
		if c.CharStart < 0 || c.CharEnd < c.CharStart || c.CharEnd > contentLen {
			// Out-of-range offsets — keep the entity but drop the
			// span. Mention insertion is offset-keyed; the
			// orchestrator will skip mention writes for entries
			// without a valid span.
			c.CharStart = 0
			c.CharEnd = 0
			c.Surface = ""
		} else if c.CharEnd > c.CharStart {
			actual := content[c.CharStart:c.CharEnd]
			if c.Surface == "" || c.Surface != actual {
				c.Surface = actual
			}
		}
		out = append(out, c)
	}
	return out
}

// isKnownEntityType returns true when t is one of the closed-set
// types declared in persistence/models.go. Unknown types are
// dropped at validation; the model gets one chance to use the
// vocabulary correctly.
func isKnownEntityType(t string) bool {
	switch t {
	case persistence.EntityTypePerson,
		persistence.EntityTypeVendor,
		persistence.EntityTypeProduct,
		persistence.EntityTypeDecision,
		persistence.EntityTypeEvent,
		persistence.EntityTypeDate,
		persistence.EntityTypePrice,
		persistence.EntityTypeLocation,
		persistence.EntityTypeTechnology,
		persistence.EntityTypeFact,
		persistence.EntityTypeOther:
		return true
	}
	return false
}
