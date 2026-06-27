package intentjudge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
)

// ResponseCachePurposeLLMRefiner is the purpose tag used as the
// second key axis on llm_response_cache rows produced by this
// package. Distinct from memory_titler / memory_classifier /
// memory_kg_extract so each call site's eviction + stats can
// scope by purpose without scanning the whole table.
const ResponseCachePurposeLLMRefiner = "intentjudge_llm_refiner"

// ResponseCache is the narrow interface the LLMRefiner consumes
// to skip the upstream LLM call when an identical (model,
// purpose, prompt) triple has already been answered.
// Structurally compatible with memory.ResponseCache; defined
// locally so intentjudge doesn't import the memory package
// (matches the graph/extractor.go pattern).
//
// Nil disables caching — Refine behaves exactly as before.
type ResponseCache interface {
	Get(ctx context.Context, key string) (content string, promptTokens, completionTokens int, hit bool, err error)
	Put(ctx context.Context, key, model, purpose, content string, promptTokens, completionTokens int) error
}

// responseCacheKey hashes the (model, purpose, messages) triple
// into the canonical cache key. SHA-256 hex over a NUL-delimited
// fingerprint — structurally identical to memory.ResponseCacheKey
// and graph.responseCacheKey, duplicated to avoid an import on
// the memory package.
//
// Cache invalidation on prompt drift: if systemPrompt is edited
// in source, the hash changes and the cache misses on the next
// call. No explicit invalidation needed.
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

// LLMRefiner produces a refined Verdict for a tool call by
// asking an LLM to re-evaluate the heuristic's output. Runs
// async on the dispatcher side — the heuristic verdict gates
// the tool call immediately; the LLM result lands later via
// the persistence repo for calibration analyses.
//
// The refiner is INTENTIONALLY simple — single LLM call, no
// tool access in this slice (the design doc proposes giving
// the LLM read-only tools to fetch context; deferred). Operators
// who want richer refinement can extend Refine to inject a
// fetched-context block.
type LLMRefiner struct {
	// Provider is the chat client the refiner talks through.
	// Production wires this to the dispatcher's chat router
	// with a small/cheap model id (the refiner doesn't need a
	// reasoning model — it's pattern-matching tool intent
	// against risk rubric).
	Provider chat.Provider

	// Model overrides the router default for refiner calls.
	// Empty leaves the router's default in place. Recommended:
	// a small OSS classifier (gpt-oss-20b, gemma-4-26b, etc.).
	Model string

	// TimeoutSeconds caps a single refinement call. Default 15s
	// — refinement is async so a slow model never blocks the
	// dispatcher, but a runaway call still needs a ceiling so
	// we don't pile up goroutines.
	TimeoutSeconds int

	// Cache memoises (model, system+user prompt) → raw JSON
	// response so repeated (tool, args, heuristic) triples skip
	// the upstream LLM call. Optional — nil disables, identical
	// behaviour to pre-cache. Wired in via the service container
	// from c.memoryManager.ResponseCache (same instance the
	// memory-trio uses). See llm-caching-design.md Phase E +
	// expansion arc.
	Cache ResponseCache
}

// systemPrompt is the refiner's instruction block. Intentionally
// tight — the LLM only needs to emit JSON matching `llmResponse`;
// any free-form reasoning lives in the `reasoning` field.
const systemPrompt = `You are a security reviewer for an AI agent system.
Given a tool call (name + JSON arguments) and a heuristic risk verdict,
return a refined JSON verdict.

Risk levels: critical / high / medium / low.
Recommendations: deny / review / approve.
Confidence: 0.0 to 1.0.

Output a single JSON object with these fields:
  {
    "risk": "<critical|high|medium|low>",
    "confidence": <0.0..1.0>,
    "recommendation": "<deny|review|approve>",
    "reasoning": "<one to three sentences explaining your verdict>"
  }

Be brief. Do not output any text outside the JSON object.`

// llmResponse is the wire shape we expect back. Fields stay
// lower-case to match the system prompt.
type llmResponse struct {
	Risk           string  `json:"risk"`
	Confidence     float64 `json:"confidence"`
	Recommendation string  `json:"recommendation"`
	Reasoning      string  `json:"reasoning"`
}

