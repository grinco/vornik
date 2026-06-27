package ui

import (
	"net/http"
	"net/url"
	"strings"
)

// loginProviderView is one render-ready provider button. Label is a
// human title-cased name; StartURL points at the top-level auth mux
// (mounted outside the /ui subtree) carrying the sanitized next path.
type loginProviderView struct {
	Name     string
	Label    string
	StartURL string
}

// loginPageData is the template payload for login.html.
type loginPageData struct {
	// Title feeds pageHead's <title>; the login page is standalone
	// (no nav) but reuses pageHead for consistent styling.
	Title       string
	CurrentPage string
	Providers   []loginProviderView
	// Awaiting is true when the user authenticated but has no project
	// access yet (zero groups) — the page shows the awaiting-access
	// banner instead of (or alongside) the provider buttons.
	Awaiting bool
	// Member reflects the org-membership signal surfaced on the
	// awaiting-access banner: "yes" | "no" | "unknown".
	Member string
	// Error is a coarse error code (e.g. "exchange", "state") used to
	// pick a banner message. Detail stays in the server logs — no
	// oracle for an attacker.
	Error string
	// Next is the sanitized post-login redirect target, threaded
	// through provider start URLs so the user lands where they meant.
	Next string
}

// Login renders the browser login page. It is mounted at /ui/login,
// which the auth middleware treats as a public endpoint so the page
// works unauthenticated.
//
// Break-glass (?method=key): the design keeps API-key access to the
// UI working alongside OIDC. The login page links here with
// method=key; this handler answers 401 + WWW-Authenticate: Basic so
// the browser pops its native credential dialog (the exact flow that
// worked before login existed). Producing the 401 from the handler
// — rather than special-casing it in the middleware — keeps the
// middleware's /ui/login exemption unconditional.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("method") == "key" {
		// Same challenge respondUnauthorized emits on the API side so
		// the browser shows the credential prompt; the user types any
		// username + their API-key bearer as the password.
		w.Header().Set("WWW-Authenticate", `Basic realm="vornik"`)
		http.Error(w, "API key authentication required", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()
	next := sanitizeLoginNext(q.Get("next"))

	data := loginPageData{
		Title:       "Sign in",
		CurrentPage: "login",
		Awaiting:    q.Get("awaiting") == "1",
		Member:      normalizeMember(q.Get("member")),
		Error:       q.Get("error"),
		Next:        next,
	}
	for _, name := range s.loginProviders {
		data.Providers = append(data.Providers, loginProviderView{
			Name:     name,
			Label:    providerLabel(name),
			StartURL: providerStartURL(name, next),
		})
	}

	s.render(w, "login.html", data)
}

// sanitizeLoginNext mirrors the loginflow next-sanitizer: a usable
// post-login redirect must be a same-origin absolute path. Anything
// that could become an open redirect (scheme-relative "//host",
// absolute URL, backslash trick, or empty) collapses to "/ui/".
func sanitizeLoginNext(next string) string {
	if next == "" {
		return "/ui/"
	}
	if !strings.HasPrefix(next, "/") {
		return "/ui/"
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/ui/"
	}
	if strings.Contains(next, "\\") {
		return "/ui/"
	}
	return next
}

// providerStartURL builds the /auth/<name>/start link. The auth mux
// is mounted at the top level (NOT under /ui), so the link is
// absolute from the site root.
func providerStartURL(name, next string) string {
	u := "/auth/" + name + "/start"
	if next != "" && next != "/ui/" {
		u += "?next=" + url.QueryEscape(next)
	}
	return u
}

// providerLabel title-cases a provider name for the button label,
// special-casing the known providers so "github" → "GitHub".
func providerLabel(name string) string {
	switch name {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "google":
		return "Google"
	case "microsoft":
		return "Microsoft"
	default:
		if name == "" {
			return ""
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

// normalizeMember coerces the org-membership query value to one of
// the three banner states; anything unexpected reads as "unknown".
func normalizeMember(m string) string {
	switch m {
	case "yes", "no":
		return m
	default:
		return "unknown"
	}
}
