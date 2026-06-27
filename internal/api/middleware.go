// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
)

// contextKey is a type for context keys in this package.
type contextKey string

const (
	// projectIDKey is the context key for the project ID.
	projectIDKey contextKey = "project_id"
	// apiKeyKey is the context key for the API key.
	apiKeyKey contextKey = "api_key"
	// apiKeyIDKey carries the api_keys.id of the matched DB-backed
	// row. Empty when auth resolved via the legacy static-key path.
	// Audit logs surface this so revocation history can be
	// cross-referenced.
	apiKeyIDKey contextKey = "api_key_id"
	// apiKeyClientKindKey carries api_keys.client_kind for DB-backed
	// companion keys. Empty means a regular key or legacy static key.
	apiKeyClientKindKey contextKey = "api_key_client_kind"
	// projectIDFromKeyKey carries the project a DB-backed key is
	// bound to. Distinct from projectIDKey (which carries the
	// static-map allowlist []string) because a DB key binds to
	// exactly one project — and that binding cannot be overridden
	// by an X-Vornik-Project-ID header downstream.
	projectIDFromKeyKey contextKey = "project_id_from_key"
	// authEnabledKey reflects whether the deployment is running
	// with API-key auth required. Downstream handlers read it to
	// decide whether unauthenticated client identity headers
	// (X-Operator-Id) can be trusted as session-ownership claims:
	//   - true  → reject the header; only the matched key may
	//     identify the caller
	//   - false → single-operator dev mode; header is the only
	//     identity available, accept it but treat as unverified
	authEnabledKey contextKey = "auth_enabled"
	// identityKey carries the *auth.Identity resolved by the
	// pluggable backend chain. The middleware stamps BOTH this and the
	// legacy keys above so downstream handlers can migrate to the
	// identity off the legacy context keys one at a time.
	identityKey contextKey = "auth_identity"
)

// noAccessSentinelProject is stamped as the sole entry of
// projectIDKey for an authenticated-but-awaiting-access session user
// (zero groups → zero projects). requestAllowsProject grants a
// project only when the stamped list contains the project ID; this
// value can never be a real project ID (the null byte makes it
// unrepresentable in any URL path or YAML key), so a session user
// with no group membership is denied every project-scoped route
// while still being recognised as authenticated. WITHOUT this, a
// zero-length Projects slice would fall into requestAllowsProject's
// "empty = all-access" branch and hand an awaiting-access user the
// keys to every project.
const noAccessSentinelProject = "\x00no-access"

// APIKeyLookup is the narrow slice of persistence.APIKeyRepository
// AuthMiddleware needs. Decoupled from the full repo interface so
// tests can supply a one-method stub without mocking every method.
type APIKeyLookup interface {
	LookupActiveByHash(ctx context.Context, keyHash string) (*persistence.APIKey, error)
}

// APIKeyToucher updates last_used_at after a successful auth.
// Separate from APIKeyLookup so the touch fires in its own
// goroutine without blocking the hot path on a write — and so
// tests can inject a no-op without driving the lookup side.
type APIKeyToucher interface {
	TouchLastUsed(ctx context.Context, keyID string) error
}

// Compile-time lockstep pins: the auth package duplicates these
// narrow interfaces (it cannot import internal/api). If either
// side's signature drifts, this stops compiling.
var (
	_ auth.APIKeyLookup  = (APIKeyLookup)(nil)
	_ auth.APIKeyToucher = (APIKeyToucher)(nil)
)

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	// Enabled controls whether authentication is required.
	Enabled bool
	// StaticAPIKeys is a map of valid API keys to their project permissions.
	// Key is the API key, value is the list of project IDs it can access.
	// An empty list means access to all projects.
	//
	// Legacy single-tenant path. Kept for backwards compatibility
	// with deployments that haven't run `vornikctl key migrate` yet.
	// New deployments should configure the DB-backed lookup via
	// APIKeyLookup below; both paths coexist during the 2026.6.0 →
	// 2026.8.0 deprecation window.
	StaticAPIKeys map[string][]string

	// APIKeyLookup is the DB-backed key lookup. When non-nil,
	// AuthMiddleware checks DB rows BEFORE the StaticAPIKeys map
	// — DB-backed keys take precedence. nil falls back to the
	// static map only (existing behaviour).
	APIKeyLookup APIKeyLookup

	// APIKeyToucher fires an async last_used_at update on every
	// successful DB-backed auth. Nil disables — the auth path
	// still works, the "last used" column just stays stale.
	APIKeyToucher APIKeyToucher

	// APIKeyLimiter throttles per-key request rate based on the
	// rate_limit_rps / rate_limit_burst columns on api_keys. Nil
	// disables enforcement (keys with configured limits get
	// through without being counted). Production wires this so
	// a leaked / runaway key can't DoS the daemon until revoked.
	APIKeyLimiter *ratelimit.APIKeyLimiter

	// RateLimitMetrics records Prometheus outcomes for the per-key
	// limiter — allow / warn / block. Nil is safe; observability
	// just stays at zero. Pairs with the warn header below so
	// operators see degradation on the dashboard before a 429
	// hits the client.
	RateLimitMetrics *ratelimit.Metrics

	// PerIPLimiter is the unauthenticated data-plane backstop
	// (sub-item 2 of rate-limit hardening). When non-nil AND
	// PerIPRateLimitRPS + PerIPRateLimitBurst are non-zero, it
	// throttles requests by client IP BEFORE auth so an
	// unauthenticated flood can't even reach the auth path. Same
	// token-bucket primitive as APIKeyLimiter; emits 429 with
	// Retry-After when drained.
	PerIPLimiter        *ratelimit.PerIPLimiter
	PerIPRateLimitRPS   int
	PerIPRateLimitBurst int

	// AuthFailures locks out a client IP after repeated API-key auth
	// failures (brute-force guard, distinct from the throughput limiters
	// above). Nil disables the lockout. IP is resolved via
	// realip.ClientIPFromContext — the spoof-safe centrally-resolved
	// client IP, so a forged header can't trip another client's lockout.
	AuthFailures *authFailureLimiter

	// SessionBackend authenticates browser login sessions via the
	// vornik_session cookie (Phase 3). When non-nil it is PREPENDED to
	// the chain (session → hmac → dbkeys → static) — the cookie is
	// unambiguous and cheap to check. Nil disables cookie auth entirely;
	// a stale cookie then never influences the request.
	SessionBackend auth.Backend

	// DryRun puts the middleware into observation-only mode. Every
	// request is served exactly as when Enabled=false, but the
	// middleware also evaluates the would-be auth verdict and records
	// a vornik_auth_dryrun_denials_total{verdict} metric increment +
	// a deduplicated WARN log for each would-be denial.
	//
	// DryRun is ignored when Enabled=true — the enabled path takes
	// precedence (config validation already blocks the combination,
	// but the middleware is defensive). Designed to soak-test the
	// auth flip (2026-05-28 rollback context) before enforcement.
	DryRun bool

	// DryRunMetrics holds the Prometheus counter for dry-run denials.
	// Nil disables metric emission; the dedup log still fires.
	// Wire via WithAuthDryRunMetrics in applyMiddleware.
	DryRunMetrics *DryRunMetrics

	// AdminKeyChecker reports whether a presented bearer key is
	// admin-class (admin.allowed_keys). An admin-class key stamps NO
	// projectIDKey restriction — the same rule session-admins get
	// (Principal.Projects ["*"] → stamp nothing) — so the project
	// gate and every row-level filter inherit the bypass from this
	// one stamp-time decision. Regression 2026-06-07 (bug-class
	// recurrence #3 after the f16ae834 reminders pair): the admin
	// key, present as a DB api_keys row bound to one project, was
	// scoped like an ordinary key and 403'd on every other project.
	// Nil = no admin-class keys (DB/static scoping applies to all).
	// Wired by BuildAuthConfig from cfg.Admin; constant-time compare
	// inside config.AdminConfig.IsAdminKey.
	AdminKeyChecker func(key string) bool

	// ChainMetrics records the per-backend admit/deny counter
	// (vornik_auth_backend_verdicts_total) at the chain-resolution
	// point. Nil disables recording (pass-1 container build, minimal
	// deployments, most tests). Pass the SAME instance to the api router
	// and the UI subtree, like DryRunMetrics.
	ChainMetrics *AuthChainMetrics
}

