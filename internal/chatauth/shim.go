// Package chatauth is the compatibility shim of
// oidc-identity-permissions-design §5.3: the layer that lets a deployment
// move from hand-maintained chat allowlists to identity-based authorization
// WITHOUT a flag day.
//
// It exists because of an arithmetic fact about every deployment the moment
// Phase 4 ships: `user_identities` holds only the bindings the web login flow
// created, so no chat sender is linked yet. Cutting straight to
// identity-only authorization would not tighten a loose grant — it would
// revoke chat access from every current chat user, the operator included, at
// the next restart.
//
// So for one migration window both authorities are consulted and EITHER may
// grant. The shim's real product is not the grant, though; it is the
// WORKLIST: a metric and a deduped WARN naming exactly the identities that
// would lose access when the legacy config keys are removed, and nothing
// else.
package chatauth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
)

// Resolver is the identity side of the OR. Implemented by authz.Service.
type Resolver interface {
	Resolve(ctx context.Context, channel, externalID string) (*authz.Principal, error)
}

// Legacy is the allowlist side: what the hand-maintained config said, and
// which key said it. The key travels with the answer because the WARN and the
// metric are a migration worklist — an operator needs to know which line to
// delete, not merely that some line granted.
type Legacy struct {
	Allowed   bool
	ConfigKey string
	// Projects is the legacy project scope, used only when the legacy list is
	// the one that granted. A resolved principal carries its own scope.
	Projects []string
}

// Decision is the OR-matrix outcome.
type Decision struct {
	Allowed bool
	// Projects is the effective project scope: the principal's when the
	// resolver granted, the legacy list's otherwise. Nil means unrestricted.
	Projects []string
	// Principal is non-nil only when the resolver granted. Callers that need
	// a real identity — attribution, admin-only commands — must require this
	// rather than treating any Allowed decision as identified.
	Principal *authz.Principal
	// ViaLegacyOnly is true when the resolver did NOT grant and the legacy
	// list did. This is the population the removal gate measures.
	ViaLegacyOnly bool
	// ResolverUnavailable is true when the resolver FAILED rather than
	// answering "no" — an outage, not a verdict. resolve() already draws that
	// line (ErrUnknownIdentity and ErrUserDisabled are ordinary noes); this
	// carries it to callers.
	//
	// It exists because the two reasons for refusing have different remedies,
	// and a caller that cannot tell them apart gives the wrong one: "you have
	// not linked yet" is fixed by /link, while "the resolver is down" is fixed
	// by waiting, and sending a person to link while the database is
	// unreachable sends them to do something that cannot work. The
	// configuration assistant's chat entrypoint must refuse while resolution
	// is unavailable AND say so (config-assistant §6.3.3, test 23).
	//
	// It can be true on an ALLOWED decision: during an outage the shim still
	// grants ordinary chat from the legacy list, which is its whole purpose.
	// A caller requiring a real identity refuses that grant anyway, on
	// Principal == nil, and uses this to explain why.
	ResolverUnavailable bool
}

// Metrics is the shim's counter.
//
// It is a HOLDER that can outlive the registry, not a counter built against
// one. The daemon builds this shim during subsystem init, where the served
// Prometheus registry may not exist yet, and `NewMetrics(nil)` used to
// substitute prometheus.DefaultRegisterer — which is the documented
// 2026-06-06 invisible-metric trap: the counter registers on a registry
// /metrics does not serve, and increments where nobody can read them.
//
// That mattered more here than a missing counter usually does. This counter
// IS the §5.3 removal gate — "14 consecutive days at zero" is read off it —
// so an invisible one does not read as missing, it reads as ZERO, and a gate
// that cannot distinguish "nobody used the legacy allowlist" from "nothing
// was ever counted" reports the first and means the second
// (review-20260914-3c36 F15, promoted from its "low severity").
//
// Grants recorded before Attach are tallied and drained on attach, so the
// gate does not start its count from a silent gap.
type Metrics struct {
	// vec is read without the lock on the hot path; it is written exactly
	// once, under the lock, by Attach.
	vec atomic.Pointer[prometheus.CounterVec]

	mu      sync.Mutex
	pending map[[2]string]int64
}

// NewMetrics builds the holder. A non-nil registerer attaches immediately
// (the test path, and any caller that already has the served registry); nil
// leaves it detached until Attach, and counts into the pending tally
// meanwhile.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{pending: map[[2]string]int64{}}
	m.Attach(reg)
	return m
}

// Attach registers the counter on the SERVED registry and drains anything
// counted before it existed. Idempotent, because initHTTPServer runs twice.
func (m *Metrics) Attach(reg prometheus.Registerer) {
	if m == nil || reg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vec.Load() != nil {
		return
	}
	vec := promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "auth_legacy_allowlist_grants_total",
			Help: "Chat authorizations that succeeded ONLY via a legacy allowlist — " +
				"the resolver did not grant. This counts exactly the population that " +
				"would lose access if the named config key were removed today, which " +
				"is why a grant the resolver would also have made is NOT counted. " +
				"The §5.3 removal gate requires 14 consecutive days at zero.",
		},
		[]string{"channel", "config_key"},
	)
	for labels, n := range m.pending {
		if n > 0 {
			vec.WithLabelValues(labels[0], labels[1]).Add(float64(n))
		}
	}
	m.pending = map[[2]string]int64{}
	m.vec.Store(vec)
}

