package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AttributionSource describes which input the project ID for a
// cost row was derived from. Surfaced on the persistence row so
// operators can grep for `attribution_source: "anonymous"` to
// find keys that should be migrated to the DB-backed flow.
type AttributionSource string

const (
	// AttributionFromDBKey — request authenticated with a
	// DB-backed bearer token. The token's bound project is the
	// authoritative billing target; any X-Vornik-Project-ID
	// header is IGNORED on this path. Trustworthy.
	AttributionFromDBKey AttributionSource = "key-bound"
	// AttributionFromHeader — request used the legacy static-key
	// path and supplied X-Vornik-Project-ID. The caller could
	// have set this to any value; the billing dashboard should
	// not treat these rows as canonical without operator review.
	AttributionFromHeader AttributionSource = "header"
	// AttributionFromFallback — the daemon-wide
	// external_api_billing_project_id pinned the project. No
	// per-request signal; rows from different clients land
	// together.
	AttributionFromFallback AttributionSource = "fallback"
	// AttributionAnonymous — none of the above; the row lands
	// under the literal "_external" bucket. Worth grepping for —
	// every "_external" row is a sign that auth could not derive
	// a project from the request.
	AttributionAnonymous AttributionSource = "anonymous"
)

// projectForCostAttribution derives the project ID a cost-row
// should be recorded under, ranking in order of trustworthiness:
//
//  1. DB-backed bearer token (cannot be overridden by header)
//  2. Legacy X-Vornik-Project-ID header
//  3. Daemon-wide ExternalAPIBillingProjectID fallback
//  4. Literal "_external" sentinel
//
// Returns the project ID + the source enum so callers can record
// both. Callers should never mutate the project ID after this
// returns — the chain has already applied every operator-trusted
// override.
func projectForCostAttribution(ctx context.Context, r *http.Request, fallback string) (string, AttributionSource) {
	// 1. DB-backed key wins. AuthMiddleware stores the bound
	// project under projectIDFromKeyKey; if present, this is the
	// only project the caller is authorised to bill.
	if pid, ok := ctx.Value(projectIDFromKeyKey).(string); ok && pid != "" {
		return pid, AttributionFromDBKey
	}
	// Note: the legacy X-Vornik-Project-ID header is checked
	// below ONLY when no DB-backed key resolved. Clients that
	// present BOTH get the header silently ignored; the per-
	// request deprecation surface (deprecation header + warn) is
	// applied by the caller via maybeWarnLegacyHeaderShadowed.
	// 2. Legacy header. Only checked when the request didn't auth
	// via a DB-backed key — operators on the static-keys path
	// still need a way to multiplex projects on one key.
	if r != nil {
		if pid := r.Header.Get("X-Vornik-Project-ID"); pid != "" {
			return pid, AttributionFromHeader
		}
	}
	// 3. Daemon-wide fallback.
	if fallback != "" {
		return fallback, AttributionFromFallback
	}
	// 4. Sentinel — the row never lands with an empty project
	// (which would fail the DB's NOT NULL constraint).
	return "_external", AttributionAnonymous
}

// legacyHeaderShadowedWarner rate-limits the "client supplied
// X-Vornik-Project-ID alongside a DB-backed bearer token" warn
// so a misbehaving client looping every 30s doesn't flood the
// log. Keyed by (path, user-agent) — same shape as the
// anonymous warner so two distinct clients each get their own
// first-occurrence warn.
type legacyHeaderShadowedWarner struct {
	mu      sync.Mutex
	lastFor map[string]time.Time
	cadence time.Duration
}