// isAdminClassKey is the nil-safe face of AdminKeyChecker.
func (c AuthConfig) isAdminClassKey(key string) bool {
	return c.AdminKeyChecker != nil && c.AdminKeyChecker(key)
}

// IsAuthEnabledFromContext reports whether API-key auth was
// enabled for the request that produced ctx. Stamped by
// AuthMiddleware on every request; absence is treated as
// "enabled" (the safer fail-closed default — a path that
// somehow bypassed the middleware shouldn't accidentally
// pretend auth is off).
//
// Used by the admin gate (internal/admin.Middleware) to
// disengage when auth is disabled. Single-operator deployments
// with auth off treat every caller as admin since the concept
// of "admin key vs regular key" is meaningless without keys.
func IsAuthEnabledFromContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	v, ok := ctx.Value(authEnabledKey).(bool)
	if !ok {
		return true
	}
	return v
}

// AuthMiddleware returns a middleware that validates static API keys.
// It extracts the Authorization header and validates the bearer token.
func AuthMiddleware(config AuthConfig) func(http.Handler) http.Handler {
	// Credential validation runs through the pluggable backend chain
	// (internal/auth). HMACWebhookBackend produces a pass-through identity
	// for signed key-less webhook deliveries; the DB-keys and static-keys
	// backends validate bearer tokens.
	backends := []auth.Backend{
		auth.NewHMACWebhookBackend(),
		auth.NewDBKeysBackend(config.APIKeyLookup, config.APIKeyToucher),
		auth.NewStaticKeysBackend(config.StaticAPIKeys),
	}
	// Session cookie backend joins FIRST when configured — the cookie is
	// unambiguous and cheap to check, and a dead cookie returns
	// ErrNoCredential so a Bearer on the same request still resolves via
	// the key backends below.
	if config.SessionBackend != nil {
		backends = append([]auth.Backend{config.SessionBackend}, backends...)
	}
	// Per-instance dedup map for dry-run warn logs. Allocating here (once
	// per AuthMiddleware call) gives each middleware instance its own store,
	// so tests are isolated and production instances don't cross-contaminate.
	// Production typically wires two instances (data-plane + UI subtree), so
	// at most two WARN lines fire per distinct (method+route+verdict) key.
	dryRunDedup := newDryRunDedup()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Stamp the auth-enabled flag on context unconditionally
			// — downstream handlers use it to decide whether the
			// X-Operator-Id header can be trusted as a session-
			// ownership claim. Set this BEFORE any return path so
			// every request the handler sees carries the flag.
			r = r.WithContext(context.WithValue(r.Context(), authEnabledKey, config.Enabled))
			// Health and metrics endpoints are intentionally public for probes and scrapes.
			if isPublicEndpoint(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			// Per-IP backstop — fires before auth so an
			// unauthenticated flood can't reach the auth path.
			// Health probes (above) are exempt so a saturated
			// limiter can't black-hole readiness checks.
			if enforcePerIPRateLimit(w, r, config.PerIPLimiter, config.RateLimitMetrics, config.PerIPRateLimitRPS, config.PerIPRateLimitBurst) {
				return
			}

			// Brute-force lockout: resolve the client IP once, refuse if it's
			// currently locked out for repeated auth failures, and arm a
			// failAuth helper the invalid-key paths below call so each wrong
			// guess counts toward the lockout.
			// Brute-force lockout keys on the centrally-resolved client
			// IP (realip.Middleware stored it in context). Fall back to
			// RemoteAddr's host when unset (non-middleware/test paths).
			// NEVER read a request header here — that was the spoofable
			// path that let an attacker trip a victim's lockout.
			authClientIP := realip.ClientIPFromContext(r.Context())
			if authClientIP == "" {
				authClientIP = realip.RemoteHost(r)
			}
			if ok, retry := config.AuthFailures.Allowed(authClientIP); !ok {
				secs := int(retry.Seconds())
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
				respondError(w, http.StatusTooManyRequests, "TOO_MANY_AUTH_FAILURES",
					"too many failed authentication attempts; try again later")
				return
			}
			failAuth := func(msg string) {
				config.AuthFailures.RecordFailure(authClientIP)
				respondUnauthorized(w, msg)
			}

			// If auth is disabled we still want to RESOLVE a DB-backed
			// bearer token when one is presented — not for security
			// (the request would pass anyway), but so downstream cost
			// attribution can credit the bound project. Without this
			// pass, every external-API call lands under `_external`
			// even when the caller is authenticating with a real
			// per-project key.
			if !config.Enabled {
				config.serveAuthDisabled(w, r, next, dryRunDedup)
				return
			}

			config.serveAuthEnabled(w, r, next, backends, failAuth)
		})
	}
}

