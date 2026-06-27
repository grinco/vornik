package api

// Dry-run evaluation mode for AuthMiddleware.
//
// When AuthConfig.DryRun is true (and Enabled is false), every request
// is served exactly as in disabled mode. In addition, the middleware
// evaluates the would-be auth verdict and, if it would have been a
// denial, increments vornik_auth_dryrun_denials_total{verdict} and emits
// a deduplicated WARN log.
//
// Dedup: one warn log per distinct (method + routeShape + verdict) key
// per middleware instance. The metric always increments. A sync.Map is
// used for the dedup store so it is safe under concurrent requests.
// Production typically wires two AuthMiddleware instances (data-plane
// and UI), so at most two WARN lines are emitted per distinct signature.
//
// Public endpoints are never counted — they pass on both flag modes.
//
// Verdict classes:
//   "missing_credential" — no key, no HMAC sig, no session → would-be 401
//   "invalid_key"        — key present but no match in static map or DB
//   "dead_session"       — vornik_session cookie presented but did not resolve

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"vornik.io/vornik/internal/apikey"
)

const (
	dryRunVerdictMissingCred = "missing_credential"
	dryRunVerdictInvalidKey  = "invalid_key"
	dryRunVerdictDeadSession = "dead_session"
)

// DryRunMetrics holds the Prometheus counter for dry-run denials.
// Registered once per process (or per test registry). Nil-safe via the
// recordDryRunDenial method.
type DryRunMetrics struct {
	DenialsTotal *prometheus.CounterVec
}

// NewDryRunMetrics registers and returns the dry-run denial counter.
// Pass nil to use prometheus.DefaultRegisterer.
// Call from applyMiddleware (or server setup) exactly like NewMetrics.
func NewDryRunMetrics(registerer prometheus.Registerer) *DryRunMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &DryRunMetrics{
		DenialsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "auth_dryrun_denials_total",
				Help:      "Dry-run auth evaluation denials by verdict class. Incremented on every would-be denial (missing_credential | invalid_key | dead_session). Always increments — use the dedup warn log to find distinct path shapes.",
			},
			[]string{"verdict"},
		),
	}
}

// newDryRunDedup returns a fresh sync.Map for use as a per-middleware-instance
// dedup store. AuthMiddleware allocates one per call so tests get isolation and
// production instances don't cross-contaminate.
func newDryRunDedup() *sync.Map { return new(sync.Map) }

// dryRunRouteShape collapses a concrete request path to a canonical
// route-shaped label, reducing cardinality in the dedup store. Reuses
// normalizePath from metrics.go which already implements the same pattern
// for HTTP metrics labels.
func dryRunRouteShape(path string) string {
	return normalizePath(path)
}

// recordDryRunDenial increments the metric counter and, on the first
// occurrence of (method + routeShape + verdict) in dedup, emits a WARN log.
// dedup is the per-middleware-instance sync.Map allocated by AuthMiddleware.
func (cfg *AuthConfig) recordDryRunDenial(r *http.Request, verdict string, dedup *sync.Map) {
	// Always increment the metric (even if the log is deduped).
	if cfg.DryRunMetrics != nil {
		cfg.DryRunMetrics.DenialsTotal.WithLabelValues(verdict).Inc()
	}

	// Deduplicated warn log: emit once per distinct (method + routeShape + verdict).
	key := fmt.Sprintf("%s %s %s", r.Method, dryRunRouteShape(r.URL.Path), verdict)
	if _, loaded := dedup.LoadOrStore(key, struct{}{}); !loaded {
		log.Warn().
			Str("component", "auth-dryrun").
			Str("method", r.Method).
			Str("route_shape", dryRunRouteShape(r.URL.Path)).
			Str("verdict", verdict).
			Msg("dry-run: would-be auth denial — enable auth_enabled: true to enforce")
	}
}

// dryRunVerdict evaluates whether the request would pass or be denied
// when auth is enabled. Returns "" (would pass) or a denial class.
//
// apiKey is the already-extracted bearer token from the disabled-branch
// attribution block in AuthMiddleware — passed in to avoid a second
// extractAPIKey call and a redundant LookupActiveByHash for DB keys.
//
// Decision ladder (mirrors the enabled-path's order):
//  1. Public endpoint → always passes (return "")
//  2. Identity already stamped on context (live session resolved by the
//     disabled-branch session-resolution block above) → passes.
//  3. Webhook endpoint with the right HMAC signature → passes.
//  4. API key present:
//     a. matches static map → passes.
//     b. matches DB lookup → passes.
//     c. presented but no match → "invalid_key".
//  5. vornik_session cookie present but NOT resolved (dead) → "dead_session".
//  6. Nothing presented → "missing_credential".
//
// READ-ONLY: never consumes rate-limit tokens, never writes a response,
// never redirects.
func dryRunVerdict(r *http.Request, cfg *AuthConfig, apiKey string) string {
	// (1) Public endpoints never produce a denial.
	if isPublicEndpoint(r.URL.Path) {
		return ""
	}

	// (2) A live session was already resolved by the disabled-branch
	// session-resolution block (it runs BEFORE dry-run).
	if id := IdentityFromContext(r.Context()); id != nil {
		return ""
	}

	// (3) Webhook endpoint with the correct HMAC header for the path.
	if isWebhookEndpoint(r.URL.Path) && hasWebhookSignatureForPath(r.URL.Path, r) {
		return ""
	}

	// (4) API key evaluation — read-only, no rate-limit consumption.
	// apiKey was already extracted by the caller; no second parse needed.
	if apiKey != "" {
		// (4a) Static map — constant-time lookup.
		if _, ok := lookupAPIKey(cfg.StaticAPIKeys, apiKey); ok {
			return ""
		}
		// (4b) DB lookup — read-only (no touch, no rate-limit).
		if cfg.APIKeyLookup != nil && strings.HasPrefix(apiKey, apikey.Prefix+"-") {
			if dbKey, err := cfg.APIKeyLookup.LookupActiveByHash(r.Context(), apikey.Hash(apiKey)); err == nil {
				if claimedProject, _, parseErr := apikey.Parse(apiKey); parseErr == nil && apikey.MatchesProject(claimedProject, dbKey.ProjectID) {
					return "" // would pass
				}
			}
		}
		// (4c) Key presented but no match.
		return dryRunVerdictInvalidKey
	}

	// (5) vornik_session cookie present but NOT resolved (dead/stale).
	// The disabled-branch resolution ran first; if the cookie failed
	// (no identity stamped, checked in step 2 above), it's dead.
	if cfg.SessionBackend != nil {
		if _, err := r.Cookie("vornik_session"); err == nil {
			// Cookie present but identity was nil (step 2 returned no "").
			return dryRunVerdictDeadSession
		}
	}

	// (6) Nothing presented.
	return dryRunVerdictMissingCred
}
