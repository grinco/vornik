package memory

import (
	"fmt"
	"time"
)

// RetrievalRoutingConfig tunes the confidence-based retrieval routing
// feature (P3): the daemon-side retrieval_trust_verdict and its bounded,
// verdict-predicated DB widen. See
// https://docs.vornik.io
//
// All fields are inert unless a caller sets SearchOptions.Routing. The
// defaults here are the §3.2 "Defaults (shipped)" table; DefaultRetrievalRoutingConfig
// materialises them and applyDefaults fills any zero-valued knob.
type RetrievalRoutingConfig struct {
	// K is the top-K window over which the trust mean is computed.
	K int
	// MinResults is the floor result count for a non-low verdict.
	// INVARIANT: MinResults <= K (Validate returns a config-load error otherwise).
	MinResults int
	// HighThreshold: mean(trust_topK) >= this ⇒ high (unless age-capped).
	HighThreshold float64
	// LowThreshold: mean(trust_topK) < this ⇒ low (EXCLUSIVE boundary — a mean
	// exactly equal to LowThreshold is medium, not low; avoids boundary flapping).
	LowThreshold float64
	// WStatus/WConf/WFresh are the status-dominant trust weights (0.6/0.2/0.2).
	WStatus float64
	WConf   float64
	WFresh  float64
	// UnverifiedConfDiscount discounts (does NOT zero) the confidence term for
	// unverified chunks so fresh-unverified lands in medium rather than low.
	UnverifiedConfDiscount float64
	// NoTTLAgeCap: a no-TTL chunk (spec/decision) older than this has its
	// freshness floored to NoTTLStaleFreshness, and — when it is the TOP hit —
	// caps the aggregate verdict at medium.
	NoTTLAgeCap time.Duration
	// NoTTLStaleFreshness is the freshness floor applied to an aged no-TTL chunk.
	NoTTLStaleFreshness float64
	// MaxRounds bounds the verdict-predicated widen loop.
	MaxRounds int
	// WidenEnabled toggles the widen loop. Effective widening is
	// (SearchOptions.Routing && WidenEnabled): default true so widening ships
	// with Routing but can be disabled to get verdict-only behaviour.
	WidenEnabled bool
	// widenEnabledSet records whether WidenEnabled was explicitly wired, so
	// applyDefaults can distinguish "left false" from "unset" (default true).
	widenEnabledSet bool
	// Enabled is the master routing kill-switch (review M-2 / §6 "reversible
	// per-caller"). When false the searcher ignores SearchOptions.Routing and
	// behaves exactly as routing-off (no verdict/guidance/trust fields, no
	// widen) — a config-only off switch so a rollout can be reverted without a
	// code change. Default true (the feature is GREEN); set
	// memory.retrieval_routing.enabled=false to disable.
	Enabled bool
	// enabledSet mirrors widenEnabledSet for the Enabled toggle.
	enabledSet bool
}

// SetWidenEnabled explicitly wires WidenEnabled so applyDefaults does not
// overwrite an intentional false with the default true. Use this (not the
// bare field) when the value comes from operator config.
func (c *RetrievalRoutingConfig) SetWidenEnabled(v bool) {
	c.WidenEnabled = v
	c.widenEnabledSet = true
}

// SetEnabled explicitly wires the master Enabled toggle so applyDefaults does
// not overwrite an intentional false with the default true.
func (c *RetrievalRoutingConfig) SetEnabled(v bool) {
	c.Enabled = v
	c.enabledSet = true
}

// DefaultRetrievalRoutingConfig returns the §3.2 shipped defaults.
func DefaultRetrievalRoutingConfig() RetrievalRoutingConfig {
	return RetrievalRoutingConfig{
		K:                      5,
		MinResults:             1,
		HighThreshold:          0.70,
		LowThreshold:           0.40,
		WStatus:                0.6,
		WConf:                  0.2,
		WFresh:                 0.2,
		UnverifiedConfDiscount: 0.5,
		NoTTLAgeCap:            180 * 24 * time.Hour,
		NoTTLStaleFreshness:    0.3,
		MaxRounds:              3,
		WidenEnabled:           true,
		widenEnabledSet:        true,
		Enabled:                true,
		enabledSet:             true,
	}
}