// serveAuthEnabled resolves and enforces a credential when auth is on, then
// serves next with the identity + legacy context keys stamped. Extracted from
// AuthMiddleware's per-request closure. failAuth is the lockout-arming 401
// helper closed over the request's client IP.
func (c AuthConfig) serveAuthEnabled(w http.ResponseWriter, r *http.Request, next http.Handler, backends []auth.Backend, failAuth func(string)) {
	// Webhook endpoints accept either a valid API key (handled below) OR a
	// per-source HMAC signature (verified later by IngestWebhook against the
	// source's secret). When neither is present we reject up front so an
	// unauthenticated probe can't enumerate project / source names by their
	// distinct error codes. The per-path signature check ties the header to
	// the path: pre-fix, ANY of the 3 HMAC headers was accepted for ANY webhook
	// path, letting an attacker present X-Slack-Signature to
	// /api/v1/webhooks/{...} and read a distinct 401-vs-404 enumeration oracle.
	// A signed key-less delivery falls through to the chain, where
	// HMACWebhookBackend produces a pass-through identity.
	if isWebhookEndpoint(r.URL.Path) {
		if extractAPIKey(r) == "" && !hasWebhookSignatureForPath(r.URL.Path, r) {
			respondUnauthorized(w, "API key or HMAC signature required")
			return
		}
	}

	apiKey := extractAPIKey(r)
	chainWebhookSig := apiKey == "" &&
		isWebhookEndpoint(r.URL.Path) && hasWebhookSignatureForPath(r.URL.Path, r)
	// hasSessionCookie is true only when the session feature is wired AND the
	// request carries a vornik_session cookie. Cookies are never auto-attached
	// to programmatic callers, so this stays false for them.
	hasSessionCookie := false
	if c.SessionBackend != nil {
		if _, err := r.Cookie("vornik_session"); err == nil {
			hasSessionCookie = true
		}
	}
	if apiKey == "" && !chainWebhookSig && !hasSessionCookie {
		// Browser path: an unauthenticated GET to a UI page redirects to the
		// login screen instead of 401ing with a JSON blob the user can't act on.
		if shouldRedirectToLogin(r, c) {
			redirectToLogin(w, r)
			return
		}
		respondUnauthorized(w, "Missing API key")
		return
	}

	// CSRF gate. Browsers auto-attach Basic credentials and session cookies on
	// every cross-origin request to a remembered origin — the textbook CSRF
	// vector. Reject mutating requests that arrived via either AND look
	// cross-site. Bearer + X-API-Key callers (CLI, MCP, vornikctl, the companion
	// plugin) skip this gate entirely; browsers don't auto-attach those headers
	// cross-origin. See isCSRFSafe for the decision ladder.
	_, _, isBasic := r.BasicAuth()
	if (isBasic || hasSessionCookie) && !isCSRFSafe(r) && !isGitSmartHTTP(r) {
		// Log the denial with the signals that drove it — this gate is
		// otherwise silent, which made the git-over-HTTPS 403 hard to
		// diagnose (the request just 403s with no server-side trace).
		log.Warn().
			Str("component", "auth-csrf").
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Bool("basic_auth", isBasic).
			Bool("session_cookie", hasSessionCookie).
			Str("sec_fetch_site", r.Header.Get("Sec-Fetch-Site")).
			Str("origin", r.Header.Get("Origin")).
			Str("host", r.Host).
			Msg("CSRF gate blocked a cross-site mutating request (Basic/cookie, no same-origin signal); programmatic clients should use Authorization: Bearer or X-API-Key")
		respondError(w, http.StatusForbidden, "CSRF_BLOCKED",
			"cross-site mutating request via Basic Auth refused; use Authorization: Bearer for programmatic clients, or open the UI in the same origin")
		return
	}

	// Pluggable-backend chain. Validates the credential and stamps BOTH the new
	// identityKey and the legacy context keys downstream handlers still read.
	// HTTP-shape concerns (rate limit 429, companion 403) stay here — backends
	// return data, never write responses.
	cred := auth.Credential{BearerToken: apiKey, Path: r.URL.Path}
	if chainWebhookSig {
		cred.HMACPresent = true // handler re-verifies the actual signature
	}
	// Extract the session cookie BEFORE building the credential so
	// SessionBackend (first in the chain) can validate it. A dead cookie yields
	// ErrNoCredential and the chain falls through to the key backends.
	if c, err := r.Cookie("vornik_session"); err == nil {
		cred.SessionToken = c.Value
	}
	identity, err := auth.Chain(r.Context(), backends, cred)
	if err != nil {
		c.ChainMetrics.recordChainVerdict("none", "denied")
		// Browser path: a UI GET whose credential was rejected (or whose only
		// credential was a dead cookie) goes to login rather than a JSON 401.
		if shouldRedirectToLogin(r, c) {
			redirectToLogin(w, r)
			return
		}
		failAuth("Invalid API key")
		return
	}
	c.ChainMetrics.recordChainVerdict(identity.Backend, "admitted")
	// HTTP-shape guards that can short-circuit (they need w): a DB-backed key
	// is rate-limited and companion-confined before any context is stamped.
	if dbRow, ok := identity.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey); ok {
		if enforceKeyRateLimit(w, c.APIKeyLimiter, c.RateLimitMetrics, dbRow) {
			return
		}
		if dbRow.ClientKind != "" && !isCompanionAllowedPath(r.URL.Path) {
			respondError(w, http.StatusForbidden, "FORBIDDEN",
				"companion API keys may only use /api/v1/mcp/companion")
			return
		}
	}
	next.ServeHTTP(w, r.WithContext(stampIdentityContext(r.Context(), identity, apiKey, c)))
}

