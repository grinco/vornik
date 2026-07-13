package chat

import (
	"sync"
	"time"
)

// health_breaker.go — the per-(route, model) circuit breaker state machine
// (LLD 2026-07-11-model-health-circuit-breaker §5). One mutex guards the
// whole breaker (state + rolling window + open timestamp); breakers are
// per-key so different models never contend, and calls on one key are already
// serialized enough by the queue layer that the lock is negligible.

// HealthConfig tunes the breaker (§9). Zero values are NOT usable defaults —
// use DefaultHealthConfig and override.
type HealthConfig struct {
	Window       time.Duration // rolling failure window
	MinSamples   int           // don't trip below this many failures in-window
	FailureRate  float64       // trip at/above this failure fraction (inclusive)
	OpenCooldown time.Duration // OPEN → HALF_OPEN after this
}

// DefaultHealthConfig returns the §5 defaults.
func DefaultHealthConfig() HealthConfig {
	return HealthConfig{Window: time.Minute, MinSamples: 5, FailureRate: 0.5, OpenCooldown: 30 * time.Second}
}

// CircuitState is both the state and the Prometheus gauge value:
// closed=0, half-open=1, open=2 (§7). Exported so the executor-side
// agent-LLM breaker (internal/executor/agenthealth) can build a reject
// error carrying the state label.
type CircuitState int32

// Circuit breaker states. The numeric value is also the Prometheus gauge
// value (0 closed / 1 half-open / 2 open — LLD 2026-07-11 §7).
const (
	CircuitClosed   CircuitState = 0
	CircuitHalfOpen CircuitState = 1
	CircuitOpen     CircuitState = 2
)

// Label returns the human/Prometheus label for the state.
func (s CircuitState) Label() string {
	switch s {
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

type breakerSample struct {
	t  time.Time
	ok bool
}

// Breaker is the per-(route, model) circuit-breaker state machine (LLD
// 2026-07-11-model-health-circuit-breaker §5). It has no Provider coupling —
// it just allows a call and folds its outcome — so the executor-side
// agent-LLM breaker reuses it directly. One mutex guards the whole breaker
// (state + rolling window + open timestamp); breakers are per-key so
// different models never contend.
type Breaker struct {
	cfg HealthConfig
	now func() time.Time

	mu       sync.Mutex
	ring     []breakerSample
	state    CircuitState
	openedAt time.Time
}

// NewBreaker constructs a breaker. `now` may be nil (defaults to time.Now).
// Zero-value HealthConfig is NOT usable — use DefaultHealthConfig and override.
func NewBreaker(cfg HealthConfig, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{cfg: cfg, now: now}
}

// Allow decides whether a call may proceed. Returns permitted (may call
// upstream), probe (this caller is the single HALF_OPEN probe), and the
// current state (for the reject error / gauge). The single-permit property
// is structural: only the caller that flips OPEN→HALF_OPEN gets probe=true;
// every subsequent caller sees HALF_OPEN and is rejected until the probe's
// Record() resolves the state.
func (b *Breaker) Allow() (permitted, probe bool, state CircuitState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case CircuitClosed:
		return true, false, CircuitClosed
	case CircuitOpen:
		if b.now().Sub(b.openedAt) >= b.cfg.OpenCooldown {
			b.state = CircuitHalfOpen
			return true, true, CircuitHalfOpen // this caller probes
		}
		return false, false, CircuitOpen
	default: // CircuitHalfOpen — a probe is already in flight
		return false, false, CircuitHalfOpen
	}
}

// OpenSince returns the OPEN timestamp for the reject error (best-effort).
func (b *Breaker) OpenSince() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openedAt
}

// Snapshot returns the breaker's current state label and OPEN timestamp
// under the lock — the read path for the doctor's live-circuit line.
func (b *Breaker) Snapshot() (state string, openedAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.Label(), b.openedAt
}

// Record folds a call outcome into the breaker and returns the resulting
// state, whether this call just tripped the circuit OPEN (for the trips
// counter), and whether the state changed at all (for the edge-triggered
// alert hook). A probe call resolves HALF_OPEN → CLOSED (ok) or OPEN (fail); a
// normal closed-state call appends to the window and may trip.
func (b *Breaker) Record(ok, probe bool) (state CircuitState, tripped, changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.state
	if probe {
		if ok {
			b.state = CircuitClosed
			b.ring = b.ring[:0]
			return CircuitClosed, false, prev != CircuitClosed
		}
		b.state = CircuitOpen
		b.openedAt = b.now()
		return CircuitOpen, true, prev != CircuitOpen // re-open counts as a trip
	}
	// Normal (closed-state) outcome.
	b.ring = append(b.ring, breakerSample{t: b.now(), ok: ok})
	b.evictLocked()
	if b.state == CircuitClosed && !ok && b.breachedLocked() {
		b.state = CircuitOpen
		b.openedAt = b.now()
		return CircuitOpen, true, true
	}
	return b.state, false, false
}

// evictLocked drops window entries older than cfg.Window.
func (b *Breaker) evictLocked() {
	cutoff := b.now().Add(-b.cfg.Window)
	i := 0
	for i < len(b.ring) && b.ring[i].t.Before(cutoff) {
		i++
	}
	if i > 0 {
		b.ring = append(b.ring[:0], b.ring[i:]...)
	}
}

// breachedLocked reports whether the in-window failures meet BOTH the
// min-samples floor and the failure-rate threshold (§5.2).
func (b *Breaker) breachedLocked() bool {
	failures := 0
	for _, s := range b.ring {
		if !s.ok {
			failures++
		}
	}
	if failures < b.cfg.MinSamples {
		return false
	}
	total := len(b.ring)
	if total == 0 {
		return false
	}
	return float64(failures)/float64(total) >= b.cfg.FailureRate
}
