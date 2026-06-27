package featuredoctor

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// ConfigReader returns the effective value of a dotted config gate key
// from the running daemon's config (defaults applied). bool/string today.
type ConfigReader interface {
	GateValue(key string) (any, bool) // value, present
}

// ModelPinger reports whether a chat/embedding model id is reachable.
type ModelPinger interface {
	Reachable(ctx context.Context, modelID string) bool
}

// EmbeddingProber reports whether an embedding model is reachable at a
// dedicated embedding endpoint. This is a DIFFERENT surface from
// ModelPinger: embeddings route to memory.embedding_endpoint (typically a
// local Ollama / TEI server), whose models the chat-provider catalog that
// ModelPinger consults never lists. Incident 2026-06-12: a locally-served
// bge-m3 was mis-reported "not reachable" because the doctor only checked
// the chat catalog. When an embedding endpoint is configured, the check
// probes it via this interface instead.
type EmbeddingProber interface {
	// ProbeEmbedding attempts a minimal embedding of a sentinel string
	// against endpoint/model (apiKey may be empty) and reports whether a
	// non-empty vector came back.
	ProbeEmbedding(ctx context.Context, endpoint, apiKey, model string) bool
}

// TaskLister reports whether any task is currently RUNNING or LEASED
// (the no-restart-during-jobs guard).
type TaskLister interface {
	HasActiveTasks(ctx context.Context) (bool, error)
}

// TradingSeriesFinding is one anomaly in a project's trading equity series,
// flattened from internal/trading/seriescheck so featuredoctor stays
// decoupled from that package.
type TradingSeriesFinding struct {
	ProjectID string
	Code      string
	Severity  string
	Detail    string
}

// TradingSeriesProbe validates the trading equity time-series
// (trading_positions_snapshots) for every trading-enabled project and returns
// per-project findings. Implemented by an adapter in the api layer that owns
// the snapshot repo + project registry; nil when trading isn't wired.
type TradingSeriesProbe interface {
	ValidateSeries(ctx context.Context) ([]TradingSeriesFinding, error)
}

// Deps is the narrow read surface the feature checks need. Each field is
// an interface so tests supply stubs (see stubInstinctRepo et al).
type Deps struct {
	Config     ConfigReader
	Instincts  persistence.InstinctRepository
	Outcomes   persistence.ExecutionStepOutcomeRepository
	Models     ModelPinger
	Embeddings EmbeddingProber
	Tasks      TaskLister
	Trading    TradingSeriesProbe
	SecretsDir string
	Logger     zerolog.Logger
}
