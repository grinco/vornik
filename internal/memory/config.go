package memory

// Config holds configuration for the project memory system.
type Config struct {
	// Enabled controls whether the memory system is active.
	Enabled bool

	// EmbeddingEndpoint is the OpenAI-compatible base URL for embedding requests.
	// Falls back to the LLM endpoint (from executor config) when empty.
	EmbeddingEndpoint string

	// EmbeddingAPIKey is the API key for the embedding endpoint.
	EmbeddingAPIKey string

	// EmbeddingModel is the model name to use for embeddings, e.g. "text-embedding-3-small".
	EmbeddingModel string

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
