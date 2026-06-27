package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
)

// Route is one model→provider mapping inside a Router. Prefix matching
// is case-sensitive first-match; list more specific prefixes first.
type Route struct {
	// Prefix selects this route when the request's model string starts
	// with it. Common values: "claude-" (Anthropic IDs), "gpt-"
	// (OpenAI), "o3-" / "o4-" (OpenAI reasoning), "codex" (Codex-family
	// models). An empty prefix matches everything — only put that last,
	// as it overrides the explicit fallback.
	Prefix string
	// Suffix selects this route when the request's model string ends
	// with it. Used to route by an intent-bearing tag that's
	// independent of the vendor prefix — notably OpenRouter's ":free"
	// variants, which span every vendor (deepseek/...:free,
	// google/...:free, qwen/...:free). Suffix matches take PRECEDENCE
	// over prefix matches (see providerFor) so a ":free" model lands on
	// OpenRouter regardless of where the route sits in the list or
	// whether a prefix route (e.g. "google/") would also match. Empty
	// (the default) means "no suffix match" — every existing
	// prefix-only route is unaffected. A route may set Prefix, Suffix,
	// or both (both = prefix OR suffix).
	Suffix string
	// Provider handles requests whose model matches Prefix or Suffix.
	Provider Provider
	// Name is an operator-facing label used in logs ("claude-cli",
	// "codex-subscription", "http"). Optional but recommended.
	Name string
}

// Router is a Provider that dispatches to one of several underlying
// Providers based on the requested model. Built to support a
// Claude-CLI + Codex-subscription (+ optional HTTP) deployment where agent
// containers pick the backend implicitly via whatever model they
// happen to send.
//
// Router implements ModelOverridable so the proxy's existing
// per-request override path keeps working — `router.WithModel(m)`
// returns the sub-provider that matches `m`, pinned to `m`.
type Router struct {
	routes   []Route
	fallback Provider
	// fallbackName is the kind label of the fallback sub-provider
	// ("bedrock" / "http" / "vertex" / "claude-cli" / etc.). Used by
	// fallbackPassesModelThrough to decide whether an unrouted
	// model name should be forwarded as a per-request override.
	// Optional — empty falls back to specialised-CLI behaviour.
	fallbackName string
	logger       zerolog.Logger
	// subs is a name → sub-provider map populated via WithRouterSubs.
	// Used by ListModels to enumerate every enabled sub-provider, even
	// ones that don't appear in routes (e.g. when a sub-provider is
	// enabled in config purely as a fallback). Empty when the caller
	// didn't pass WithRouterSubs — ListModels then derives the set
	// from routes + fallback as a best-effort.
	subs map[string]Provider
}

// RouterOption configures a Router.
type RouterOption func(*Router)

// WithRouterLogger sets the logger for per-route dispatch telemetry.
func WithRouterLogger(l zerolog.Logger) RouterOption {
	return func(r *Router) { r.logger = l }
}

// WithRouterSubs registers the full name → sub-provider map so
// ListModels can enumerate every enabled sub-provider. Without this,
// ListModels has to infer the set from routes + fallback, which loses
// any sub-provider that's enabled but not referenced by a route.
func WithRouterSubs(subs map[string]Provider) RouterOption {
	return func(r *Router) {
		// Defensive copy — the caller may continue mutating its own map.
		r.subs = make(map[string]Provider, len(subs))
		for k, v := range subs {
			r.subs[k] = v
		}
	}
}

// WithRouterFallbackName labels the fallback sub-provider's kind so
// the unrouted-model dispatch path can decide whether to forward
// the request's model name. General-purpose proxies (bedrock / http
// / vertex) honour the request; specialised CLIs ignore it. See
// Router.fallbackPassesModelThrough for the kind list.
func WithRouterFallbackName(name string) RouterOption {
	return func(r *Router) { r.fallbackName = name }
}

// NewRouter constructs a Router. fallback must be non-nil: it handles
// requests whose model doesn't match any explicit route, and also
// serves as the "default" provider when callers don't override model
// at all (the dispatcher's Telegram/autonomy paths).
//
// The returned Router is read-only after construction — safe to share
// across concurrent requests. Each WithModel call returns a fresh
// sub-provider; the Router itself is never mutated.
func NewRouter(fallback Provider, routes []Route, opts ...RouterOption) (*Router, error) {
	if fallback == nil {
		return nil, fmt.Errorf("chat.Router: fallback Provider is required")
	}
	// Validate every Route has a non-nil Provider so we fail at
	// startup rather than deferring to the first mismatched request.
	for i, r := range routes {
		if r.Provider == nil {
			return nil, fmt.Errorf("chat.Router: routes[%d] (prefix=%q) has nil Provider", i, r.Prefix)
		}
	}
	out := &Router{routes: routes, fallback: fallback, logger: zerolog.Nop()}
	for _, opt := range opts {
		opt(out)
	}
	return out, nil
}