// applyDefaults fills any zero-valued knob from the shipped defaults,
// returning the completed config. Non-zero operator values win. Weights
// are treated as a group: if all three are zero, the default 0.6/0.2/0.2
// is applied; a partially-specified weight set is left as-is (Validate
// rejects nonsensical combinations).
func (c RetrievalRoutingConfig) applyDefaults() RetrievalRoutingConfig {
	d := DefaultRetrievalRoutingConfig()
	out := c
	if out.K <= 0 {
		out.K = d.K
	}
	if out.MinResults <= 0 {
		out.MinResults = d.MinResults
	}
	if out.HighThreshold <= 0 {
		out.HighThreshold = d.HighThreshold
	}
	if out.LowThreshold <= 0 {
		out.LowThreshold = d.LowThreshold
	}
	if out.WStatus == 0 && out.WConf == 0 && out.WFresh == 0 {
		out.WStatus, out.WConf, out.WFresh = d.WStatus, d.WConf, d.WFresh
	}
	if out.UnverifiedConfDiscount <= 0 {
		out.UnverifiedConfDiscount = d.UnverifiedConfDiscount
	}
	if out.NoTTLAgeCap <= 0 {
		out.NoTTLAgeCap = d.NoTTLAgeCap
	}
	if out.NoTTLStaleFreshness <= 0 {
		out.NoTTLStaleFreshness = d.NoTTLStaleFreshness
	}
	if out.MaxRounds <= 0 {
		out.MaxRounds = d.MaxRounds
	}
	if !out.widenEnabledSet {
		out.WidenEnabled = d.WidenEnabled
	}
	if !out.enabledSet {
		out.Enabled = d.Enabled
	}
	return out
}

// Validate enforces the config invariants. The load-time check the LLD
// pins is MinResults <= K; it is evaluated against the EFFECTIVE (defaulted)
// values so an operator who sets only minResults still gets a meaningful
// error against the default K.
func (c RetrievalRoutingConfig) Validate() error {
	eff := c.applyDefaults()
	if eff.MinResults > eff.K {
		return fmt.Errorf("memory.retrieval_routing: min_results (%d) must be <= k (%d)", eff.MinResults, eff.K)
	}
	return nil
}

// Config holds configuration for the project memory system.
type Config struct {
	// Enabled controls whether the memory system is active.
	Enabled bool

	// EmbeddingProvider selects the embedding transport. Empty / "openai"
	// keeps the historical OpenAI-compatible /v1/embeddings path.
	// "bedrock" uses AWS Bedrock's native InvokeModel API.
	EmbeddingProvider string

	// EmbeddingEndpoint is the OpenAI-compatible base URL for embedding requests.
	// Falls back to the LLM endpoint (from executor config) when empty.
	EmbeddingEndpoint string

	// EmbeddingAPIKey is the API key for the embedding endpoint.
	EmbeddingAPIKey string

	// EmbeddingModel is the model name to use for embeddings, e.g. "text-embedding-3-small".
	EmbeddingModel string

	// BedrockRegion is the AWS region used when EmbeddingProvider=="bedrock".
	BedrockRegion string

	// EmbeddingDimension is the vector dimension produced by the embedding model.
	// Default: 1536 (matches text-embedding-3-small).
	EmbeddingDimension int

	// ChunkTokens is the approximate token count per chunk (1 token ≈ 4 chars).
	// Default: 512.
	ChunkTokens int

	// ChunkOverlap is the overlap in approximate tokens between adjacent chunks.
	// Default: 64.
	ChunkOverlap int

	// WorkerConcurrency is the number of embed queue worker goroutines.
	// Default: 2.
	WorkerConcurrency int

	// EmbeddingCacheEnabled turns on the postgres-backed embedding
	// cache (LLM caching design Phase D). When true, identical
	// (content, model) pairs serve from the embedding_cache table
	// instead of round-tripping to the upstream API. Default off
	// because the table needs migration 41 applied; operators
	// opt in once the schema is current.
	EmbeddingCacheEnabled bool

	// ResponseCacheEnabled turns on the postgres-backed full-response
	// cache (LLM caching design Phase E). When true, the Titler /
	// Classifier / KG Extractor memoise raw responses keyed on
	// (model, purpose, prompt) so re-runs over the same chunks skip
	// the upstream LLM call. Default off because the table needs
	// migration 47 applied.
	ResponseCacheEnabled bool

	// PricingFunc costs a (model, prompt_tokens, completion_tokens)
	// triple in USD. When wired, the Phase E response cache's
	// CacheStats populates TotalSavingsUSD by summing each row's
	// (prompt + completion) cost × hit_count. Optional — nil leaves
	// TotalSavingsUSD at 0 so operators on un-priced models still
	// see hit volume. Matches pricing.Table.CostUSD shape so the
	// service container can wire it directly.
	PricingFunc func(model string, promptTokens, completionTokens int) float64

	// RetrievalRouting tunes the confidence-based retrieval routing
	// feature (verdict + verdict-predicated widen). Inert unless a
	// caller sets SearchOptions.Routing. Zero-valued knobs fall back
	// to DefaultRetrievalRoutingConfig via the searcher.
	RetrievalRouting RetrievalRoutingConfig
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		EmbeddingDimension: 1536,
		ChunkTokens:        512,
		ChunkOverlap:       64,
		WorkerConcurrency:  2,
	}
}
