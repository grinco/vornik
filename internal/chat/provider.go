package chat

import (
	"context"
	"time"
)

// DefaultTimeout is the fallback per-request timeout shared by every
// chat provider (HTTP client, claude-cli, codex-cli, router). 5 minutes
// matches the agent container's curl --max-time default in
// images/vornik-agent/entrypoint.sh and the recommended chat.timeout in
// the sample configs; concentrating the value here keeps those layers
// from drifting apart silently. Any layer that overrides it (per-config
// chat.timeout, per-client WithXxxTimeout option) should still use this
// as its fallback so an unset field doesn't dip below what the outer
// layer expects.
const DefaultTimeout = 300 * time.Second

// Provider is the interface every LLM backend must satisfy. Introduced
// so the dispatcher and autonomy manager can switch between the HTTP
// OpenAI-compatible client and the subprocess-based Claude CLI client
// without the callers knowing which is which.
//
// The method set deliberately mirrors the original concrete *Client
// type — adding the interface is a drop-in: no caller needs to change
// method-call shape, only the field/parameter type they use.
//
// New providers MUST also populate ChatResponse.Usage when they can
// (tokens + cost) so the executor's cost tracking stays accurate; a
// zero Usage struct is tolerated but deflates the daemon's spend
// dashboards.
type Provider interface {
	// Complete runs a chat completion without any tools available. Used
	// by summarization and classification paths that don't need
	// tool-calling.
	Complete(ctx context.Context, messages []Message) (*ChatResponse, error)

	// CompleteWithTools runs a completion where the model may choose
	// to return tool calls. The resulting Message.ToolCalls slice is
	// non-empty when the model invoked a tool; an empty slice means
	// the model returned a final text response.
	CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error)

	// CompleteWithToolsStream is the streaming variant of
	// CompleteWithTools. onText receives the accumulated text content
	// as the model streams; onText is called in-band and should not
	// block. Callers should use the returned ChatResponse (post-stream)
	// for authoritative tool-call data — incremental tool-call chunks
	// are buffered internally and surfaced only on completion.
	CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error)

	// Model returns the currently-configured model identifier. Used by
	// metrics labels and for "which model ran this step" display in the
	// UI; must remain stable for the client's lifetime.
	Model() string

	// SetMetrics wires (or re-wires) the Prometheus metrics sink after
	// construction. Providers that don't emit Prometheus metrics may
	// implement this as a no-op.
	SetMetrics(m *Metrics)
}

// Compile-time check that the existing HTTP client satisfies the new
// interface. The var _ pattern prevents accidental drift — if someone
// later adds a method that changes a signature, this line fails to
// compile, flagging the provider contract as broken.
var _ Provider = (*Client)(nil)

// Pinger is an optional interface a Provider can implement to support
// readiness probing — used by the daemon's startup gate so the
// scheduler doesn't dispatch tasks into a half-initialised LLM
// backend (CLI binary still cold, HTTP gateway unreachable,
// subscription token loader still reading disk). A nil error means
// the provider is ready to serve at least one completion request.
//
// Implementations pick the cheapest probe that exercises the
// provider's critical-path dependencies — HTTP providers hit
// GET /v1/models (validates network + auth), subprocess providers
// exec the binary with --version (validates binary path), subscription
// providers parse the on-disk auth token. Probes MUST NOT consume
// LLM tokens — the gate runs on every daemon start and on every
// /readyz scrape.
//
// Providers that can't be meaningfully probed (or for whom every
// probe would be useless or costly) simply don't implement this
// interface; the gate treats them as ready by construction.
type Pinger interface {
	Ping(ctx context.Context) error
}

// ModelOverridable is an optional interface a Provider can implement
// to accept per-request model selection. The chat-completions proxy
// checks for it and, when present, routes each incoming request's
// `model` field through to the underlying CLI / HTTP call. Providers
// that can't honor it (or for whom model is a construction-time
// concept only) simply don't implement this interface — the proxy
// falls back to the provider's default.
//
// Implementations return a shallow-copy Provider pinned to `model`,
// leaving every other setting (timeouts, metrics, binary path, etc.)
// untouched on the original. Copy semantics — not mutation — are what
// let two concurrent requests use two different models without
// racing on a shared struct field.
type ModelOverridable interface {
	WithModel(model string) Provider
}

// ModelInfo describes a single model offered by a sub-provider. The
// shape is deliberately small: just enough to let an operator answer
// "what can I pin in a swarm role's `model:` field, and is it priced".
// The router stamps Provider when aggregating; sub-providers leave it
// blank.
type ModelInfo struct {
	// ID is the exact identifier the provider expects in the
	// `model` field of a chat request. Pin this verbatim in swarm
	// YAML — case and prefixes matter for router dispatch.
	ID string `json:"id"`
	// Provider is the sub-provider that serves this model: "http",
	// "vertex", "claude-subscription", "codex-subscription",
	// "claude-cli", "codex-cli". Stamped by the router; sub-providers
	// leave it empty.
	Provider string `json:"provider,omitempty"`
	// Source distinguishes a list returned by the provider's own API
	// ("live") from a hardcoded fallback ("static"). Subscription
	// providers (Claude/Codex OAuth) don't expose a /v1/models
	// endpoint; their lists are static and may lag actual catalogue
	// changes.
	Source string `json:"source"`
	// OwnedBy is the owner string the provider returned (OpenAI-shape
	// /v1/models populates this with "anthropic", "google", etc.).
	// Empty when the provider didn't surface one.
	OwnedBy string `json:"owned_by,omitempty"`
	// Created is the unix timestamp the provider stamped on the
	// model entry. Zero when not surfaced.
	Created int64 `json:"created,omitempty"`
}

// ModelLister is an optional interface for sub-providers that can
// enumerate the models they serve. The HTTP API surfaces this via
// GET /api/v1/models so operators can compare what each provider
// actually offers against the static pricing.yaml table — and catch
// model IDs that have been silently retired or renamed by the upstream
// catalogue.
//
// Implementations should:
//   - Return a "live" list when the provider exposes a /v1/models
//     endpoint and the call succeeded.
//   - Return a "static" list (hardcoded against current subscription
//     tiers) when the provider doesn't expose live discovery, or when
//     the live call failed and a static list is the next-best answer.
//   - Honor ctx for cancellation/timeout so a slow provider doesn't
//     stall the whole aggregation.
//
// Providers that can't enumerate at all (rare — the CLI subprocess
// providers technically can via `--list-models`, but parsing that
// output is fragile) skip implementing this interface; the router
// silently omits them from the aggregate.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}