// stampIdentityContext returns ctx with the resolved identity plus the legacy
// context keys downstream handlers + scope filters still read. Pure: callers
// run the HTTP-shape guards (rate-limit 429, companion 403) BEFORE calling
// this. The mapping per backend:
//   - db-keys: api-key id/kind + bound project (admin-class keys stamp no
//     project restriction; the bound project stays on projectIDFromKeyKey).
//   - static-keys: api-key + allowed projects (admin-class → no restriction).
//   - session: NO api-key stamps. Projects map to projectIDKey so the same
//     IDOR guard every path uses applies. CAREFUL: requestAllowsProject treats
//     a MISSING projectIDKey (and a zero-length slice) as legacy all-access. An
//     admin / star-group ["*"] genuinely IS all-access → stamp nothing. An
//     awaiting-access user has ZERO projects and must NOT get all-access, so we
//     stamp the explicit no-access sentinel — it can never match a real project
//     id, so every project-scoped route 403s until the user joins a group.
//   - hmac-webhook: no legacy stamps.
func stampIdentityContext(ctx context.Context, identity *auth.Identity, apiKey string, config AuthConfig) context.Context {
	ctx = context.WithValue(ctx, identityKey, identity)
	if dbRow, ok := identity.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey); ok {
		ctx = context.WithValue(ctx, apiKeyKey, apiKey)
		ctx = context.WithValue(ctx, apiKeyIDKey, dbRow.ID)
		ctx = context.WithValue(ctx, apiKeyClientKindKey, dbRow.ClientKind)
		ctx = context.WithValue(ctx, projectIDFromKeyKey, dbRow.ProjectID)
		if !config.isAdminClassKey(apiKey) {
			ctx = context.WithValue(ctx, projectIDKey, []string{dbRow.ProjectID})
		}
		return ctx
	}
	switch identity.Backend {
	case "static-keys":
		ctx = context.WithValue(ctx, apiKeyKey, apiKey)
		if len(identity.Projects) > 0 && !config.isAdminClassKey(apiKey) {
			ctx = context.WithValue(ctx, projectIDKey, identity.Projects)
		}
	case "session":
		switch {
		case len(identity.Projects) == 1 && identity.Projects[0] == "*":
			// all-access; stamp nothing.
		case len(identity.Projects) > 0:
			ctx = context.WithValue(ctx, projectIDKey, identity.Projects)
		default:
			ctx = context.WithValue(ctx, projectIDKey, []string{noAccessSentinelProject})
		}
	}
	return ctx
}

