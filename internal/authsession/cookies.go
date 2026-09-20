package authsession

// The cookie names the browser session rides on.
//
// THEY LIVE HERE BECAUSE THERE WERE SEVEN SPELLINGS OF THEM. One constant in
// the EE login flow and six string literals in `internal/api` — the
// hand-mirrored-registry pattern this repository has already paid to remove
// from the agent tool vocabulary. A mismatch between the name a login SETS and
// the name the middleware READS is a login that silently does not
// authenticate, which is the worst kind of bug to debug because both halves
// look right in isolation.
//
// CE's credential→session exchange and EE's OIDC login now set the same
// cookies by construction, because they name them from the same place.
const (
	// SessionCookieName carries the opaque session token. HttpOnly: a
	// script in the console must not be able to read it, so an XSS cannot
	// exfiltrate the session.
	SessionCookieName = "vornik_session"
	// UIMarkerCookieName is a JS-READABLE marker that says a session
	// exists and at what role, so the nav can render "Sign out" without
	// the token being readable. It is decoration, never authority — the
	// server never trusts it.
	UIMarkerCookieName = "vornik_session_ui"
	// CapsCookieName is a JS-readable capability marker, on the same
	// terms: it changes what the nav offers, never what the server allows.
	CapsCookieName = "vornik_session_caps"
	// CSRFCookieName is the double-submit token. Readable by design —
	// the defence is that an attacker's page cannot READ it to echo it
	// back in a header, not that it is secret from the page that owns it.
	CSRFCookieName = "vornik_csrf"
	// CSRFHeaderName is where a mutating request echoes the token.
	CSRFHeaderName = "X-Vornik-CSRF"
)
