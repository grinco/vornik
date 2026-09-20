package api

// POST /api/v1/auth/session — the CE credential→browser-session exchange.
//
// 2026-09-19-ce-human-login-design.md §4, §6. It turns "this caller holds a
// credential" into "this browser is acting as this account, for a bounded
// time, revocably" — which is the thing CE lacked, rather than lacking a
// login page.
//
// IT DOES NOT VERIFY THE KEY. The request has already been through the auth
// chain, and this handler reads the identity the chain produced. Writing a
// second verifier here is how this repository produced a gate with two
// implementations, one of them wrong, three times — and a login door is the
// worst place to be the fourth.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/authz"
)

// sessionExchangeMinter mints a browser session for an account.
type sessionExchangeMinter interface {
	Create(ctx context.Context, userID, provider, ip, userAgent, originCredentialID string) (string, error)
}

// exchangeSubjectResolver answers which account a credential may open a
// session as. *authz.Accounts satisfies it.
//
// A NAMED FIELD rather than a reach into s.accounts, so this route's
// dependencies are exactly what WithSessionExchange wires. A handler that
// quietly depends on a field some other option happens to set is a handler
// whose wiring you discover by reading it — and one that cannot be tested
// without constructing the whole account service.
type exchangeSubjectResolver interface {
	ResolveExchangeSubject(ctx context.Context, keyID string) (*authz.ExchangeSubject, error)
}

// SessionExchange exchanges an authenticated API key for a session cookie.
func (s *Server) SessionExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
		return
	}
	if s.sessionSubjects == nil || s.sessionMinter == nil {
		// Not an oracle: a deployment either offers this door or does
		// not, and that is not a fact about any credential.
		respondError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE",
			"session exchange is not wired on this deployment")
		return
	}

	// PLAINTEXT REFUSAL. A session cookie travelling in the clear is the
	// whole session, and an operator who wants this on HTTP must say so
	// knowingly rather than discover it.
	if !requestIsSecure(r) && !s.insecureSessionsAllowed {
		respondError(w, http.StatusBadRequest, "INSECURE_TRANSPORT",
			"refusing to mint a session cookie over plaintext HTTP; terminate TLS in front of the daemon, "+
				"or set the development override if this is a local deployment")
		return
	}
	if !originAllowed(r) {
		// Generic: an origin failure must not read differently from a
		// bad credential, or a prober learns which half it got wrong.
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", sessionExchangeRefusal)
		return
	}

	// A session may not mint a session. The door exists to turn a MACHINE
	// credential into a browser one; a caller that already has a browser
	// session gains nothing, and allowing it would make session lifetime
	// renewable without ever re-presenting the credential — which is the
	// cap in §5 quietly removed.
	keyID := APIKeyIDFromContext(r.Context())
	if keyID == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", sessionExchangeRefusal)
		return
	}

	subject, err := s.sessionSubjects.ResolveExchangeSubject(r.Context(), keyID)
	switch {
	case errors.Is(err, authz.ErrExchangeRateLimited):
		respondError(w, http.StatusTooManyRequests, "RATE_LIMITED",
			"too many session exchange attempts for this credential")
		return
	case err != nil:
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", sessionExchangeRefusal)
		return
	}

	raw, err := s.sessionMinter.Create(r.Context(), subject.UserID, "credential",
		clientIPFromRequest(r), r.UserAgent(), keyID)
	if err != nil {
		s.logger.Error().Err(err).Str("user", subject.UserID).Msg("session exchange: mint failed")
		respondError(w, http.StatusInternalServerError, "DB_ERROR", "failed to mint session")
		return
	}

	csrf, err := newCSRFToken()
	if err != nil {
		s.logger.Error().Err(err).Msg("session exchange: csrf entropy failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", "failed to mint session")
		return
	}

	secure := requestIsSecure(r)
	maxAge := int(s.sessionLifetime.Seconds())
	if maxAge <= 0 {
		maxAge = int((12 * time.Hour).Seconds())
	}

	http.SetCookie(w, &http.Cookie{
		Name: authsession.SessionCookieName, Value: raw, Path: "/", MaxAge: maxAge,
		// HttpOnly: an XSS in the console must not be able to read the
		// session. SameSite=Lax stops cross-site POSTs; the CSRF token
		// below is what keeps that true for flows Lax does not cover.
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: authsession.CSRFCookieName, Value: csrf, Path: "/", MaxAge: maxAge,
		// READABLE by design: the defence is that an attacker's page
		// cannot read it to echo back in a header, not that it is
		// secret from the page that owns it.
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: authsession.UIMarkerCookieName, Value: navMarker(subject.Role), Path: "/", MaxAge: maxAge,
		// Decoration, never authority — the server re-resolves the real
		// principal on every request.
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode,
	})

	w.WriteHeader(http.StatusNoContent)
}

// sessionExchangeRefusal is the ONE message every refusal carries.
//
// The exchange is a key-verification oracle by construction. What is avoidable
// is a second signal: distinguishing "bad key" from "bad origin" from
// "unmapped account" would tell a prober which half it got wrong, and the
// differences are facts about someone else's account.
const sessionExchangeRefusal = "could not exchange this credential for a session"

// Values of the JS-readable nav marker. They are a CONTRACT with the
// console's markup, not free strings: the EE login flow sets the same two, and
// the nav renders "Sign out" on either and the Admin link only on the first.
const (
	navMarkerAdmin = "admin"
	navMarkerUser  = "1"
)

// navMarker is the JS-readable role hint the nav reads. Decoration, never
// authority — the server re-resolves the real principal on every request.
func navMarker(role string) string {
	if role == authz.RoleAdmin {
		return navMarkerAdmin
	}
	return navMarkerUser
}

// requestIsSecure reports whether the request reached the daemon over TLS,
// directly or through a proxy that said so.
//
// The proxy header is trusted only because the daemon is not directly exposed
// in any supported topology — and that assumption is stated here rather than
// assumed silently, because it is the one an operator could break by putting
// the daemon on a public interface.
func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// originAllowed checks the Origin/Referer against the request's own host.
//
// Same-origin only: this door is used by the console on the same deployment.
// A request with NO Origin and no Referer is allowed, because non-browser
// callers (curl, the CLI) legitimately send neither — and the cross-site
// attack this stops is by definition made by a browser, which always sends
// one on a cross-origin POST.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// newCSRFToken mints a 256-bit double-submit token.
func newCSRFToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// WithSessionExchange wires the CE credential→session exchange.
//
// All three together or none: a minter with no lifetime would mint sessions
// that never expire, and the route reports 503 until the minter is present, so
// a half-wired deployment refuses rather than mints something unbounded.
func WithSessionExchange(subjects exchangeSubjectResolver, minter sessionExchangeMinter, lifetime time.Duration, allowInsecure bool) ServerOption {
	return func(s *Server) {
		s.sessionSubjects = subjects
		s.sessionMinter = minter
		s.sessionLifetime = lifetime
		s.insecureSessionsAllowed = allowInsecure
	}
}
