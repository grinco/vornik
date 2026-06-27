package ratelimit

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	metricsNamespace = "vornik"
	metricsSubsystem = "ratelimit"
)

// OutcomeLabel values for the DecisionsTotal counter — narrow enum so
// dashboards can build stable PromQL without code reading.
const (
	OutcomeAllow = "allow"
	OutcomeWarn  = "warn"
	OutcomeBlock = "block"
)

// ScopeLabel values for the DecisionsTotal counter.
const (
	ScopeAPIKey  = "api_key"
	ScopeProject = "project"
	// ScopeIP is the unauthenticated data-plane backstop scope
	// (hardening sub-item 2). AuthMiddleware's PerIPLimiter
	// path stamps decisions with this so dashboards can split
	// "unauthenticated flood" from "key over limit".
	ScopeIP = "ip"
)

// Metrics holds the operator-visible rate-limit observability surface
// added by hardening item (7). All counters are namespaced on
// (scope, outcome) — the scope_id is intentionally NOT a label
// because per-key cardinality would explode the series set; operators
// query the RemainingTokens gauge for per-key headroom and the
// audit log for who got the 429s.
type Metrics struct {
	// DecisionsTotal counts rate-limit decisions by (scope, outcome).
	// Three outcomes per scope: allow (under threshold), warn (≥80%
	// consumed), block (429). The warn tier fires on the same call
	// that crosses the threshold AND on every subsequent call until
	// the bucket refills — so warn count > block count in normal
	// degradation.
	DecisionsTotal *prometheus.CounterVec

	// RemainingTokens is the post-decision bucket level keyed by
	// (scope, scope_id). scope_id is per-key for ScopeAPIKey and
	// per-project for ScopeProject. Cardinality is bounded by the
	// number of active keys/projects — manageable at SaaS scale
	// because revoked keys are Forget()'d.
	RemainingTokens *prometheus.GaugeVec

	// events stores a bounded ring of recent (warn|block) decisions
	// keyed by (scope, scope_id). Drives the
	// /api/v1/projects/{id}/ratelimit-status endpoint and the
	// "approaching limit" UI banner — Prometheus counters are
	// monotonic so we'd need a 5-minute rate() PromQL query to read
	// recent warn count, which the UI surface doesn't have. The ring
	// keeps the last eventRingCap timestamps per (scope, id); older
	// entries are GC'd in StatusFor when the window slides past them.
	events     map[eventKey][]eventEntry
	eventsMu   sync.Mutex
	eventClock func() time.Time // injectable for tests; default time.Now
}

// eventRingCap bounds the per-(scope, id) event log so a hot key
// can't grow unbounded memory. 256 entries × ~30 active keys ≈
// 8 KiB; ample for the 5-minute status window even at sustained
// 1 rps degradation (300 events / 5 min).
const eventRingCap = 256

// eventKey is the composite (scope, id) used to bucket recent
// warn/block events.
type eventKey struct {
	scope string
	id    string
}

// eventEntry is one recorded warn/block timestamp + outcome label
// — block events also fire the warn flag so a single ring covers
// both "recent warnings" and "last 429" without double-storage.
type eventEntry struct {
	at      time.Time
	blocked bool
}

// NewMetrics registers the Prometheus collectors on the supplied
// registerer (defaults to prometheus.DefaultRegisterer when nil). The
// limiter doesn't OWN these — they're shared with the auth middleware
// which calls Observe* helpers.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		DecisionsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Subsystem: metricsSubsystem,
				Name:      "decisions_total",
				Help:      "Rate-limit decisions by scope (api_key|project) and outcome (allow|warn|block). Warn fires from the same call as the threshold crossing onward until the bucket refills, so warn count > block count under normal degradation.",
			},
			[]string{"scope", "outcome"},
		),
		RemainingTokens: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Subsystem: metricsSubsystem,
				Name:      "remaining_tokens",
				Help:      "Post-decision token-bucket level. scope_id is the key_id for api_key scope, project_id for project scope. Operators read this to spot keys approaching saturation.",
			},
			[]string{"scope", "scope_id"},
		),
		events:     make(map[eventKey][]eventEntry),
		eventClock: time.Now,
	}
}

// StatusWindow is the lookback the UI status endpoint uses for
// "recent warnings". Held at 5 minutes so the homepage banner
// reflects the immediate previous activity without flapping on a
// single spike.
const StatusWindow = 5 * time.Minute