// Refine asks the LLM to re-evaluate the heuristic verdict and
// returns a refined Verdict tagged with Tier=TierLLM. Returns
// (nil, err) on any transport or parsing failure — callers
// persist only successful refinements (the heuristic verdict
// is what stayed authoritative for the tool-call decision).
func (r *LLMRefiner) Refine(ctx context.Context, tool, argsJSON string, heuristic Verdict) (*Verdict, error) {
	if r == nil || r.Provider == nil {
		return nil, fmt.Errorf("llmrefiner: not configured")
	}
	timeout := time.Duration(r.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	user := fmt.Sprintf(`Tool: %s
Arguments: %s
Heuristic verdict:
  risk: %s
  confidence: %.2f
  recommendation: %s
  reasoning: %s

Refine the verdict. If the heuristic was right, return the same risk
level (but you may adjust confidence). If you disagree, explain why
in the reasoning field.`,
		tool, argsJSON,
		heuristic.Risk, heuristic.Confidence, heuristic.Recommendation,
		heuristic.Reasoning,
	)

	provider := r.Provider
	if r.Model != "" {
		if o, ok := provider.(chat.ModelOverridable); ok {
			provider = o.WithModel(r.Model)
		}
	}

	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: user},
	}
	cacheModel := r.Model
	// Cache key uses an empty model string when r.Model is unset
	// so swap-default rollouts share the cache only by accident.
	// Same convention the memory-trio uses.
	cacheKey := responseCacheKey(cacheModel, ResponseCachePurposeLLMRefiner, messages)
	cacheHit := false
	var content string
	var elapsed time.Duration
	if r.Cache != nil {
		// Errors are swallowed — a broken cache must never stall
		// dispatcher refinement. Same contract as memory.Titler /
		// memory.Classifier.
		if hitContent, _, _, hit, _ := r.Cache.Get(callCtx, cacheKey); hit {
			content = strings.TrimSpace(hitContent)
			cacheHit = true
		}
	}
	if !cacheHit {
		start := time.Now()
		resp, err := provider.Complete(callCtx, messages)
		if err != nil {
			return nil, fmt.Errorf("llmrefiner: chat call: %w", err)
		}
		elapsed = time.Since(start)
		if resp == nil || len(resp.Choices) == 0 {
			return nil, fmt.Errorf("llmrefiner: empty response")
		}
		content = strings.TrimSpace(resp.Choices[0].Message.Content)
	}
	// Models sometimes wrap JSON in ```json ... ``` despite the
	// prompt telling them not to. Strip the fence so we don't
	// fail to parse perfectly-good output.
	content = stripJSONFence(content)
	var parsed llmResponse
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("llmrefiner: parse response %q: %w", content, err)
	}

	refined := Verdict{
		Tool:           tool,
		IntentSummary:  heuristic.IntentSummary, // keep the heuristic summary
		Risk:           Risk(strings.ToLower(parsed.Risk)),
		Confidence:     clamp01(parsed.Confidence),
		Recommendation: Recommendation(strings.ToLower(parsed.Recommendation)),
		Reasoning:      parsed.Reasoning,
		Tier:           TierLLM,
		LatencyMs:      elapsed.Milliseconds(),
	}
	// Reject obviously-malformed outputs rather than silently
	// downgrade to the typed-zero values. The caller treats an
	// error as "stick with heuristic"; that's the right default
	// when the LLM emits garbage.
	if rank(refined.Risk) == 0 {
		return nil, fmt.Errorf("llmrefiner: invalid risk %q", parsed.Risk)
	}
	switch refined.Recommendation {
	case RecommendDeny, RecommendReview, RecommendApprove:
		// ok
	default:
		return nil, fmt.Errorf("llmrefiner: invalid recommendation %q", parsed.Recommendation)
	}
	// Cache only fully-validated responses so a malformed reply
	// doesn't poison the cache. The stored content is the parse-
	// ready string (post fence-strip) so cache hits skip both the
	// network round-trip AND the fence stripping. Put errors are
	// swallowed — same broken-cache discipline as Get.
	if r.Cache != nil && !cacheHit {
		_ = r.Cache.Put(callCtx, cacheKey, cacheModel, ResponseCachePurposeLLMRefiner, content, 0, 0)
	}
	return &refined, nil
}

// stripJSONFence removes a `json` markdown code fence around the
// LLM's response if one is present. Lenient — handles both
// ```json\n...\n``` and bare ```\n...\n``` shapes.
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence (with optional language tag).
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
