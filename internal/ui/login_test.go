package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogin_RendersProviderButtons(t *testing.T) {
	srv := NewServer(WithLoginProviders([]string{"github"}))
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Login(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sign in with GitHub") {
		t.Errorf("login page missing GitHub button:\n%s", body)
	}
	if !strings.Contains(body, "/auth/github/start") {
		t.Errorf("login page missing /auth/github/start link")
	}
	if !strings.Contains(body, "Sign in with an API key") {
		t.Errorf("login page missing break-glass link")
	}
}

func TestLogin_NoProvidersShowsOnlyKeyPath(t *testing.T) {
	srv := NewServer() // no providers wired
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Login(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "/auth/") {
		t.Errorf("no-provider login page should not link any /auth/ start URL:\n%s", body)
	}
	if !strings.Contains(body, "Sign in with an API key") {
		t.Errorf("break-glass link must always render")
	}
	if !strings.Contains(body, "No login providers are configured") {
		t.Errorf("expected the empty-provider notice")
	}
}

func TestLogin_MethodKeyBreakGlass(t *testing.T) {
	srv := NewServer(WithLoginProviders([]string{"github"}))
	req := httptest.NewRequest(http.MethodGet, "/login?method=key", nil)
	rec := httptest.NewRecorder()
	srv.Login(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want Basic challenge", got)
	}
}

func TestLogin_AwaitingBanner(t *testing.T) {
	srv := NewServer(WithLoginProviders([]string{"github"}))
	req := httptest.NewRequest(http.MethodGet, "/login?awaiting=1&member=yes", nil)
	rec := httptest.NewRecorder()
	srv.Login(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Awaiting access") {
		t.Errorf("expected awaiting-access banner")
	}
	if !strings.Contains(body, "verified") {
		t.Errorf("expected org-membership 'verified' line for member=yes")
	}
}

func TestLogin_ErrorBanner(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/login?error=exchange", nil)
	rec := httptest.NewRecorder()
	srv.Login(rec, req)

	if !strings.Contains(rec.Body.String(), "provider rejected the sign-in") {
		t.Errorf("expected exchange error banner")
	}
}

func TestLogin_NextThreadedIntoStartURL(t *testing.T) {
	srv := NewServer(WithLoginProviders([]string{"github"}))
	req := httptest.NewRequest(http.MethodGet, "/login?next=%2Fui%2Fprojects", nil)
	rec := httptest.NewRecorder()
	srv.Login(rec, req)

	if !strings.Contains(rec.Body.String(), "next=%2Fui%2Fprojects") {
		t.Errorf("sanitized next not threaded into provider start URL:\n%s", rec.Body.String())
	}
}

func TestSanitizeLoginNext(t *testing.T) {
	cases := map[string]string{
		"":                 "/ui/",
		"/ui/projects":     "/ui/projects",
		"//evil.com":       "/ui/",
		"https://evil.com": "/ui/",
		"/\\evil.com":      "/ui/",
		"/ui/x\\y":         "/ui/",
		"relative":         "/ui/",
	}
	for in, want := range cases {
		if got := sanitizeLoginNext(in); got != want {
			t.Errorf("sanitizeLoginNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderLabel(t *testing.T) {
	cases := map[string]string{
		"github":    "GitHub",
		"gitlab":    "GitLab",
		"google":    "Google",
		"microsoft": "Microsoft",
		"okta":      "Okta",
		"":          "",
	}
	for in, want := range cases {
		if got := providerLabel(in); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogoutRouteMountedOnlyWhenWired(t *testing.T) {
	// With a logout handler wired, POST /logout reaches it.
	var hit bool
	lh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusFound)
	})
	srv := NewServer(WithLogoutHandler(lh))
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !hit {
		t.Fatal("wired logout handler was not reached")
	}

	// Without a logout handler, /logout 404s (route not mounted).
	srv2 := NewServer()
	h2 := srv2.Handler()
	req2 := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unwired /logout status = %d, want 404", rec2.Code)
	}
}

func TestHasSessionFlag(t *testing.T) {
	type withFlag struct{ IsSession bool }
	type without struct{ X int }
	if !hasSessionFlag(withFlag{IsSession: true}) {
		t.Error("struct with IsSession=true should be truthy")
	}
	if hasSessionFlag(withFlag{IsSession: false}) {
		t.Error("struct with IsSession=false should be falsey")
	}
	if hasSessionFlag(without{X: 1}) {
		t.Error("struct without IsSession should be falsey")
	}
	if hasSessionFlag(nil) {
		t.Error("nil should be falsey")
	}
	if !hasSessionFlag(map[string]any{"IsSession": true}) {
		t.Error("map with IsSession=true should be truthy")
	}
}
