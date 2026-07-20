// Package agenthealth is the executor-side per-model circuit breaker for
// agent-container LLM calls (LLD 2026-07-12-agent-llm-health-breaker).
//
// The chat-router breaker (internal/chat HealthGatedProvider) gates LLM calls
// that flow through the daemon chat proxy. Agent containers in topology 1
// (the shipped default — `agent_llm.endpoint` empty → inherits the upstream
// gateway URL) curl the upstream DIRECTLY, bypassing that breaker, so a sick
// model burns the full infra-retry ladder per step (the 2026-07-12
// ~12-container-starts incident). This breaker sits in the executor, fed by
// agent-container call outcomes (the post-hoc result.json error string), and
// fast-fails-over to the role's modelFallback via the existing
// chat.IsModelUnhealthyFailure (skip-the-ladder) + isModelShapedFailure
// (trigger fallback) paths — no new fallback logic.
//
// It reuses chat.Breaker (the state machine; the 2nd breaker — a 3rd consumer
// triggers extraction to a shared internal/circuit package per the
// centralize-on-recurrence rule). Keyed by MODEL only (the agent path uses a
// single endpoint; design §3). Default-on; nil-safe metrics; enabled:false is
// a passthrough.
package agenthealth

import (
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
)

// Config tunes the agent-LLM breaker. Reuses chat.HealthConfig; zero values
// are NOT usable — use chat.DefaultHealthConfig and override. The agent
// default (LLD §5/§6) is MinSamples=3, FailureRate=0.5, OpenCooldown=30s,
// Window=1m (containers are expensive — see §5 rationale).
type Config struct {
	Health  chat.HealthConfig
	Enabled bool
	Now     func() time.Time // injectable clock; nil → time.Now
}

// MetricsSink is the narrow observability seam (nil-safe: a nil sink is a
// no-op, the breaker still gates — LLD §12 B1). The executor wires a sink that
// wraps its vornik_agent_model_health_state gauge + _trips_total counter.
type MetricsSink interface {
	SetStateGauge(model string, state chat.CircuitState)
	IncTrips(model string)
}

// Registry holds the per-model agent-LLM circuit breakers. One shared
// *sync.Map across all agent steps + WithModel clones (LLD §4).
type Registry struct {
	cfg     Config
	reg     *sync.Map // model → *chat.Breaker
	metrics MetricsSink
}

// NewRegistry constructs a registry. When Enabled is false the registry's
// Gate/Record are passthroughs (byte-identical to today) and the underlying
// sync.Map is unused.
func NewRegistry(cfg Config) *Registry {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Registry{cfg: cfg, reg: &sync.Map{}}
}

// SetMetrics stores the observability sink (nil-safe). Call once at the
// executor's SetMetrics timing.
func (r *Registry) SetMetrics(m MetricsSink) { r.metrics = m }

// ModelHealthSnapshot returns the live state of every agent-LLM breaker,
// implementing chat.ModelHealthReporter so the doctor surfaces the agent
// breaker through the SAME interface as the chat-router breaker. Route is the
// constant "agent" (this breaker is not per-route). Iteration is point-in-time
// per breaker (each Snapshot is atomic; state may shift across the Range) —
// acceptable for a diagnostic read. Nil-safe.
func (r *Registry) ModelHealthSnapshot() []chat.ModelHealthSnapshot {
	if r == nil || r.reg == nil {
		return nil
	}
	var out []chat.ModelHealthSnapshot
	r.reg.Range(func(k, v any) bool {
		model, _ := k.(string)
		b, ok := v.(*chat.Breaker)
		if !ok {
			return true
		}
		state, openedAt := b.Snapshot()
		out = append(out, chat.ModelHealthSnapshot{
			Route:     "agent",
			Model:     model,
			State:     state,
			OpenSince: openedAt,
		})
		return true
	})
	return out
}

// Registry implements chat.ModelHealthReporter (consumed by the doctor).
var _ chat.ModelHealthReporter = (*Registry)(nil)

func (r *Registry) breakerFor(model string) *chat.Breaker {
	if v, ok := r.reg.Load(model); ok {
		return v.(*chat.Breaker)
	}
	actual, _ := r.reg.LoadOrStore(model, chat.NewBreaker(r.cfg.Health, r.cfg.Now))
	return actual.(*chat.Breaker)
}

// Gate is the pre-call check (LLD §4). It consumes the single HALF_OPEN probe
// permit when the circuit is due to probe (so half-open actually probes — a
// non-consuming peek would strand the circuit half-open forever). Returns:
//   - permitted: the caller may start the container;
//   - probe: this caller is the half-open probe (pass to Record so the probe
//     outcome resolves the state);
//   - reject: a *chat.ModelUnhealthyError when not permitted (carries
//     "MODEL_UNHEALTHY", so isModelUnhealthyFailure fast-exits the infra
//     ladder and isModelShapedFailure triggers the role's modelFallback).
//
// Passthrough (no-op) when the registry is nil, disabled, or the model is
// empty: (true, false, nil).
func (r *Registry) Gate(model string) (permitted, probe bool, reject error) {
	if r == nil || !r.cfg.Enabled || model == "" {
		return true, false, nil
	}
	b := r.breakerFor(model)
	permitted, probe, state := b.Allow()
	r.setGauge(model, state)
	if !permitted {
		return false, false, &chat.ModelUnhealthyError{
			Route:     "agent",
			Model:     model,
			State:     state.Label(),
			OpenSince: b.OpenSince(),
		}
	}
	return true, probe, nil
}

// Record folds a call outcome into the model's breaker (LLD §4 classification).
//
// Abstains (no vote) when err is a chat-breaker MODEL_UNHEALTHY reject
// (topology-2 isolation — the chat breaker owns that signal; double-counting
// would trip the agent breaker on a reject it never made a real call for).
// context.Canceled is abstain (caller gave up) and context.DeadlineExceeded is
// a failure — both fall out of chat.IsUpstreamInfraError, which excludes
// Canceled and includes DeadlineExceeded (LLD §12 I10).
//
// A nil err or a shape/content error (model reachable, bad output) counts as
// success; an upstream infra failure (PROVIDER_ERROR, 5xx, timeout) counts as
// a failure. No-op when the registry is nil, disabled, or the model is empty.
func (r *Registry) Record(model string, probe bool, err error) {
	if r == nil || !r.cfg.Enabled || model == "" {
		return
	}
	if chat.IsModelUnhealthyFailure(err) {
		return // abstain
	}
	ok := !chat.IsUpstreamInfraError(err)
	b := r.breakerFor(model)
	newState, tripped, _ := b.Record(ok, probe)
	r.setGauge(model, newState)
	if tripped {
		r.incTrips(model)
	}
}

func (r *Registry) setGauge(model string, s chat.CircuitState) {
	if r != nil && r.metrics != nil {
		r.metrics.SetStateGauge(model, s)
	}
}

func (r *Registry) incTrips(model string) {
	if r != nil && r.metrics != nil {
		r.metrics.IncTrips(model)
	}
}
