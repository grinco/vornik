// Package api: iOS home-screen icon — root-path probe tests.
//
// iOS Safari probes /apple-touch-icon.png, /apple-touch-icon-precomposed.png,
// and /favicon.ico before checking the <link> tags when adding a page to the
// home screen. These paths must:
//  1. Pass isPublicEndpoint so they aren't 401'd by AuthMiddleware.
//  2. Be served by the router (redirect 302 to the /ui/static/ equivalents)
//     without requiring auth — i.e., GET returns 3xx, not 401.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- isPublicEndpoint ---------------------------------------------------

// TestIsPublicEndpoint_IOSIconPaths asserts that the three iOS root-probe
// paths are unconditionally public so AuthMiddleware never 401s them.
func TestIsPublicEndpoint_IOSIconPaths(t *testing.T) {
	public := []string{
		"/favicon.ico",
		"/apple-touch-icon.png",
		"/apple-touch-icon-precomposed.png",
	}
	for _, path := range public {
		if !isPublicEndpoint(path) {
			t.Errorf("isPublicEndpoint(%q) = false, want true", path)
		}
	}
}

// TestIsPublicEndpoint_UnrelatedRootPathStillGated guards against an
// over-broad exemption: a non-icon root path must remain gated.
func TestIsPublicEndpoint_UnrelatedRootPathStillGated(t *testing.T) {
	gated := []string{"/secret", "/admin", "/api/v1/projects"}
	for _, path := range gated {
		if isPublicEndpoint(path) {
			t.Errorf("isPublicEndpoint(%q) = true, want false (must stay gated)", path)
		}
	}
}

// --- routing (unauthenticated redirect) ---------------------------------

// TestFaviconRoutes_RedirectWithoutAuth verifies that the three root icon
// paths respond with a 3xx redirect (to the /ui/static/ equivalent) without
// any credentials. An auth-gated 401 would break iOS home-screen icon fetch.
func TestFaviconRoutes_RedirectWithoutAuth(t *testing.T) {
	// Build a minimal router with auth ENABLED but no keys configured —
	// any path that is NOT public would get 401.
	srv := NewServer()
	router := NewRouter(srv, nil)
	h := router.Handler()

	cases := []struct {
		path   string
		target string
	}{
		{"/favicon.ico", "/ui/static/favicon.ico"},
		{"/apple-touch-icon.png", "/ui/static/apple-touch-icon.png"},
		{"/apple-touch-icon-precomposed.png", "/ui/static/apple-touch-icon.png"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code < 300 || rec.Code >= 400 {
			t.Errorf("GET %s: got status %d, want 3xx redirect", tc.path, rec.Code)
			continue
		}
		loc := rec.Header().Get("Location")
		if loc != tc.target {
			t.Errorf("GET %s: Location = %q, want %q", tc.path, loc, tc.target)
		}
	}
}
