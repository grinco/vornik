package api

// Double-submit CSRF validation for cookie-authenticated mutations —
// 2026-09-19-ce-human-login-design.md §6, §6.1.
//
// THIS IS NOT A SECOND CSRF GATE. `isCSRFSafe` already refuses a mutating
// cookie-authenticated request with no same-origin signal, and adding a
// parallel middleware for one threat is how this repository produced a
// security predicate with two implementations, one of them wrong, three times.
// So this is a second CONDITION inside the one gate, and it runs only after
// the ladder has already passed.
//
// WHAT IT ADDS, precisely — because "defence in depth" is the phrase people
// reach for when they cannot name the gap. The ladder accepts
// `Sec-Fetch-Site: same-site`, which is TRUE FOR A SIBLING SUBDOMAIN: a daemon
// on `vornik.example.com` and an XSS on `blog.example.com` are same-site and
// different origins, so the ladder passes a request the victim never made. The
// double-submit token closes that, because the attacking origin cannot READ
// the `vornik_csrf` cookie — it is host-only, set without a Domain attribute,
// so it is not visible to a sibling host at all.
//
// It adds nothing against a cross-SITE attacker; the ladder already refuses
// those. Stated so nobody later "simplifies" one into the other.

import (
	"crypto/subtle"
	"net/http"

	"vornik.io/vornik/internal/authsession"
)

// csrfDoubleSubmitOK reports whether a mutating cookie-authenticated request
// carries a header token matching its cookie.
//
// ABSENT COOKIE MEANS SKIP, and that is a deliberate compatibility seam rather
// than a hole an attacker can open. The attacker does not choose which cookies
// the victim's browser sends: if the victim holds a `vornik_csrf` cookie it is
// attached, and the attacker must then produce a matching header they cannot
// read. A victim with NO such cookie is one whose session predates this
// mechanism, and they are protected by the ladder exactly as they were before.
// Those sessions age out with their lifetime.
func csrfDoubleSubmitOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	cookie, err := r.Cookie(authsession.CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return true
	}
	sent := r.Header.Get(authsession.CSRFHeaderName)
	if sent == "" {
		return false
	}
	// Constant-time: the comparison is against a secret the caller is trying
	// to guess, and a length-or-prefix leak is the kind of thing that is
	// free to avoid and awkward to explain afterwards.
	return subtle.ConstantTimeCompare([]byte(sent), []byte(cookie.Value)) == 1
}
