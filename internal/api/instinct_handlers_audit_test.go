package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// instinctAuditScopedReq stamps auth-enabled + a project-scope list so the
// per-project scope guards on the instinct handlers engage. Helper name is
// prefixed to avoid collisions with sibling audit-test files.
func instinctAuditScopedReq(r *http.Request, projects ...string) *http.Request {
	ctx := context.WithValue(r.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, projects)
	return r.WithContext(ctx)
}

// TestListInstincts_ScopedHidesOtherProjects asserts that a caller scoped
// to project "beta" cannot see project-scoped instincts owned by "alpha",
// but still sees global-scope advisory rows. Pre-fix this returned all
// rows regardless of scope.
func TestListInstincts_ScopedHidesOtherProjects(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()} // ins_1/ins_2 = alpha, ins_3 = global
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts", nil)
	req = instinctAuditScopedReq(req, "beta")
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	var got InstinctListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, ins := range got.Instincts {
		if ins.Scope == persistence.InstinctScopeProject && ins.ProjectID == "alpha" {
			t.Fatalf("scoped caller (beta) leaked alpha project instinct %s", ins.ID)
		}
	}
	// Global advisory row must remain visible.
	sawGlobal := false
	for _, ins := range got.Instincts {
		if ins.ID == "ins_3" {
			sawGlobal = true
		}
	}
	if !sawGlobal {
		t.Fatalf("global-scope instinct ins_3 should stay visible to scoped callers")
	}
}

// TestListInstincts_ScopedSeesOwnProject confirms the in-scope project's
// rows are still returned (no over-filtering regression).
func TestListInstincts_ScopedSeesOwnProject(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts", nil)
	req = instinctAuditScopedReq(req, "alpha")
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	var got InstinctListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sawAlpha := false
	for _, ins := range got.Instincts {
		if ins.ProjectID == "alpha" {
			sawAlpha = true
		}
	}
	if !sawAlpha {
		t.Fatalf("alpha-scoped caller should still see its own project instincts")
	}
}

// TestListInstincts_ScopedRejectsForeignProjectFilter asserts that an
// explicit ?project= outside the caller's scope is rejected with 403.
func TestListInstincts_ScopedRejectsForeignProjectFilter(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts?project=alpha", nil)
	req = instinctAuditScopedReq(req, "beta")
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign ?project= = %d, want 403", rec.Code)
	}
}

// TestListInstincts_UnscopedUnchanged confirms auth-off (single-tenant)
// behaviour is preserved: every row is returned.
func TestListInstincts_UnscopedUnchanged(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts", nil)
	rec := httptest.NewRecorder()
	s.ListInstincts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	var got InstinctListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Instincts) != len(sampleInstincts()) {
		t.Fatalf("auth-off list returned %d rows, want %d", len(got.Instincts), len(sampleInstincts()))
	}
}

// TestShowInstinct_ScopedForeignProject404 asserts a scoped caller gets 404
// when fetching another project's project-scoped instinct by id. Pre-fix
// the row was returned verbatim (IDOR).
func TestShowInstinct_ScopedForeignProject404(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_1", nil)
	req = instinctAuditScopedReq(req, "beta")
	rec := httptest.NewRecorder()
	s.ShowInstinct(rec, req, "ins_1") // ins_1 is project=alpha

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project show = %d, want 404", rec.Code)
	}
}

// TestShowInstinct_ScopedGlobalVisible confirms global-scope rows stay
// readable by scoped callers.
func TestShowInstinct_ScopedGlobalVisible(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_3", nil)
	req = instinctAuditScopedReq(req, "beta")
	rec := httptest.NewRecorder()
	s.ShowInstinct(rec, req, "ins_3") // ins_3 is global

	if rec.Code != http.StatusOK {
		t.Fatalf("global show = %d, want 200", rec.Code)
	}
}

// TestShowInstinct_ScopedOwnProjectVisible confirms in-scope project rows
// are still served (no over-restriction regression).
func TestShowInstinct_ScopedOwnProjectVisible(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instincts/ins_1", nil)
	req = instinctAuditScopedReq(req, "alpha")
	rec := httptest.NewRecorder()
	s.ShowInstinct(rec, req, "ins_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("own project show = %d, want 200", rec.Code)
	}
}

// TestRetireInstinct_ScopedForeignProjectBlocked asserts a scoped caller
// cannot retire another project's project-scoped instinct, and that Retire
// is never invoked on the repo. Pre-fix Retire ran unconditionally.
func TestRetireInstinct_ScopedForeignProjectBlocked(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instincts/ins_1/retire", nil)
	req = instinctAuditScopedReq(req, "beta")
	rec := httptest.NewRecorder()
	s.RetireInstinct(rec, req, "ins_1") // ins_1 is project=alpha

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign project retire = %d, want 404", rec.Code)
	}
	if repo.retireCalls != 0 {
		t.Fatalf("Retire was called %d times on an out-of-scope row, want 0", repo.retireCalls)
	}
}

// TestRetireInstinct_ScopedOwnProjectAllowed confirms an in-scope caller
// can still retire its own project's instinct.
func TestRetireInstinct_ScopedOwnProjectAllowed(t *testing.T) {
	repo := &stubInstinctRepo{rows: sampleInstincts()}
	s := instinctTestServer(repo, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/instincts/ins_1/retire", nil)
	req = instinctAuditScopedReq(req, "alpha")
	rec := httptest.NewRecorder()
	s.RetireInstinct(rec, req, "ins_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("own project retire = %d, want 200", rec.Code)
	}
	if repo.retireCalls != 1 {
		t.Fatalf("Retire calls = %d, want 1", repo.retireCalls)
	}
}