// serveAuthDisabled handles the auth_enabled=false request path: it serves
// every request (auth off = no rejection) but still RESOLVES a presented
// credential so downstream cost attribution + the UI's signed-in state work,
// and records the would-be dry-run verdict. Extracted from AuthMiddleware.
//
// A presented vornik_session cookie is resolved to an identity stamp ONLY (no
// projectIDKey — auth off means no scoping; the enabled path's no-access
// sentinel would close pages that are open today) and never rejected. A
// presented per-project bearer is resolved for attribution (rate-limited even
// with auth off — the limit is a property of the key, not the toggle).
func (c AuthConfig) serveAuthDisabled(w http.ResponseWriter, r *http.Request, next http.Handler, dryRunDedup *sync.Map) {
	if c.SessionBackend != nil {
		if cookie, err := r.Cookie("vornik_session"); err == nil {
			cred := auth.Credential{SessionToken: cookie.Value, Path: r.URL.Path}
			if identity, err := auth.Chain(r.Context(), []auth.Backend{c.SessionBackend}, cred); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), identityKey, identity))
			}
		}
	}
	// Extract the key once — used for the attribution lookup below AND passed
	// to dryRunVerdict to avoid a duplicate extractAPIKey + LookupActiveByHash.
	disabledBranchKey := extractAPIKey(r)
	if dbKey := c.attributionKeyForDisabledAuth(r, disabledBranchKey); dbKey != nil {
		// Rate-limit even with auth off — the limit is a property of the key,
		// not of the auth toggle.
		if enforceKeyRateLimit(w, c.APIKeyLimiter, c.RateLimitMetrics, dbKey) {
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyKey, disabledBranchKey)
		ctx = context.WithValue(ctx, apiKeyIDKey, dbKey.ID)
		ctx = context.WithValue(ctx, apiKeyClientKindKey, dbKey.ClientKind)
		ctx = context.WithValue(ctx, projectIDFromKeyKey, dbKey.ProjectID)
		ctx = context.WithValue(ctx, projectIDKey, []string{dbKey.ProjectID})
		if c.APIKeyToucher != nil {
			go func(id string) {
				_ = c.APIKeyToucher.TouchLastUsed(context.Background(), id)
			}(dbKey.ID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	// Dry-run: compute the would-be verdict without enforcing. Runs AFTER the
	// session + DB-key resolution above so the live-session check in
	// dryRunVerdict can read the already-stamped identity from context.
	if c.DryRun {
		if verdict := dryRunVerdict(r, &c, disabledBranchKey); verdict != "" {
			c.recordDryRunDenial(r, verdict, dryRunDedup)
		}
	}
	next.ServeHTTP(w, r)
}

// attributionKeyForDisabledAuth resolves a presented per-project bearer to its
// active api_keys row for cost attribution under auth_enabled=false, or nil
// when no valid per-project key is presented. Guard-clause flattened (the
// caller then rate-limits + stamps). Defence-in-depth: the prefix-embedded
// project must match the row's project, else the key is treated as no-match.
func (c AuthConfig) attributionKeyForDisabledAuth(r *http.Request, key string) *persistence.APIKey {
	if c.APIKeyLookup == nil || key == "" || !strings.HasPrefix(key, apikey.Prefix+"-") {
		return nil
	}
	dbKey, err := c.APIKeyLookup.LookupActiveByHash(r.Context(), apikey.Hash(key))
	if err != nil {
		return nil
	}
	claimedProject, _, parseErr := apikey.Parse(key)
	if parseErr != nil || !apikey.MatchesProject(claimedProject, dbKey.ProjectID) {
		return nil
	}
	return dbKey
}

// ProjectAuthMiddleware validates that the request has access to the
// specified project. It must be used after AuthMiddleware.
//
// The scope check itself is delegated to requestAllowsProject — same
// helper every per-handler visibility filter uses. Centralising the
// logic prevents drift between the route-level gate and the row-level
// filter (the "scoped key sees foreign project" leak class).
//
// The middleware's only added responsibility on top of the helper is
// extracting the project ID from the URL and short-circuiting on
// non-project routes (e.g. /api/v1/health); the helper itself rejects
// empty project IDs by design.
func ProjectAuthMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			projectID := extractProjectID(r)
			if projectID == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !requestAllowsProject(r, projectID) {
				respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
				return
			}
			// Companion keys have no legitimate project-scoped surface:
			// their routes (/api/v1/mcp/companion, /api/v1/capabilities)
			// all yield an empty projectID and short-circuited above. So
			// reaching here with a non-empty client_kind means a companion
			// key is hitting a project-scoped route — REST (/api/v1/projects/)
			// or A2A (/a2a/v1/agents/), both of which can submit work and
			// bypass the companion MCP allowlist/budget gates. Block all of
			// them, not just the /api/v1/projects/ prefix.
			if APIKeyClientKindFromContext(r.Context()) != "" {
				respondError(w, http.StatusForbidden, "FORBIDDEN",
					"companion API keys may only use /api/v1/mcp/companion")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// APIKeyFromContext returns the bearer key AuthMiddleware stashed
// on the request context, or "" when none is present. Exported for
// downstream middleware (the admin gate in particular) so it can
// avoid re-parsing the Authorization header just to read the
// already-validated key.
func APIKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(apiKeyKey).(string)
	return v
}

// IdentityFromContext returns the chain-resolved Identity, or nil
// on the legacy path / unauthenticated requests. Downstream code
// migrating off the legacy context keys reads this instead.
func IdentityFromContext(ctx context.Context) *auth.Identity {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(identityKey).(*auth.Identity)
	return v
}

// SessionRoleFromContext returns the role ("admin" | "user") stamped
// by SessionBackend when the request authenticated via a browser
// session cookie, or "" for key-based / unauthenticated requests.
// The admin gate reads "admin" here to let a session-admin through
// alongside the api-key admin allowlist.
func SessionRoleFromContext(ctx context.Context) string {
	id := IdentityFromContext(ctx)
	if id == nil || id.Extra == nil {
		return ""
	}
	v, _ := id.Extra[auth.ExtraSessionRole].(string)
	return v
}

// SessionIDFromContext returns the ui_sessions.id of the session
// that authenticated the request, or "" when the request did not
// authenticate via a session cookie. The logout handler reads this
// to revoke the exact session backing the cookie.
func SessionIDFromContext(ctx context.Context) string {
	id := IdentityFromContext(ctx)
	if id == nil || id.Extra == nil {
		return ""
	}
	v, _ := id.Extra[auth.ExtraSessionID].(string)
	return v
}

// shouldRedirectToLogin reports whether an unauthenticated request
// should be sent to the login page (302) instead of receiving a JSON
// 401. True only for a browser GET to a /ui page when the session
// feature is wired — programmatic callers (no SessionBackend, or
// non-GET, or no text/html Accept) keep today's 401 exactly, so the
// flag-equivalence contract for non-session deployments is untouched.
//
//   - /ui/login is excluded (it's public; the middleware never
//     reaches a redirect for it) so we don't loop.
//   - ?method=key is excluded: it's the break-glass path, which must
//     surface the 401 + WWW-Authenticate dialog (the login handler
//     emits that for /ui/login itself; this guard covers any other
//     /ui page reached with method=key).
func shouldRedirectToLogin(r *http.Request, cfg AuthConfig) bool {
	if cfg.SessionBackend == nil {
		return false
	}
	if r.Method != http.MethodGet {
		return false
	}
	path := r.URL.Path
	if !strings.HasPrefix(path, "/ui") {
		return false
	}
	if path == "/ui/login" {
		return false
	}
	if r.URL.Query().Get("method") == "key" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// redirectToLogin clears any stale vornik_session cookie and 302s to
// the login page, preserving the originally-requested URI in ?next so
// the user lands where they meant to go after authenticating.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "vornik_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, "/ui/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
}

// APIKeyIDFromContext returns the DB-backed `api_keys.id` of the
// matched key, or "" for static-keys-only requests / unauth. Exported
// so audit-writing handlers (B-15: memory search) can stamp the
// `actor_id` audit column without re-resolving the key. Matches the
// companion-side shape (`*persistence.APIKey.ID`) so dashboards see
// one ID-space across surfaces.
func APIKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(apiKeyIDKey).(string)
	return v
}

// isCompanionAllowedPath is the positive allowlist of routes a companion
// (DB-backed client_kind) key may use. Exact match only — never a prefix —
// so a companion key cannot reach /api/v1/internal/*, /api/v1/instincts/*,
// or /api/v1/memory/* via a path-prefix trick. Keep in sync with the
// companion's documented surface (the MCP dispatch endpoint + capability
// discovery).
func isCompanionAllowedPath(p string) bool {
	switch p {
	case "/api/v1/mcp/companion", "/api/v1/capabilities":
		return true
	}
	return false
}

// APIKeyClientKindFromContext returns the DB-backed companion client
// kind stamped by AuthMiddleware. Empty means the request authenticated
// with a regular DB key, a legacy static key, or no key.
func APIKeyClientKindFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(apiKeyClientKindKey).(string)
	return v
}

