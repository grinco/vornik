package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression: review of a799e3f2 (2026-06-07) — memory per-project routes
// lacked the scope gate the sibling surfaces have. HEAD added the scope
// filter only to the memory LIST page (memory.go:59); every per-project
// handler behind memoryRouter (MemoryProject, MemorySearchAction, the KG
// routes, etc.) was reachable by a scoped browser user for ANY project id,
// leaking another tenant's memory. The fix gates once at the router choke
// point. These tests fail before that gate exists and pass after.

// servePath runs req through the full UI mux so the memoryRouter scope
// gate (not just the leaf handler) is exercised. The mux dispatches on the
// un-prefixed path (the /ui prefix is stripped by an outer handler), so
// tests target /memory/... directly.
func servePath(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// scopedMemoryRig builds a server with the project registry plus a memory
// searcher wired, so the leaf handlers would otherwise render the
// requested project — the only thing that may stop a cross-project read is
// the router scope gate under test.
func scopedMemoryRig(t *testing.T) *Server {
	t.Helper()
	base := projectsRig(t)
	return NewServer(
		WithProjectRegistry(base.projectReg),
		WithMemorySearcher(&stubMemorySearcher{}),
	)
}

// TestMemoryRouter_ScopedUserDeniedCrossProject pins the gate on the
// per-project memory surfaces. A session user allowed only "alpha" must
// not reach "beta" via the detail page, the search action, or the
// knowledge-graph entities route.
func TestMemoryRouter_ScopedUserDeniedCrossProject(t *testing.T) {
	// Regression: review of a799e3f2 (2026-06-07) — memory per-project
	// routes lacked the scope gate the sibling surfaces have.
	srv := scopedMemoryRig(t)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"detail", "/memory/beta"},
		{"search", "/memory/beta/search?q=secrets"},
		{"kg-entities", "/memory/beta/entities"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sessionUserUIRequest(http.MethodGet, tc.path, []string{"alpha"})
			rec := servePath(srv, req)
			require.Equalf(t, http.StatusNotFound, rec.Code,
				"scoped user must be denied %s (got %d, body=%s)",
				tc.path, rec.Code, rec.Body.String())
		})
	}
}

// TestMemoryRouter_ScopedUserAllowedOwnProject confirms the gate does not
// over-block: a session user allowed "alpha" still reaches alpha's search
// action (200). Guards against the fix denying the legitimate path.
func TestMemoryRouter_ScopedUserAllowedOwnProject(t *testing.T) {
	// Regression: review of a799e3f2 (2026-06-07) — memory per-project
	// routes lacked the scope gate the sibling surfaces have.
	srv := scopedMemoryRig(t)
	req := sessionUserUIRequest(http.MethodGet, "/memory/alpha/search?q=hello", []string{"alpha"})
	rec := servePath(srv, req)
	require.Equalf(t, http.StatusOK, rec.Code,
		"scoped user must still reach own project search (got %d, body=%s)",
		rec.Code, rec.Body.String())
}