// providerFor picks the route matching `model` and reports whether an
// explicit route matched. When no route applies the fallback is
// returned with matched=false — callers use that signal to decide
// whether to pass `model` through to the sub-provider (known prefix,
// honor the choice) or ignore it (unknown model, let the sub-provider
// use its own default rather than forwarding garbage to e.g. the
// Claude CLI which will exit 1 on a made-up model name).
func (r *Router) providerFor(model string) (p Provider, name string, matched bool) {
	if model == "" {
		return r.fallback, "fallback", false
	}
	// Pass 1 — suffix routes take precedence. A ":free" model is
	// definitively OpenRouter regardless of its vendor prefix, and this
	// pass guarantees that even when a prefix route (e.g. "google/")
	// appears earlier in the list or a default route was appended after
	// the operator's routes by mergeWithDefaultRoutes. Backward-
	// compatible: no route sets Suffix today, so this pass is a no-op for
	// every existing configuration.
	for _, route := range r.routes {
		if route.Suffix != "" && strings.HasSuffix(model, route.Suffix) {
			return route.Provider, route.Name, true
		}
	}
	// Pass 2 — prefix routes (the historical path). An empty prefix is a
	// catch-all ONLY when the route carries no suffix, so a suffix-only
	// route (Prefix=="", Suffix!=":...") never swallows non-matching
	// models here.
	for _, route := range r.routes {
		if route.Prefix == "" {
			if route.Suffix == "" {
				return route.Provider, route.Name, true
			}
			continue
		}
		if strings.HasPrefix(model, route.Prefix) {
			return route.Provider, route.Name, true
		}
	}
	return r.fallback, "fallback", false
}

// Complete implements Provider. Routes through the fallback (no model
// info available here; callers that care use WithModel first).
func (r *Router) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return r.fallback.Complete(ctx, messages)
}

// CompleteWithTools implements Provider. Same default-route behaviour.
func (r *Router) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return r.fallback.CompleteWithTools(ctx, messages, tools)
}

// CompleteWithToolsStream implements Provider.
func (r *Router) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return r.fallback.CompleteWithToolsStream(ctx, messages, tools, onText)
}

// Model implements Provider. Returns the fallback's model so metrics
// labels stay stable for non-request-scoped callers (dispatcher's
// default flow).
func (r *Router) Model() string { return r.fallback.Model() }

// SetMetrics fan-outs the metrics sink to every sub-provider so their
// per-model Prometheus labels all land on the same counters.
func (r *Router) SetMetrics(m *Metrics) {
	r.forEachSubProvider(func(_ string, p Provider) {
		p.SetMetrics(m)
	})
}

// forEachSubProvider walks the fallback plus every distinct provider
// referenced by a route, calling fn once per provider. fn sees the
// route Name where available ("" for the fallback). Visit order: the
// fallback first, then routes in declaration order. Each provider is
// visited at most once even if it appears as both the fallback and a
// route's Provider, or under multiple route prefixes.
func (r *Router) forEachSubProvider(fn func(name string, p Provider)) {
	seen := make(map[Provider]struct{}, len(r.routes)+1)
	if r.fallback != nil {
		seen[r.fallback] = struct{}{}
		fn("", r.fallback)
	}
	for _, route := range r.routes {
		if route.Provider == nil {
			continue
		}
		if _, dup := seen[route.Provider]; dup {
			continue
		}
		seen[route.Provider] = struct{}{}
		fn(route.Name, route.Provider)
	}
}

// resolveSubs returns the name→Provider map used by ListModels and
// Ping. Prefers the operator-supplied map (WithRouterSubs); otherwise
// derives a best-effort set from the route table plus the fallback
// stamped as "fallback" when it's not already referenced by a named
// route. Returned map is owned by the caller — they may not mutate
// the Router's stored copy.
func (r *Router) resolveSubs() map[string]Provider {
	if len(r.subs) > 0 {
		return r.subs
	}
	out := make(map[string]Provider, len(r.routes)+1)
	for _, route := range r.routes {
		if route.Name != "" && route.Provider != nil {
			out[route.Name] = route.Provider
		}
	}
	if r.fallback != nil {
		dup := false
		for _, p := range out {
			if p == r.fallback {
				dup = true
				break
			}
		}
		if !dup {
			out["fallback"] = r.fallback
		}
	}
	return out
}

