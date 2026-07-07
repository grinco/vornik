package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// operatorReq builds a request that passes requireOperatorScope: auth
// enabled, no project scope (the single-operator / all-access path).
func operatorReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
}

func TestSkillSetGlobalHTTP_FlipsBothDirections(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	admin := skillKey("p1", true, true)
	out, _ := s.companionToolSkillPropose(ctx, admin, rawArgs(t, map[string]any{
		"name": "cli-promote", "description": "d", "body": "b", "repo_scope": "github.com/x/a",
	}))
	id := proposeID(t, out)
	if _, err := s.companionToolSkillApprove(ctx, admin, rawArgs(t, map[string]any{"id": id})); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// set-global via the CLI endpoint.
	rec := httptest.NewRecorder()
	s.SkillSetGlobal(rec, operatorReq(http.MethodPost, "/api/v1/skills/"+id+"/global", `{"global":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("set-global: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := s.skillStore.GetByID(ctx, id)
	if !got.IsGlobal || got.Maturity != persistence.SkillMaturityActive {
		t.Fatalf("endpoint must flip is_global without touching maturity: %+v", got)
	}

	// set-project (global:false).
	rec = httptest.NewRecorder()
	s.SkillSetGlobal(rec, operatorReq(http.MethodPost, "/api/v1/skills/"+id+"/global", `{"global":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("set-project: expected 200, got %d", rec.Code)
	}
	got, _ = s.skillStore.GetByID(ctx, id)
	if got.IsGlobal {
		t.Fatalf("endpoint must demote to project-only: %+v", got)
	}
}

func TestSkillSetGlobalHTTP_DeniesProjectTenant(t *testing.T) {
	s := newSkillTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/whatever/global", strings.NewReader(`{"global":true}`))
	req = req.WithContext(context.WithValue(
		ContextWithProjectScope(req.Context(), "proj-a"), authEnabledKey, true))
	s.SkillSetGlobal(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("project-scoped tenant must be denied (404), got %d", rec.Code)
	}
}

func TestSkillSetGlobalHTTP_UnknownIDIs404(t *testing.T) {
	s := newSkillTestServer(t)
	rec := httptest.NewRecorder()
	s.SkillSetGlobal(rec, operatorReq(http.MethodPost, "/api/v1/skills/no-such/global", `{"global":true}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id must 404, got %d", rec.Code)
	}
}

// TestSkillGlobalRoute_NotUnderAdminPrefix pins the CE-availability
// decision: the global-reach route must NOT be registered under
// /api/v1/admin/ (that prefix carries the EE admin-gate invariant and
// would be stripped in Community). Guards against a future refactor
// silently moving it and breaking CE.
func TestSkillGlobalRoute_NotUnderAdminPrefix(t *testing.T) {
	files := parsePackageFiles(t)
	admin := adminRouteHandlers(files)
	if admin["SkillSetGlobal"] {
		t.Fatal("SkillSetGlobal must NOT be on an /api/v1/admin/ route — it is a Community feature")
	}
}