// maybeWarnLegacyHeaderShadowed emits a one-time (rate-limited
// per route+UA) warn AND sets a `Deprecation: true` response
// header when a client presents X-Vornik-Project-ID alongside a
// DB-backed key. The header is silently ignored on the
// attribution path (the DB key already pins the project); this
// surface tells the client to drop the header explicitly.
//
// Per-request side effects:
//   - Deprecation: true (RFC-8594 shape)
//   - Link: <docs URL>; rel="deprecation" (operator-curated;
//     omitted today, future follow-on).
//
// Rate limit: at most once every 5 minutes per (path, ua) pair.
// Same cadence the anonymous warner uses.
func (s *Server) maybeWarnLegacyHeaderShadowed(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		return
	}
	// Must have BOTH the DB-key context AND the header.
	pid, ok := r.Context().Value(projectIDFromKeyKey).(string)
	if !ok || pid == "" {
		return
	}
	header := strings.TrimSpace(r.Header.Get("X-Vornik-Project-ID"))
	if header == "" {
		return
	}
	// Stamp the Deprecation header on every shadowed request —
	// the header itself is the canonical RFC-8594 signal and
	// clients can branch on it without log scraping. Rate-
	// limiting only applies to the log warn.
	if w != nil {
		w.Header().Set("Deprecation", "true")
	}

	if s == nil {
		return
	}
	s.legacyHeaderShadowedWarnerOnce.Do(func() {
		s.legacyHeaderShadowedWarner = &legacyHeaderShadowedWarner{
			lastFor: map[string]time.Time{},
			cadence: 5 * time.Minute,
		}
	})
	ww := s.legacyHeaderShadowedWarner
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		ua = "(missing)"
	}
	key := r.URL.Path + "\x00" + ua
	ww.mu.Lock()
	last, seen := ww.lastFor[key]
	if seen && time.Since(last) < ww.cadence {
		ww.mu.Unlock()
		return
	}
	ww.lastFor[key] = time.Now()
	ww.mu.Unlock()

	s.logger.Warn().
		Str("path", r.URL.Path).
		Str("user_agent", ua).
		Str("db_key_project", pid).
		Str("header_project", header).
		Msg("client sent X-Vornik-Project-ID alongside a DB-backed API key — header is ignored (key's bound project wins). Drop the header from the client config.")
}

// anonymousAttributionWarner rate-limits the "external API call
// landed on _external" warn so a busy unkeyed client (e.g. Home
// Assistant's Ollama integration looping every 30s) doesn't flood
// the log. Keyed by (path, user-agent) so two distinct clients on
// the same daemon still each get their own first-occurrence warn.
type anonymousAttributionWarner struct {
	mu      sync.Mutex
	lastFor map[string]time.Time
	cadence time.Duration
}

// warnOnAnonymousAttribution emits a one-time warn (rate-limited
// per route+UA) when a chat-proxy / ollama-proxy call lands on
// the _external attribution bucket. The warn surfaces the gap so
// operators can diagnose "I configured a key but my project isn't
// showing up in the spend dashboard" without trawling through
// successful 200 lines. Includes the diagnostic signals an
// operator needs to decide what's wrong:
//
//   - has_authorization / has_x_api_key — was a bearer token
//     presented at all? If both are false, the client (HA's Ollama
//     integration in the canonical case) isn't sending the key.
//   - has_x_vornik_project_id — did the request supply the legacy
//     header escape hatch?
//   - user_agent — distinguishes "ollama-python" / "OpenWebUI" /
//     "curl" / "PostmanRuntime" cases at a glance.
//   - path — separates /api/chat (Ollama-shape) from
//     /api/v1/chat/completions (OpenAI-shape).
//
// Rate limit: at most once every 5 minutes per (path, ua) pair.
// First warn lands immediately; subsequent calls within the
// window are silent. The cache resets on daemon restart, so each
// restart re-emits one warn per misbehaving client — enough to
// notice in log review without being noisy.
func (s *Server) warnOnAnonymousAttribution(r *http.Request, attribution AttributionSource) {
	if attribution != AttributionAnonymous {
		return
	}
	if r == nil {
		return
	}
	s.anonAttrWarnerOnce.Do(func() {
		s.anonAttrWarner = &anonymousAttributionWarner{
			lastFor: map[string]time.Time{},
			cadence: 5 * time.Minute,
		}
	})
	w := s.anonAttrWarner
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		ua = "(missing)"
	}
	key := r.URL.Path + "\x00" + ua
	w.mu.Lock()
	last, seen := w.lastFor[key]
	if seen && time.Since(last) < w.cadence {
		w.mu.Unlock()
		return
	}
	w.lastFor[key] = time.Now()
	w.mu.Unlock()

	authHeader := r.Header.Get("Authorization")
	hasAuth := strings.TrimSpace(authHeader) != ""
	hasXAPIKey := strings.TrimSpace(r.Header.Get("X-API-Key")) != ""
	hasProjectHeader := strings.TrimSpace(r.Header.Get("X-Vornik-Project-ID")) != ""

	s.logger.Warn().
		Str("path", r.URL.Path).
		Str("user_agent", ua).
		Bool("has_authorization", hasAuth).
		Bool("has_x_api_key", hasXAPIKey).
		Bool("has_x_vornik_project_id", hasProjectHeader).
		Msg("external API call attributed to _external — no API key resolved to a project; check client auth configuration")
}
