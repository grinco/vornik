package chatauth

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
)

// ExposureGuard removes link codes from chat text before it reaches a model,
// and burns the ones it finds — oidc-identity-permissions-design §5.2b.
//
// THE CASE IT CLOSES. A code typed with the redemption keyword redeems. A code
// pasted BARE, or introduced by any other word, was an ordinary prompt: it was
// dispatched to a model, landed in a conversation transcript, and stayed live
// until it expired, with nothing reporting it.
//
// Recognition is LOCAL — a marker and a check character, consulting no store —
// which is what makes this acceptable where §5.2's declined detector was not:
// there is no surface here that answers "does this code exist".
type ExposureGuard struct {
	burner  Burner
	metrics *ExposureMetrics
	logger  zerolog.Logger
}

// Burner consumes an exposed code. Implemented by *authz.Accounts.
type Burner interface {
	BurnExposedLinkCode(ctx context.Context, code string) (bool, error)
}

// NewExposureGuard builds the guard. A nil burner still scrubs: keeping the
// token away from the model is the half that does not need a store, and a
// deployment without identity wiring should not be the one deployment that
// forwards credentials to an LLM.
func NewExposureGuard(b Burner, m *ExposureMetrics, logger zerolog.Logger) *ExposureGuard {
	return &ExposureGuard{burner: b, metrics: m, logger: logger}
}

// Scrub returns text safe to dispatch. Any code-shaped token is replaced, and
// each one is burned so the remainder of its TTL is not spent on a secret that
// is no longer secret.
//
// The caller's reply must be the SAME generic notice whether or not anything
// was found — that identical answer is what keeps the store consultation from
// becoming the redemption oracle §5.2 refuses to build. Callers learn a code
// was removed from the returned text, which the speaker sees too.
func (g *ExposureGuard) Scrub(ctx context.Context, channel, text string) string {
	if g == nil {
		return text
	}
	scrubbed, found := authz.ScrubLinkCodes(text)
	if len(found) == 0 {
		return text
	}
	for _, code := range found {
		outcome := "scrubbed"
		if g.burner != nil {
			burned, err := g.burner.BurnExposedLinkCode(ctx, code)
			switch {
			case err != nil:
				outcome = "burn_failed"
			case burned:
				outcome = "revoked"
			}
		}
		g.metrics.observe(channel, outcome)
		// NEVER the text, and never the token: §5.2's rule is that a log is the
		// second place a one-time credential escapes to, and this path exists
		// precisely because the text may hold one.
		g.logger.Warn().Str("channel", channel).Str("outcome", outcome).
			Msg("chat: a link code was pasted outside the redemption command; removed before dispatch")
	}
	return scrubbed
}

// ExposureMetrics counts exposures by channel and outcome. Same holder shape as
// the shim's counter above, and for the same reason: the daemon builds this
// during subsystem init, where the served Prometheus registry may not exist
// yet, and a counter registered on an unserved registry reads as zero rather
// than as missing.
type ExposureMetrics struct {
	vec atomic.Pointer[prometheus.CounterVec]

	mu      sync.Mutex
	pending map[[2]string]int64
}

// NewExposureMetrics builds the holder.
func NewExposureMetrics(reg prometheus.Registerer) *ExposureMetrics {
	m := &ExposureMetrics{pending: map[[2]string]int64{}}
	m.Attach(reg)
	return m
}

// Attach registers the counter on the served registry and drains anything
// counted before it existed. Idempotent: initHTTPServer runs twice.
func (m *ExposureMetrics) Attach(reg prometheus.Registerer) {
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
			Name:      "chat_link_code_exposed_total",
			Help: "Link codes found in chat text OUTSIDE the redemption command and removed " +
				"before dispatch, by channel and outcome. outcome=revoked means the token was " +
				"a live code and is now burned; scrubbed means it was code-shaped but not live " +
				"(unknown, expired or already used — indistinguishable by design); burn_failed " +
				"means the store could not be reached, and the token was still kept away from " +
				"the model. A non-zero count is the control working: the code reached a channel " +
				"either way, and this is the daemon noticing.",
		},
		[]string{"channel", "outcome"},
	)
	m.vec.Store(vec)
	for key, n := range m.pending {
		vec.WithLabelValues(key[0], key[1]).Add(float64(n))
	}
	m.pending = map[[2]string]int64{}
}

func (m *ExposureMetrics) observe(channel, outcome string) {
	if m == nil {
		return
	}
	if vec := m.vec.Load(); vec != nil {
		vec.WithLabelValues(channel, outcome).Inc()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[[2]string{channel, outcome}]++
}
