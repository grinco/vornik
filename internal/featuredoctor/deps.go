package featuredoctor

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
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
	// against the effective memory embedding config and reports whether a
	// non-empty vector came back.
	ProbeEmbedding(ctx context.Context, cfg memory.Config) bool
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

// DurabilityReport is the outcome of probing the active store's commit
// durability contract (2026-09-13 config-assistant review R6): SQLite must
// run with synchronous=FULL, Postgres with synchronous_commit=on. The
// config-assistant feature refuses to enable when the contract cannot be
// established, with no silent single-file fallback.
type DurabilityReport struct {
	Driver string // "sqlite" | "postgres" | ""
	OK     bool
	Detail string
}

// DurabilityProber reports whether the active store honours the durable
// commit contract the config-apply journal relies on. Implemented by an
// adapter in the api layer that owns the *sql.DB; nil when not wired.
type DurabilityProber interface {
	ProbeDurability(ctx context.Context) DurabilityReport
}

// A2APeerLister names the configured a2a.peers keys so the
// architect-consult feature can confirm its configured peer exists.
type A2APeerLister interface {
	A2APeerNames() []string
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
	// RoleLibraryDir is the daemon's deployed configs root (the same
	// directory rolelibrary.Load(dir) appends "role-library" onto) —
	// the composer feature's "≥1 role-library entry" prereq (task
	// 1.1b) reads it directly rather than through a repository
	// interface, mirroring the SecretsDir convention above.
	RoleLibraryDir string
	Logger         zerolog.Logger

	// Identity is the identity-core repository on the ACTIVE store
	// (users, groups, bindings). The `identity` feature's prereq and
	// Verify read it; nil means the tables are not wired on this backend.
	Identity persistence.IdentityRepository

	// AdminAudit is the admin audit sink the `architect-consult` feature
	// requires before it may enable (review R8: a nil repository means
	// zero consultations, never an unaudited one).
	AdminAudit persistence.AdminAuditRepository
	// Durability probes the store's commit contract for `config-assistant`.
	Durability DurabilityProber
	// A2APeers lists configured a2a.peers keys for `architect-consult`.
	A2APeers A2APeerLister
}
