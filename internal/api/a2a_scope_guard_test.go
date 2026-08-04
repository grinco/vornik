package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// The A2A inbound path (/a2a/v1/agents/<project>/<workflow>/...) created
// tasks in the URL's project WITHOUT checking the caller key's scope —
// any valid key could invoke any published workflow (authz bypass,
// a2a-expert-federation-design §4). a2aScopeGuard closes it by enforcing
// the same project-scope convention the rest of /api uses, plus the
// key's AllowedWorkflows allowlist.

func passThroughOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("reached-handler"))
}

func TestA2AScopeGuard_CrossProjectKey_Forbidden(t *testing.T) {
	guard := a2aScopeGuard(passThroughOK)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/companion-example/product-qa/tasks", nil)
	// Key bound to a DIFFERENT project than the one in the path.
	req = req.WithContext(ContextWithProjectScope(req.Context(), "janka"))
	rec := httptest.NewRecorder()

	guard(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project key: status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "reached-handler" {
		t.Fatal("cross-project key reached the handler — task would have been created")
	}
}

func TestA2AScopeGuard_MatchingProject_PassesThrough(t *testing.T) {
	guard := a2aScopeGuard(passThroughOK)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/companion-example/product-qa/tasks", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "companion-example"))
	rec := httptest.NewRecorder()

	guard(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "reached-handler" {
		t.Fatalf("matching-project key: status=%d body=%q, want 200/reached-handler", rec.Code, rec.Body.String())
	}
}

func TestA2AScopeGuard_AuthOff_PassesThrough(t *testing.T) {
	guard := a2aScopeGuard(passThroughOK)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/companion-example/product-qa/tasks", nil)
	// No scope stamped → auth-off / single-tenant → allowed.
	rec := httptest.NewRecorder()

	guard(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "reached-handler" {
		t.Fatalf("auth-off: status=%d body=%q, want 200/reached-handler", rec.Code, rec.Body.String())
	}
}

func TestA2AScopeGuard_WorkflowNotInKeyAllowlist_Forbidden(t *testing.T) {
	guard := a2aScopeGuard(passThroughOK)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/companion-example/product-qa/tasks", nil)
	// Key IS scoped to the project, but its AllowedWorkflows excludes product-qa.
	ctx := ContextWithProjectScope(req.Context(), "companion-example")
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{
		BoundProjectID: "companion-example",
		Extra: map[string]any{
			auth.ExtraDBKeyRow: &persistence.APIKey{
				ProjectID:        "companion-example",
				AllowedWorkflows: []string{"some-other-workflow"},
			},
		},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	guard(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("workflow-not-allowed: status=%d, want 403; body=%q", rec.Code, rec.Body.String())
	}
}

func TestA2AScopeGuard_WorkflowInKeyAllowlist_PassesThrough(t *testing.T) {
	guard := a2aScopeGuard(passThroughOK)
	req := httptest.NewRequest(http.MethodPost, "/a2a/v1/agents/companion-example/product-qa/tasks", nil)
	ctx := ContextWithProjectScope(req.Context(), "companion-example")
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{
		BoundProjectID: "companion-example",
		Extra: map[string]any{
			auth.ExtraDBKeyRow: &persistence.APIKey{
				ProjectID:        "companion-example",
				AllowedWorkflows: []string{"product-qa"},
			},
		},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	guard(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "reached-handler" {
		t.Fatalf("workflow-allowed: status=%d body=%q, want 200/reached-handler", rec.Code, rec.Body.String())
	}
}
