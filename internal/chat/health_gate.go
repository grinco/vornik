package chat

import (
	"context"
	"sync"
	"time"
)

// health_gate.go — HealthGatedProvider: the decorator that puts a
// per-(route, model) circuit breaker in front of a sub-provider (LLD
// 2026-07-11-model-health-circuit-breaker §4). Wraps each route's provider in
// initChatRouter (Phase 3), mirroring BoundedRouteProvider: it shares its
// breaker registry across WithModel clones (the whole point — otherwise a
// per-request model pin would get its own breaker), and keys each breaker by
// (routeName, provider.Model() at call time).

// HealthGatedProvider gates calls through a per-(route, model) breaker.
type HealthGatedProvider struct {
	inner     Provider
	routeName string
	reg       *sync.Map // key(route,model) → *modelBreaker ; SHARED across clones
	cfg       HealthConfig
	now       func() time.Time
	metrics   *Metrics // set via SetMetrics; nil-safe
	// onChange is fired (edge-triggered) whenever a (route, model) circuit
	// changes state — the alert/observability seam (LLD §7). Nil-safe; SHARED
	// across WithModel clones so every clone reports to the same sink.
	onChange func(route, model, state string)
}

// WithStateChangeHook returns h configured to call fn on every circuit state
// transition (closed/half_open/open). Shared across WithModel clones. The
// container wires this to the operator-alert path.
func (h *HealthGatedProvider) WithStateChangeHook(fn func(route, model, state string)) *HealthGatedProvider {
	h.onChange = fn
	return h
}

// NewHealthGatedProvider wraps inner. inner == nil short-circuits (returns
// inner) so the wire site can apply the layer uniformly. Each route passes
// its own name but the SAME shared registry, so every (route, model) across
// the router has one breaker.
func NewHealthGatedProvider(inner Provider, routeName string, reg *sync.Map, cfg HealthConfig) Provider {
	return newHealthGatedProvider(inner, routeName, reg, cfg, time.Now)
}

// newHealthGatedProvider is the clock-injectable constructor (tests pass a
// fake now).
func newHealthGatedProvider(inner Provider, routeName string, reg *sync.Map, cfg HealthConfig, now func() time.Time) *HealthGatedProvider {
	if inner == nil {
		return nil
	}
	if reg == nil {
		reg = &sync.Map{}
	}
	if now == nil {
		now = time.Now
	}
	return &HealthGatedProvider{inner: inner, routeName: routeName, reg: reg, cfg: cfg, now: now}
}

// ModelHealthSnapshot is one (route, model) circuit's live state — the
// read model behind the doctor's live-circuit line. State is one of
// "closed" / "half_open" / "open"; OpenSince is the zero time unless the
// circuit is currently open.
type ModelHealthSnapshot struct {
	Route     string
	Model     string
	State     string
	OpenSince time.Time
}

// ModelHealthReporter is implemented by providers that can expose their
// live per-(route, model) circuit state. The doctor type-asserts the wired
// chat provider to this so it degrades cleanly (no live data) when the
// provider is a plain client or the breaker layer is disabled.
type ModelHealthReporter interface {
	ModelHealthSnapshot() []ModelHealthSnapshot
}

// ModelHealthSnapshot walks this provider's SHARED breaker registry and
// returns one entry per (route, model) breaker. Because every route +
// WithModel clone shares the one *sync.Map, a single walk over any one
// provider yields the whole router's circuits. Keys are "route\x00model".
func (h *HealthGatedProvider) ModelHealthSnapshot() []ModelHealthSnapshot {
	if h == nil || h.reg == nil {
		return nil
	}
	var out []ModelHealthSnapshot
	h.reg.Range(func(k, v any) bool {
		key, _ := k.(string)
		b, ok := v.(*modelBreaker)
		if !ok {
			return true
		}
		route, model := key, ""
		if i := indexByte0(key); i >= 0 {
			route, model = key[:i], key[i+1:]
		}
		state, openedAt := b.snapshot()
		out = append(out, ModelHealthSnapshot{Route: route, Model: model, State: state, OpenSince: openedAt})
		return true
	})
	return out
}

