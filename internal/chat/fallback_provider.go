package chat

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
)

// withoutModelFallbackKey marks a request ctx so FallbackProvider skips its
// fallback for that call.
type withoutModelFallbackKey struct{}

// WithoutModelFallback marks ctx so the router-level FallbackProvider does NOT
// apply its per-model fallback for this call. The HTTP chat proxy sets it on
// agent-originated calls, which the executor's role modelFallback +
// both-down→PARK recovery already own; in-daemon direct callers (dispatcher,
// autonomy, wizard, and the pinned workers), which have no fallback of their
// own, leave it unset and get the router fallback. See
// https://docs.vornik.io §4.
func WithoutModelFallback(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutModelFallbackKey{}, true)
}

func modelFallbackSkipped(ctx context.Context) bool {
	v, _ := ctx.Value(withoutModelFallbackKey{}).(bool)
	return v
}

// FallbackProvider is a Provider decorator that, on a MODEL_UNHEALTHY reject
// from its inner provider (the chat router with its per-route circuit breakers),
// retries the call ONCE on a configured per-model fallback ("twin"). It wraps
// the router so the retry re-runs prefix routing via inner.WithModel(fallback)
// and lands on the right sub-provider, re-gated by that model's breaker.
//
// Scope: skipped when the request carries WithoutModelFallback(ctx) (agent
// calls, owned by the executor's role fallback). Inert when the map is empty.
// Single-hop: the retry is a plain inner call, never the decorator, so there is
// no fallback chaining. Auto-flip-back is inherent — no sticky state; each call
// re-evaluates the primary circuit, so traffic returns to the primary when its
// circuit recloses.
type FallbackProvider struct {
	inner Provider // serves the primary call; may be WithModel-descended into a route
	// root is the ORIGINAL router (never WithModel-descended). The fallback
	// retry re-routes THROUGH root so the twin resolves by its OWN prefix — a
	// caller that did WithModel(primary) first descended `inner` into the
	// primary's route, and retrying the twin on THAT provider would pin it onto
	// the wrong sub-provider (e.g. gpt-oss:20b→openai.gpt-oss-20b-1:0 landing on
	// ollama_cloud instead of bedrock). root re-runs prefix routing.
	root      Provider
	fallbacks map[string]string // primary model → fallback model
	metrics   *Metrics          // nil-safe; RecordModelFallback guards nil
	logger    zerolog.Logger
}

// NewFallbackProvider wraps inner with a per-model fallback map. A nil/empty map
// makes the decorator inert (inner's result passes through unchanged).
func NewFallbackProvider(inner Provider, fallbacks map[string]string, logger zerolog.Logger) *FallbackProvider {
	return &FallbackProvider{inner: inner, root: inner, fallbacks: fallbacks, logger: logger}
}

func (f *FallbackProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return f.withFallback(ctx, func(p Provider) (*ChatResponse, error) {
		return p.Complete(ctx, messages)
	})
}

func (f *FallbackProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return f.withFallback(ctx, func(p Provider) (*ChatResponse, error) {
		return p.CompleteWithTools(ctx, messages, tools)
	})
}

func (f *FallbackProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return f.withFallback(ctx, func(p Provider) (*ChatResponse, error) {
		return p.CompleteWithToolsStream(ctx, messages, tools, onText)
	})
}

// withFallback runs call on the inner provider and, on a fallback-worthy
// failure with a configured twin, retries call ONCE on root.WithModel(twin).
//
// Two triggers fail over (mirroring the executor's swarm-role fallback, which
// already treats an exhausted gateway error as a model-shaped failure):
//   - MODEL_UNHEALTHY — the per-route circuit is OPEN; mu.Model is authoritative.
//   - a raw upstream infra error (exhausted 429/5xx/timeout) that reached us
//     BEFORE the circuit tripped. This is the case a low-volume model (never
//     reaches MinSamples in-window, e.g. the autonomy Complete() path on
//     glm-5.2) or a HALF_OPEN probe hits during a sustained outage — without
//     it those calls hard-fail with no fallback (2026-07-18 weekly-limit
//     incident). The failed model is the descended primary's pinned model
//     (inner.Model(); the router resolves to its default sub-provider's model).
func (f *FallbackProvider) withFallback(ctx context.Context, call func(Provider) (*ChatResponse, error)) (*ChatResponse, error) {
	resp, err := call(f.inner)
	if err == nil || len(f.fallbacks) == 0 || modelFallbackSkipped(ctx) {
		return resp, err
	}
	var failedModel, route, trigger string
	var mu *ModelUnhealthyError
	switch {
	case errors.As(err, &mu):
		failedModel, route, trigger = mu.Model, mu.Route, "circuit_open"
	case IsUpstreamInfraError(err):
		failedModel, trigger = f.inner.Model(), "upstream_error"
	default:
		return resp, err // shape error / cancel — nothing a fallback fixes
	}
	fb, ok := f.fallbacks[failedModel]
	if !ok || fb == "" || fb == failedModel {
		return resp, err // no twin, or twin == primary (same circuit, would re-reject)
	}
	ov, ok := f.root.(ModelOverridable)
	if !ok {
		return resp, err // root can't re-pin the model — can't fall back
	}
	f.logger.Warn().
		Str("primary_model", failedModel).
		Str("fallback_model", fb).
		Str("route", route).
		Str("trigger", trigger).
		Msg("chat: primary model unavailable, retrying on configured fallback")
	f.metrics.RecordModelFallback(failedModel, fb)
	// Single hop: retry on the ROOT router pinned to the twin (re-runs prefix
	// routing so the twin lands on its own sub-provider) — NOT the decorator,
	// so the fallback attempt cannot itself trigger another fallback.
	return call(ov.WithModel(fb))
}

// Unwrap exposes the wrapped provider. Returns root rather than inner: inner
// may have been WithModel-descended into one route, whereas root is the
// original chain (the *Router in a router deployment) that discovery and
// readiness probes need to reach. See Unwrapper.
func (f *FallbackProvider) Unwrap() Provider {
	if f.root != nil {
		return f.root
	}
	return f.inner
}

func (f *FallbackProvider) Model() string { return f.inner.Model() }

func (f *FallbackProvider) SetMetrics(m *Metrics) {
	f.metrics = m
	f.inner.SetMetrics(m)
}

// WithModel returns a FallbackProvider wrapping inner.WithModel(model) so
// per-call model pinning composes; the fallback lookup still keys off the
// MODEL_UNHEALTHY error's authoritative .Model.
func (f *FallbackProvider) WithModel(model string) Provider {
	inner := f.inner
	if ov, ok := f.inner.(ModelOverridable); ok {
		inner = ov.WithModel(model)
	}
	// root is PRESERVED (not descended) so the fallback retry always re-routes
	// the twin by its own prefix through the original router.
	return &FallbackProvider{inner: inner, root: f.root, fallbacks: f.fallbacks, metrics: f.metrics, logger: f.logger}
}

// Ping delegates to the inner provider's readiness probe when it has one.
func (f *FallbackProvider) Ping(ctx context.Context) error {
	if p, ok := f.inner.(Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

var (
	_ Provider         = (*FallbackProvider)(nil)
	_ ModelOverridable = (*FallbackProvider)(nil)
)
