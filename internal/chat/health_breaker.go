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

// circuitState is both the state and the Prometheus gauge value:
// closed=0, half-open=1, open=2 (§7).
type circuitState int32

const (
	stateClosed   circuitState = 0
	stateHalfOpen circuitState = 1
	stateOpen     circuitState = 2
)

func (s circuitState) label() string {
	switch s {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

type breakerSample struct {
	t  time.Time
	ok bool
}

type modelBreaker struct {
	cfg HealthConfig
	now func() time.Time

	mu       sync.Mutex
	ring     []breakerSample
	state    circuitState
	openedAt time.Time
}

func newModelBreaker(cfg HealthConfig, now func() time.Time) *modelBreaker {
	if now == nil {
		now = time.Now
	}
	return &modelBreaker{cfg: cfg, now: now}
}

// allow decides whether a call may proceed. Returns permitted (may call
// upstream), probe (this caller is the single HALF_OPEN probe), and the
// current state (for the reject error / gauge). The single-permit property
// is structural: only the caller that flips OPEN→HALF_OPEN gets probe=true;
// every subsequent caller sees HALF_OPEN and is rejected until the probe's
// record() resolves the state.
func (b *modelBreaker) allow() (permitted, probe bool, state circuitState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return true, false, stateClosed
	case stateOpen:
		if b.now().Sub(b.openedAt) >= b.cfg.OpenCooldown {
			b.state = stateHalfOpen
			return true, true, stateHalfOpen // this caller probes
		}
		return false, false, stateOpen
	default: // stateHalfOpen — a probe is already in flight
		return false, false, stateHalfOpen
	}
}

// openSince returns the OPEN timestamp for the reject error (best-effort).
func (b *modelBreaker) openSince() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openedAt
}

// snapshot returns the breaker's current state label and OPEN timestamp
// under the lock — the read path for the doctor's live-circuit line.
func (b *modelBreaker) snapshot() (state string, openedAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.label(), b.openedAt
}

// record folds a call outcome into the breaker and returns the resulting
// state, whether this call just tripped the circuit OPEN (for the trips
// counter), and whether the state changed at all (for the edge-triggered
// alert hook). A probe call resolves HALF_OPEN → CLOSED (ok) or OPEN (fail); a
// normal closed-state call appends to the window and may trip.
func (b *modelBreaker) record(ok, probe bool) (state circuitState, tripped, changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.state
	if probe {
		if ok {
			b.state = stateClosed
			b.ring = b.ring[:0]
			return stateClosed, false, prev != stateClosed
		}
		b.state = stateOpen
		b.openedAt = b.now()
		return stateOpen, true, prev != stateOpen // re-open counts as a trip
	}
	// Normal (closed-state) outcome.
	b.ring = append(b.ring, breakerSample{t: b.now(), ok: ok})
	b.evictLocked()
	if b.state == stateClosed && !ok && b.breachedLocked() {
		b.state = stateOpen
		b.openedAt = b.now()
		return stateOpen, true, true
	}
	return b.state, false, false
}

// evictLocked drops window entries older than cfg.Window.
func (b *modelBreaker) evictLocked() {
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
func (b *modelBreaker) breachedLocked() bool {
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