// StatusSummary is the per-scope+id status surface read by the
// /api/v1/projects/{id}/ratelimit-status endpoint and the UI
// banner. All fields are zero-valued safe; an empty summary means
// "no recent warn/block traffic on this scope id" which renders
// the banner hidden.
type StatusSummary struct {
	// RecentWarns is the count of warn-or-block events within the
	// trailing StatusWindow. > 0 lights up the homepage banner.
	RecentWarns int
	// RecentBlocks is the count of block (429) events within the
	// trailing StatusWindow. Always ≤ RecentWarns since block
	// implies warn.
	RecentBlocks int
	// LastBlockAt is the most recent block (429) timestamp inside
	// the window, or zero when no block occurred. The UI renders
	// "Last 429 at HH:MM" when non-zero.
	LastBlockAt time.Time
}

// StatusFor returns the per-(scope, id) status summary for the
// trailing StatusWindow. Side-effect: GCs entries older than the
// window so the ring doesn't grow unbounded over long uptime when
// a hot key cools down. Safe on nil receiver — returns the zero
// summary, mirroring the no-emission Observe contract.
func (m *Metrics) StatusFor(scope, id string) StatusSummary {
	if m == nil || scope == "" || id == "" {
		return StatusSummary{}
	}
	now := m.eventClock()
	cutoff := now.Add(-StatusWindow)
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	key := eventKey{scope: scope, id: id}
	ring := m.events[key]
	// GC pass — drop entries older than the window. Ring is
	// append-ordered so the first element is the oldest; a single
	// forward walk finds the cut point.
	cut := 0
	for cut < len(ring) && ring[cut].at.Before(cutoff) {
		cut++
	}
	if cut > 0 {
		ring = ring[cut:]
		m.events[key] = ring
	}
	if len(ring) == 0 {
		// Free the empty slot so an idle scope doesn't squat memory
		// indefinitely after its last warn event slides out of window.
		delete(m.events, key)
		return StatusSummary{}
	}
	var s StatusSummary
	for _, e := range ring {
		s.RecentWarns++
		if e.blocked {
			s.RecentBlocks++
			if e.at.After(s.LastBlockAt) {
				s.LastBlockAt = e.at
			}
		}
	}
	return s
}

// recordEvent appends a warn-or-block entry to the per-(scope, id)
// ring. Trims to eventRingCap entries by dropping the oldest. Caller
// must hold no lock — eventsMu is taken internally.
func (m *Metrics) recordEvent(scope, id string, blocked bool) {
	if m == nil || scope == "" || id == "" {
		return
	}
	now := m.eventClock()
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	key := eventKey{scope: scope, id: id}
	ring := m.events[key]
	ring = append(ring, eventEntry{at: now, blocked: blocked})
	if len(ring) > eventRingCap {
		// Drop the oldest entries — copy-down keeps the ring
		// O(eventRingCap) bytes per scope.
		ring = ring[len(ring)-eventRingCap:]
	}
	m.events[key] = ring
}

// Observe records a per-call decision: emits one counter increment
// keyed on the highest-severity outcome and sets the remaining-tokens
// gauge. Safe on nil receiver — when metrics are disabled the limiter
// still functions, the dashboard panel just stays at zero.
func (m *Metrics) Observe(scope, scopeID string, d KeyDecision) {
	if m == nil || scope == "" {
		return
	}
	outcome := OutcomeAllow
	switch {
	case d.Blocked:
		outcome = OutcomeBlock
	case d.Warn:
		outcome = OutcomeWarn
	}
	m.DecisionsTotal.WithLabelValues(scope, outcome).Inc()
	if scopeID != "" {
		m.RemainingTokens.WithLabelValues(scope, scopeID).Set(d.RemainingTokens)
	}
	// Record warn/block to the event ring so StatusFor can answer
	// "recent warnings" + "last 429" without scraping Prometheus.
	// Allow outcomes don't enter the ring — the homepage banner only
	// fires on degradation, and the ring stays small under healthy
	// traffic.
	if outcome != OutcomeAllow {
		m.recordEvent(scope, scopeID, d.Blocked)
	}
}

// ObserveProject records a per-project task-creation decision. Same
// outcome taxonomy as Observe but the Decision shape is the project
// limiter's (sliding-window) rather than the key limiter's
// (token-bucket). No RemainingTokens gauge update — projects don't
// have a bucket level to surface.
func (m *Metrics) ObserveProject(projectID string, d Decision) {
	if m == nil || projectID == "" {
		return
	}
	outcome := OutcomeAllow
	if d.Blocked {
		outcome = OutcomeBlock
	}
	m.DecisionsTotal.WithLabelValues(ScopeProject, outcome).Inc()
	if outcome != OutcomeAllow {
		m.recordEvent(ScopeProject, projectID, d.Blocked)
	}
}