// ContextWithScopeForTesting stamps an auth-enabled flag + a
// project scope onto ctx so sibling packages can exercise the
// scoped-key code paths without instantiating the full
// AuthMiddleware. Passing zero projects simulates an admin key
// (auth on, no scope). The exported name carries `ForTesting`
// so production code doesn't accidentally reach for it; the
// helper is the only seam through which the unexported context
// keys leak into other packages.
func ContextWithScopeForTesting(ctx context.Context, projects ...string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, authEnabledKey, true)
	if len(projects) > 0 {
		// Copy to defend against the test mutating the slice
		// after passing it in.
		cp := make([]string, len(projects))
		copy(cp, projects)
		ctx = context.WithValue(ctx, projectIDKey, cp)
	}
	return ctx
}

// apiKeyPrincipalFromContext returns a stable audit/ownership
// principal for the matched key without persisting the bearer token
// itself. DB-backed keys prefer their row ID; legacy static keys fall
// back to a short SHA-256 fingerprint.
func apiKeyPrincipalFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, _ := ctx.Value(apiKeyIDKey).(string); id != "" {
		return "api_key_id:" + id
	}
	if key := APIKeyFromContext(ctx); key != "" {
		sum := apikey.Hash(key)
		if len(sum) > 16 {
			sum = sum[:16]
		}
		return "api_key_sha256:" + sum
	}
	return ""
}

// lookupAPIKey walks the configured key set in constant time. A plain map
// lookup would short-circuit on the first differing byte of the hash key,
// giving a network observer a timing oracle for key enumeration.
//
// Both sides are first hashed via apikey.Hash, which produces a fixed-
// length 64-char hex digest of the SHA-256 sum. That neutralises the
// documented behaviour of subtle.ConstantTimeCompare returning 0 in O(1)
// when the slice lengths differ — if we passed raw key bytes, a short
// or long presented key would return faster than a same-length one, and
// an attacker could binary-search the key length even without ever
// guessing the contents.
//
// Hashing the configured keys on every request is acceptable: the
// static-map path is legacy single-tenant only (the DB-backed path is
// the production deployment) and configured key sets are tiny (under
// a dozen entries in practice). For larger sets the configured side
// can be pre-hashed at AuthMiddleware construction time.
func lookupAPIKey(keys map[string][]string, presented string) ([]string, bool) {
	presentedHash := []byte(apikey.Hash(presented))
	var match []string
	found := 0
	for k, v := range keys {
		stored := []byte(apikey.Hash(k))
		if subtle.ConstantTimeCompare(stored, presentedHash) == 1 {
			match = v
			found = 1
		}
	}
	return match, found == 1
}

func isPublicEndpoint(path string) bool {
	switch path {
	case "/livez", "/healthz", "/readyz", "/health/live", "/health/ready", "/metrics":
		return true
	case "/ui/login":
		// The login page renders provider buttons + the break-glass
		// link; it carries no project data and MUST be reachable
		// unauthenticated (an authenticated user wouldn't need it).
		// Public on BOTH flag modes — it's harmless when the session
		// feature isn't wired (the UI just renders an empty provider
		// list + the key form). The `?method=key` break-glass 401 is
		// produced by the login HANDLER itself, not here, so this
		// exemption is unconditional.
		return true
	}
	// A2A agent cards are public per spec — clients fetch the card
	// BEFORE they have credentials to submit a task. Per-agent
	// task submission + SSE stream endpoints under /api/v1/... go
	// through normal auth.
	// See https://docs.vornik.io
	if path == "/.well-known/agent.json" || strings.HasPrefix(path, "/.well-known/agent.json/") {
		return true
	}
	// Static UI assets (htmx, icons, PWA manifest) carry no data and
	// are referenced by the login screen's chrome — without this
	// exemption the pre-login page renders with 401'd favicon/manifest
	// (2026-06-06 dry-run soak triage). Prefix-exact: /ui/staticX
	// stays gated.
	if strings.HasPrefix(path, "/ui/static/") {
		return true
	}
	// iOS Safari root-path icon probes — iOS probes these well-known root
	// paths when adding a page to the home screen, before following any
	// <link rel="apple-touch-icon"> tag. Without this exemption they hit
	// AuthMiddleware and get 401, leaving iOS with no icon (falls back to
	// a page screenshot). Exact-match only — nothing else is opened up.
	switch path {
	case "/favicon.ico", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png":
		return true
	}
	return false
}

// isWebhookEndpoint reports whether the path matches an HMAC-signed
// webhook ingest route. AuthMiddleware uses this to relax the
// API-key requirement when an HMAC signature header is present; the
// endpoint handler still verifies that signature against its own
// configured secret before accepting the delivery.
func isWebhookEndpoint(path string) bool {
	return strings.HasPrefix(path, "/api/v1/webhooks/") ||
		path == "/api/v1/github-app/webhook" ||
		path == "/api/v1/slack/webhook"
}