// WithModel implements ModelOverridable. Looks up the matching
// sub-provider and, if that provider is itself ModelOverridable,
// pins it to `model`. This is what makes per-role `model:` settings
// in swarm YAMLs route to the right backend — a request for
// "claude-sonnet-4-6" finds the claude-cli sub-provider and pins
// that sub-provider's model; a request for "gpt-5.3-codex" finds
// codex-subscription instead.
//
// Fallback handling for un-routed model names varies by sub-provider
// type. Some fallback kinds (claude-cli, codex-cli) refuse to accept
// arbitrary model names — passing through an unrecognised ID makes
// them exit 1, which surfaces to the agent as an opaque failure.
// Other kinds (bedrock, http) are general-purpose proxies that
// happily serve any model the upstream catalogue publishes; passing
// the requested name through is the correct behaviour there. We
// distinguish via fallbackPassesModelThrough: kinds whose fallback
// path SHOULD honour the request's model. Reproduced 2026-05-07
// when default:bedrock + un-routed moonshotai.kimi-k2.5 silently
// served as zai.glm-4.7-flash because the old code returned the
// fallback un-pinned; the strategist saw responses from the wrong
// model and looped on current_time when the wire shape didn't
// match its expectations.
func (r *Router) WithModel(model string) Provider {
	sub, name, matched := r.providerFor(model)
	r.logger.Debug().
		Str("model", model).
		Str("route", name).
		Bool("matched", matched).
		Msg("router: dispatching")
	if !matched {
		// Unrouted model — for general-purpose fallback kinds
		// (bedrock / http), forward the request's model so the
		// caller's choice is honoured. For specialised CLI kinds
		// (claude-cli, codex-cli), let the fallback's default
		// stand — those refuse arbitrary model IDs and would
		// surface as opaque exit-1 failures.
		if r.fallbackPassesModelThrough() {
			if o, ok := sub.(ModelOverridable); ok {
				return o.WithModel(model)
			}
		}
		return sub
	}
	if o, ok := sub.(ModelOverridable); ok {
		return o.WithModel(model)
	}
	return sub
}

// fallbackPassesModelThrough reports whether the fallback sub-provider
// is a general-purpose model proxy (bedrock / http / vertex) where
// forwarding an unrouted model name is safe, vs. a specialised
// backend (claude-cli / codex-cli) that refuses arbitrary model
// IDs. The distinction is documented in WithModel above; this
// helper centralises the kind list so adding a new general-purpose
// sub-provider is a one-line change.
//
// fallbackName is empty when the router was constructed without a
// kind label (older test paths); treat that as "specialised, don't
// pass through" so we don't regress the historical CLI behaviour.
func (r *Router) fallbackPassesModelThrough() bool {
	switch r.fallbackName {
	case "bedrock", "http", "vertex", "openrouter":
		return true
	default:
		return false
	}
}

// ListModels implements ModelLister by aggregating every sub-provider
// that itself implements ModelLister. Each result has Provider stamped
// with the sub-provider's registration name (e.g. "vertex", "http",
// "claude-subscription") so the caller can see who served which model.
//
// Failures from a single sub-provider don't fail the aggregate — the
// router records them on the returned ListModelsResult.Errors slice,
// keyed by sub-provider name, and continues. That way an operator
// running `vornikctl models list` against a deployment where one
// gateway is unreachable still gets the lists from the providers that
// are healthy.
//
// When the operator didn't pass WithRouterSubs, the router falls back
// to deriving the set from the route table — every named route plus
// the fallback. Sub-providers that are enabled in config but
// unreferenced by any route (rare) are missed in that fallback path.
func (r *Router) ListModels(ctx context.Context) ListModelsResult {
	subs := r.resolveSubs()
	out := ListModelsResult{
		Providers: make(map[string][]ModelInfo, len(subs)),
		Errors:    map[string]string{},
	}
	for name, sub := range subs {
		lister, ok := sub.(ModelLister)
		if !ok {
			continue
		}
		models, err := lister.ListModels(ctx)
		if err != nil {
			out.Errors[name] = err.Error()
			continue
		}
		stamped := make([]ModelInfo, len(models))
		for i, m := range models {
			m.Provider = name
			stamped[i] = m
		}
		out.Providers[name] = stamped
	}
	return out
}

// ListModelsResult is the aggregated return shape for Router.ListModels.
// Providers maps a sub-provider's registered name to the list of
// models it returned. Errors maps the same name space to the failure
// string when a sub-provider's ListModels returned an error — present
// here rather than in Providers so callers can distinguish "no models
// returned" from "the provider is broken".
type ListModelsResult struct {
	Providers map[string][]ModelInfo `json:"providers"`
	Errors    map[string]string      `json:"errors,omitempty"`
}

// Ping implements Pinger. The router pings the fallback first
// because every unrouted request lands there — if it's unhealthy
// the daemon is effectively offline regardless of how many sub-
// providers are up. It then pings each named sub-provider that
// implements Pinger; a single sub-provider failure is logged but
// doesn't fail the gate (otherwise a momentarily-flaky Vertex
// would block daemon startup even when the configured fallback is
// claude-cli on the local box). Sub-providers that don't implement
// Pinger are silently skipped — treated as ready by construction.
func (r *Router) Ping(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("router not configured")
	}
	if pg, ok := r.fallback.(Pinger); ok {
		if err := pg.Ping(ctx); err != nil {
			return fmt.Errorf("router fallback: %w", err)
		}
	}
	for name, sub := range r.resolveSubs() {
		if sub == r.fallback {
			continue
		}
		pg, ok := sub.(Pinger)
		if !ok {
			continue
		}
		if err := pg.Ping(ctx); err != nil {
			r.logger.Warn().Err(err).Str("sub_provider", name).
				Msg("router: sub-provider ping failed at startup — continuing on fallback")
		}
	}
	return nil
}

// Compile-time conformance checks.
var _ Provider = (*Router)(nil)
var _ ModelOverridable = (*Router)(nil)
var _ Pinger = (*Router)(nil)