// indexByte0 finds the NUL key separator used by breakerFor.
func indexByte0(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\x00' {
			return i
		}
	}
	return -1
}

// breakerFor returns the breaker for (routeName, model), creating it atomically
// on first use (LoadOrStore — two concurrent first-callers never orphan a
// breaker).
func (h *HealthGatedProvider) breakerFor(model string) *modelBreaker {
	key := h.routeName + "\x00" + model
	if v, ok := h.reg.Load(key); ok {
		return v.(*modelBreaker)
	}
	actual, _ := h.reg.LoadOrStore(key, newModelBreaker(h.cfg, h.now))
	return actual.(*modelBreaker)
}

// gate runs call() behind the breaker for the inner provider's current model.
func (h *HealthGatedProvider) gate(_ context.Context, call func() (*ChatResponse, error)) (*ChatResponse, error) {
	model := h.inner.Model()
	b := h.breakerFor(model)
	permitted, probe, state := b.allow()
	if !permitted {
		h.setStateGauge(model, state)
		return nil, &ModelUnhealthyError{Route: h.routeName, Model: model, State: state.label(), OpenSince: b.openSince()}
	}
	resp, err := call()
	// A call is a health "success" unless it's an upstream infra failure —
	// shape/plausibility errors, nil, and caller-cancellation all count as
	// success (the model is reachable). IsUpstreamInfraError already excludes
	// context.Canceled and includes DeadlineExceeded (§5.3).
	ok := !IsUpstreamInfraError(err)
	newState, tripped, changed := b.record(ok, probe)
	h.setStateGauge(model, newState)
	if tripped {
		h.incTrips(model)
	}
	if changed && h.onChange != nil {
		h.onChange(h.routeName, model, newState.label())
	}
	return resp, err
}

func (h *HealthGatedProvider) setStateGauge(model string, s circuitState) {
	if h.metrics != nil && h.metrics.ModelHealthState != nil {
		h.metrics.ModelHealthState.WithLabelValues(h.routeName, model).Set(float64(s))
	}
}

func (h *HealthGatedProvider) incTrips(model string) {
	if h.metrics != nil && h.metrics.ModelHealthTrips != nil {
		h.metrics.ModelHealthTrips.WithLabelValues(h.routeName, model).Inc()
	}
}

// Complete implements Provider with health gating.
func (h *HealthGatedProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return h.gate(ctx, func() (*ChatResponse, error) { return h.inner.Complete(ctx, messages) })
}

// CompleteWithTools implements Provider with health gating.
func (h *HealthGatedProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return h.gate(ctx, func() (*ChatResponse, error) { return h.inner.CompleteWithTools(ctx, messages, tools) })
}

// CompleteWithToolsStream implements Provider with health gating.
func (h *HealthGatedProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return h.gate(ctx, func() (*ChatResponse, error) { return h.inner.CompleteWithToolsStream(ctx, messages, tools, onText) })
}

// Model delegates to the inner provider (gating is invisible to "which model").
func (h *HealthGatedProvider) Model() string { return h.inner.Model() }

// SetMetrics stores the metrics for the state gauge + trips counter and
// forwards to the inner provider so per-model chat metrics keep landing.
func (h *HealthGatedProvider) SetMetrics(m *Metrics) {
	h.metrics = m
	h.inner.SetMetrics(m)
}

// WithModel follows the BoundedRouteProvider pattern: a fresh
// HealthGatedProvider carrying the SAME shared registry + routeName, with a
// new inner from inner.WithModel — so per-request model pins share the
// route's breakers and record under their own resolved-model key.
func (h *HealthGatedProvider) WithModel(model string) Provider {
	mo, ok := h.inner.(ModelOverridable)
	if !ok {
		return h
	}
	return &HealthGatedProvider{
		inner:     mo.WithModel(model),
		routeName: h.routeName,
		reg:       h.reg,
		cfg:       h.cfg,
		now:       h.now,
		metrics:   h.metrics,
		onChange:  h.onChange,
	}
}

// Compile-time conformance.
var (
	_ Provider         = (*HealthGatedProvider)(nil)
	_ ModelOverridable = (*HealthGatedProvider)(nil)
)