// hasWebhookSignatureForPath reports whether the request bears the
// HMAC signature header that the path's handler will actually
// verify. Pre-fix the auth middleware accepted any of the 3
// signature headers for any webhook path, letting a caller present
// (say) X-Slack-Signature against /api/v1/webhooks/{project}/{src}
// — the auth gate let it through, then verifyWebhookSignature at
// the handler rejected with a specific reason that an attacker
// could distinguish from a 404 on an unknown project, enumerating
// valid project/source pairs without an API key. Per-path
// scoping closes the oracle.
func hasWebhookSignatureForPath(path string, r *http.Request) bool {
	switch {
	case path == "/api/v1/slack/webhook":
		return strings.TrimSpace(r.Header.Get("X-Slack-Signature")) != ""
	case path == "/api/v1/github-app/webhook":
		return strings.TrimSpace(r.Header.Get("X-Hub-Signature-256")) != ""
	case strings.HasPrefix(path, "/api/v1/webhooks/"):
		// Generic ingest path accepts the daemon's own
		// X-Vornik-Signature plus the GitHub-compat
		// X-Hub-Signature-256 (the generic verifier handles both).
		return strings.TrimSpace(r.Header.Get("X-Vornik-Signature")) != "" ||
			strings.TrimSpace(r.Header.Get("X-Hub-Signature-256")) != ""
	default:
		return false
	}
}

// PerIPLimit applies the per-IP backstop as a standalone middleware
// for routes mounted OUTSIDE AuthMiddleware (the /auth/* login flow).
// Same limiter instance as the main router so the budget is shared —
// an attacker can't double their quota by splitting requests across
// the /auth/* and /api/* surfaces.
//
// When l is nil OR rps/burst are zero the returned middleware is a
// transparent pass-through (same no-op contract as the per-IP gate
// inside AuthMiddleware).
func PerIPLimit(l *ratelimit.PerIPLimiter, metrics *ratelimit.Metrics, rps, burst int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if enforcePerIPRateLimit(w, r, l, metrics, rps, burst) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// enforcePerIPRateLimit returns true when the per-IP backstop
// blocked the request — same contract as enforceKeyRateLimit:
// caller MUST return without further processing after the 429
// has been written. Returns false when no limit is configured
// OR the bucket has tokens remaining. The IP-extraction logic
// honours X-Forwarded-For only when the caller's RemoteAddr is
// in the configured trusted-proxies list.
func enforcePerIPRateLimit(w http.ResponseWriter, r *http.Request, limiter *ratelimit.PerIPLimiter, metrics *ratelimit.Metrics, rps, burst int) bool {
	if limiter == nil || rps <= 0 || burst <= 0 {
		return false
	}
	d, ip := limiter.Allow(r, rps, burst, time.Now())
	if ip == "" {
		// Unparseable RemoteAddr — skip the limiter rather than
		// risking a wide-net match. Tighten if abuse from
		// malformed peer addresses becomes a real attack vector.
		return false
	}
	if metrics != nil {
		metrics.Observe(ratelimit.ScopeIP, ip, d)
	}
	if d.Warn {
		w.Header().Set("X-Vornik-RateLimit-Warning",
			fmt.Sprintf("ip-tokens=%.1f/%d threshold=%.0f%%",
				d.RemainingTokens, burst, ratelimit.WarnThresholdFrac*100))
	}
	if !d.Blocked {
		return false
	}
	retrySecs := int(d.RetryAfter.Seconds())
	if d.RetryAfter%time.Second != 0 {
		retrySecs++
	}
	if retrySecs < 1 {
		retrySecs = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySecs))
	respondError(w, http.StatusTooManyRequests, "RATE_LIMITED",
		fmt.Sprintf("per-IP rate limit reached (rps=%d burst=%d); retry after %ds",
			rps, burst, retrySecs))
	return true
}

// enforceKeyRateLimit returns true when the per-key rate limit
// blocked the request — in which case it ALREADY wrote the 429
// response and the caller MUST return without further processing.
// Returns false to mean "no limit configured / limit not exceeded;
// proceed with the request".
//
// The Retry-After header is in seconds (HTTP/1.1 §7.1.3); we round
// the bucket's nanosecond deficit UP to the next whole second so
// the client backs off at least as long as the bucket needs.
func enforceKeyRateLimit(w http.ResponseWriter, limiter *ratelimit.APIKeyLimiter, metrics *ratelimit.Metrics, key *persistence.APIKey) bool {
	if limiter == nil || key == nil || key.RateLimitRPS == nil || key.RateLimitBurst == nil {
		return false
	}
	d := limiter.Allow(key.ID, *key.RateLimitRPS, *key.RateLimitBurst, time.Now())
	if metrics != nil {
		metrics.Observe(ratelimit.ScopeAPIKey, key.ID, d)
	}

	// Warn header fires for both warn-only AND blocked outcomes (the
	// limiter sets Warn=true on block too). Clients see a hint
	// before the 429 — they can scale back without hitting the
	// ceiling.
	if d.Warn {
		w.Header().Set("X-Vornik-RateLimit-Warning",
			fmt.Sprintf("tokens=%.1f/%d threshold=%.0f%%",
				d.RemainingTokens, *key.RateLimitBurst, ratelimit.WarnThresholdFrac*100))
	}
	if !d.Blocked {
		return false
	}
	retrySecs := int(d.RetryAfter.Seconds())
	if d.RetryAfter%time.Second != 0 {
		retrySecs++
	}
	if retrySecs < 1 {
		retrySecs = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySecs))
	respondError(w, http.StatusTooManyRequests, "RATE_LIMITED",
		fmt.Sprintf("per-key rate limit reached (rps=%d burst=%d); retry after %ds",
			*key.RateLimitRPS, *key.RateLimitBurst, retrySecs))
	return true
}