func (m *Metrics) recordLegacyGrant(channel, configKey string) {
	if m == nil {
		return
	}
	if vec := m.vec.Load(); vec != nil {
		vec.WithLabelValues(channel, configKey).Inc()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check under the lock: Attach may have landed between the load and
	// here, and a grant counted into a drained tally would be lost.
	if vec := m.vec.Load(); vec != nil {
		vec.WithLabelValues(channel, configKey).Inc()
		return
	}
	m.pending[[2]string{channel, configKey}]++
}

// Shim applies the OR-matrix.
type Shim struct {
	resolver Resolver
	metrics  *Metrics
	logger   zerolog.Logger

	mu sync.Mutex
	// warned dedupes the WARN per (channel, external_id) for the process
	// lifetime, so the log reads as a list of identities awaiting migration
	// rather than one line per message.
	warned map[string]bool
}

// New builds the shim. A nil resolver means Phase 4 is not wired, and the
// legacy list is the only authority — the pre-Phase-4 behaviour exactly,
// including no metric and no WARN, because there is no migration to measure.
func New(r Resolver, m *Metrics, logger zerolog.Logger) *Shim {
	return &Shim{resolver: r, metrics: m, logger: logger, warned: map[string]bool{}}
}

// NO Sever() METHOD, deliberately — and this comment is the record of why,
// because the design asked for one.
//
// Round-3 finding F15 argued that the §5.3 removal gate ("14 consecutive days
// of zero legacy grants") could be held open forever by a single identity
// that can never be linked — a colleague who has left — because "removal is
// per config key, wholesale, not per identity". I accepted that and built an
// in-memory Sever().
//
// The premise is wrong. `telegram.allowed_users` is a MAP KEYED BY USER ID and
// the Slack sender allowlists are per-installation sets: an operator removes
// ONE entry. That is already per-identity, and unlike a process-local sever it
// survives a restart — which matters enormously against a gate measured in
// days, since an in-memory sever silently resets and the metric starts
// counting the straggler again.
//
// So the durable operator action is to delete the straggler's line from the
// allowlist. A sever method would have been a second, weaker mechanism for a
// job the config already does properly, and shipping a security control that
// quietly forgets itself on restart is worse than not shipping one.

// Authorize applies the OR-matrix for one inbound message.
func (s *Shim) Authorize(ctx context.Context, channel, externalID string, legacy Legacy) Decision {
	principal, resolveErr := s.resolve(ctx, channel, externalID)
	unavailable := resolveErr != nil
	if principal != nil {
		// The resolver granted. Whether the legacy list would also have
		// granted is irrelevant and deliberately NOT counted: the metric
		// measures what would break on removal, and this identity would not.
		return Decision{Allowed: true, Projects: principal.Projects, Principal: principal}
	}

	if !legacy.Allowed {
		return Decision{ResolverUnavailable: unavailable}
	}

	// Legacy-only grant: the migration worklist.
	if s.resolver != nil {
		s.metrics.recordLegacyGrant(channel, legacy.ConfigKey)
		s.warnOnce(channel, externalID, legacy.ConfigKey, resolveErr)
	}
	return Decision{Allowed: true, Projects: legacy.Projects, ViaLegacyOnly: true, ResolverUnavailable: unavailable}
}

// resolve returns the principal when the resolver granted, plus any error
// worth reporting. An unknown identity and a disabled user are ordinary "no"
// answers; anything else is an outage.
func (s *Shim) resolve(ctx context.Context, channel, externalID string) (*authz.Principal, error) {
	if s.resolver == nil {
		return nil, nil
	}
	p, err := s.resolver.Resolve(ctx, channel, externalID)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, authz.ErrUnknownIdentity), errors.Is(err, authz.ErrUserDisabled):
		return nil, nil
	default:
		return nil, err
	}
}

// warnOnce emits the migration line once per identity per process.
// warnedCap bounds the dedup set. It is a WORKLIST — one entry per identity
// still riding the legacy allowlist — and the §5.3 removal gate is satisfied
// when the population reaches zero, so in a healthy migration it shrinks to
// nothing. The cap is for the unhealthy case: a channel whose external ids
// churn (rotated Slack ids, ephemeral bot chats) never repeats a key, and the
// map would then grow for the life of the process (review-20260914-3c36 F3).
//
// Overflow drops the whole set rather than evicting one entry. Losing the
// dedup state costs a repeated WARN line — the metric, which is what the gate
// actually reads, is not deduped and is unaffected. An LRU would cost more
// code than the property is worth.
const warnedCap = 4096

func (s *Shim) warnOnce(channel, externalID, configKey string, resolveErr error) {
	s.mu.Lock()
	key := channel + ":" + externalID
	first := !s.warned[key]
	if first && len(s.warned) >= warnedCap {
		s.warned = map[string]bool{}
	}
	s.warned[key] = true
	s.mu.Unlock()
	if !first {
		return
	}
	ev := s.logger.Warn().
		Str("channel", channel).
		Str("external_id", externalID).
		Str("config_key", configKey)
	if resolveErr != nil {
		// A resolver that is DOWN is not a resolver that said no. Reporting
		// both the same way would turn a database blip into a silent
		// authorization downgrade nobody notices.
		ev.Err(resolveErr).Msg("chat auth: resolver unavailable; granted via legacy allowlist — this is an outage, not a migration gap")
		return
	}
	ev.Msg("chat auth: granted via legacy allowlist only; link this identity (/link) and assign a group, then remove the named config key")
}