// isCSRFSafe reports whether r is safe to dispatch when the
// caller authenticated via HTTP Basic. Basic Auth credentials are
// auto-attached by browsers on every request to a remembered
// origin, so a mutating request triggered by an attacker-controlled
// page in another tab would carry the user's UI bearer too — the
// textbook CSRF vector. Bearer + X-API-Key requests skip this
// gate entirely (browsers don't auto-attach custom headers cross-
// origin), so CLI / MCP / curl callers are unaffected.
//
// Decision ladder, evaluated top-down:
//
//  1. Non-mutating method (GET/HEAD/OPTIONS) → safe. CSRF
//     protections only need to cover state changes; reading
//     UI pages cross-origin is benign.
//  2. Sec-Fetch-Site header present (modern browsers, Chrome 76+,
//     Firefox 90+, Safari 16.4+):
//     - "same-origin" / "same-site" / "none" → safe.
//     - "cross-site" → unsafe.
//  3. Sec-Fetch-Site absent → fall back to Origin:
//     - No Origin → UNSAFE (fail closed). A2/A3 audit finding A3
//     (https://docs.vornik.io): a mutating
//     request on the Basic/cookie path carrying NEITHER
//     Sec-Fetch-Site NOR Origin gives the gate no same-origin
//     signal to rely on, so it must fail closed rather than
//     fail open. Realistic browser CSRF vectors are already
//     closed by SameSite=Lax; this is defense-in-depth. NOTE:
//     non-browser Basic clients (curl/scripts) that hit this
//     path MUST send an Origin header matching the host, or
//     switch to Authorization: Bearer / X-API-Key (which bypass
//     this gate entirely). This branch is never reached for
//     Bearer/X-API-Key callers — they skip the CSRF gate before
//     isCSRFSafe is consulted.
//     - Origin host matches r.Host → safe.
//     - Otherwise → unsafe.
//
// Reverse-proxy edge case: when vornik sits behind TLS termination
// on a different hostname, the proxy MUST forward the original
// Host header (X-Forwarded-Host is not consulted today). The
// modern Sec-Fetch-Site path is unaffected — it doesn't depend on
// host equality.
// gitSmartHTTPContentTypes are the content-types git sets on its mutating
// smart-HTTP RPC POSTs (clone/fetch negotiation and push). They are NOT CORS
// "simple" content-types, so a cross-site browser fetch cannot set them
// without a preflight vornik never answers permissively — only a real
// (non-browser) git client can produce such a request.
var gitSmartHTTPContentTypes = map[string]bool{
	"application/x-git-upload-pack-request":  true,
	"application/x-git-receive-pack-request": true,
}

// isGitSmartHTTP reports whether the request is a git smart-HTTP RPC POST on
// the git transport (/api/v1/git/...) carrying a git smart-HTTP content-type.
// Such requests are exempt from the browser-oriented CSRF gate, for two
// independent reasons:
//
//  1. The git layer (gitHTTPAuth) authenticates ONLY via the per-project API
//     key (Basic password / Bearer / X-API-Key) and ignores the session
//     cookie, so the ambient-cookie CSRF vector the gate guards is already
//     closed for git routes.
//  2. The git RPC content-type cannot be set by a cross-site browser fetch
//     without a CORS preflight that vornik does not grant, so a forged
//     browser request can never reach this branch.
//
// Without the exemption the gate's fail-closed branch 403s every clone/push
// RPC POST (a CLI sends no Sec-Fetch-Site and no Origin), breaking
// git-over-HTTPS entirely. The non-mutating info/refs GET needs no exemption —
// it is already CSRF-safe by method. Incident: git-over-HTTPS 403 CSRF_BLOCKED
// (2026-06-21).
func isGitSmartHTTP(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/git/") {
		return false
	}
	// Content-Type may carry parameters (e.g. "...; charset=..."); match the
	// media type prefix before any ';'.
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return gitSmartHTTPContentTypes[ct]
}

func isCSRFSafe(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "same-site", "none":
		return true
	case "cross-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// A3: neither Sec-Fetch-Site nor Origin present on a mutating
		// Basic/cookie request — no trustworthy same-origin signal.
		// Fail closed (the caller returns 403 CSRF_BLOCKED).
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		// Malformed Origin from a browser is a strong signal of
		// tampering. Conservative default: reject.
		return false
	}
	return parsed.Host == r.Host
}

// respondUnauthorized writes a 401 with both the JSON body
// non-browser callers expect AND the WWW-Authenticate challenge a
// browser needs to pop a credential dialog. Browsers ignore the
// JSON body; CLI / MCP clients ignore the header. Without this
// pair, browser users see a JSON blob and have nowhere to enter a
// bearer token.
//
// realm="vornik" is what the browser shows in the prompt header;
// users type any username + their API-key bearer as the password.
// See extractAPIKey for the Basic→bearer decode.
func respondUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="vornik"`)
	respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", message)
}

// extractAPIKey supports both the legacy X-API-Key header used by the E2E guide
// and standard Bearer authorization.
func extractAPIKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}

	// HTTP Basic Auth: browsers can't send `Authorization: Bearer ...`
	// from a plain URL, so we let them carry the bearer as the
	// password field of a Basic credential. The username is ignored
	// (operators can type any value, conventionally "api"). This
	// keeps single-user / single-operator deployments accessible from
	// a browser without adding a login template, session cookies, or
	// CSRF protection — the credential check is still the same
	// bearer-token path every other client uses.
	//
	// MUST be checked BEFORE the Bearer branch so a request that
	// carries both (rare; some HTTP debuggers do) is parsed
	// consistently: Bearer wins.
	if _, pass, ok := r.BasicAuth(); ok {
		if v := strings.TrimSpace(pass); v != "" {
			// Allow `Basic <user>:<bearer>` too — some clients
			// prepend `Bearer ` to the password field.
			parts := strings.SplitN(v, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				return strings.TrimSpace(parts[1])
			}
			return v
		}
	}

	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}

	return strings.TrimSpace(parts[1])
}

// respondError writes a JSON error response. Uses json.Marshal so
// message strings containing quotes, newlines, or other JSON-sensitive
// characters don't corrupt the response body — the previous hand-spliced
// version produced invalid JSON any time a subprocess's stderr (with
// its own newlines) reached the error path.
func respondError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, err := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
	if err != nil {
		// json.Marshal on plain strings shouldn't fail; if it does,
		// fall back to an opaque body so the client at least sees the
		// right status code.
		_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL","message":"error marshalling error"}}`))
		return
	}
	_, _ = w.Write(body)
}
