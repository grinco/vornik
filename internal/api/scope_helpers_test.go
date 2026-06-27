package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
)

// requestAllowsProject and requestAllowsOperator are the two
// scope primitives every visibility-filter call site reaches for.
// They both bake in the auth-disabled bypass so a future caller
// can't reintroduce the "filter drops every row when auth is off"
// bug class. These tests pin that behaviour at the primitive level.

func TestRequestAllowsProject_AuthOffAdmitsEverything(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	if !requestAllowsProject(req, "any-project") {
		t.Fatal("auth-off should admit any project")
	}
}

func TestRequestAllowsProject_AuthOffIgnoresStaleScope(t *testing.T) {
	// Belt-and-suspenders: even if a buggy middleware stamped a
	// scope list on an auth-off request, the bypass takes
	// precedence — auth-off means no enforcement.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, false)
	ctx = context.WithValue(ctx, projectIDKey, []string{"only-this"})
	req = req.WithContext(ctx)
	if !requestAllowsProject(req, "different") {
		t.Fatal("auth-off bypass should beat any scope list")
	}
}

func TestRequestAllowsProject_AuthOnWithoutScopeAdmits(t *testing.T) {
	// Admin-class keys (no project scope attached) admit any
	// project — same behaviour the codebase had before this
	// refactor, just made testable.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	if !requestAllowsProject(req, "any") {
		t.Fatal("auth-on without scope list should admit")
	}
}

func TestRequestAllowsProject_AuthOnScopedFiltersMismatch(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, []string{"a", "b"})
	req = req.WithContext(ctx)
	if !requestAllowsProject(req, "a") {
		t.Fatal("scoped key should admit listed project")
	}
	if requestAllowsProject(req, "c") {
		t.Fatal("scoped key admitted unlisted project")
	}
}

func TestRequestAllowsProject_EmptyProjectIDRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if requestAllowsProject(req, "") {
		t.Fatal("empty projectID must not pass scope check")
	}
}

func TestRequestAllowsOperator_AuthOffAdmitsEverything(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	if !requestAllowsOperator(req, "telegram:42") {
		t.Fatal("auth-off should admit any operator")
	}
	// Even global rows (empty operatorID) are admitted under
	// auth-off — single-tenant deployments see everything.
	if !requestAllowsOperator(req, "") {
		t.Fatal("auth-off should admit global (empty-operator) rows")
	}
}

func TestRequestAllowsOperator_AuthOnMatchingPrincipal(t *testing.T) {
	// Matched DB-backed key → principal is "api_key_id:<row-id>".
	// requestOperatorID returns that exact string, so the
	// expected operatorID on the row is the same.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-test")
	ctx = context.WithValue(ctx, apiKeyIDKey, "key_42")
	req = req.WithContext(ctx)
	if !requestAllowsOperator(req, "api_key_id:key_42") {
		t.Fatal("matching principal should be admitted")
	}
}

func TestRequestAllowsOperator_AuthOnMismatchRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-test")
	ctx = context.WithValue(ctx, apiKeyIDKey, "key_42")
	req = req.WithContext(ctx)
	if requestAllowsOperator(req, "telegram:99") {
		t.Fatal("mismatched principal admitted")
	}
}

func TestRequestAllowsOperator_AuthOnNoPrincipalRejected(t *testing.T) {
	// Auth on + no principal stamped on context (anonymous)
	// must not admit any operator's rows — that's the
	// fail-closed safety property the old code already had.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	if requestAllowsOperator(req, "telegram:42") {
		t.Fatal("anonymous caller must not see arbitrary operator rows")
	}
}

func TestRequestAllowsOperator_AuthOnEmptyOperatorIDRejected(t *testing.T) {
	// Global rows (operatorID="") under auth-on belong to
	// admin-class callers — the per-handler admin check runs
	// before this and short-circuits. Without that path,
	// requestAllowsOperator must refuse.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-test")
	ctx = context.WithValue(ctx, apiKeyIDKey, "key_42")
	req = req.WithContext(ctx)
	if requestAllowsOperator(req, "") {
		t.Fatal("non-admin caller must not see global rows via the operator helper")
	}
}

// RequestOperatorIDOrSingleTenant fills in a non-empty operator for
// handlers that need one to function. The fail-closed property
// (auth-on without principal → "") must hold so admin/operator
// gates downstream stay safe.

func TestRequestOperatorIDOrSingleTenant_AuthOffUsesFallback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	got := RequestOperatorIDOrSingleTenant(req, "local:dev")
	if got != "local:dev" {
		t.Fatalf("auth-off + no principal: got %q, want %q", got, "local:dev")
	}
}

func TestRequestOperatorIDOrSingleTenant_AuthOffPrefersHeader(t *testing.T) {
	// An operator who self-identifies under auth-off should
	// still win over the daemon-wide fallback — same precedence
	// requestOperatorID enforces. The fallback only fills in
	// when no signal is present.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Operator-Id", "telegram:42")
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	if got := RequestOperatorIDOrSingleTenant(req, "local:dev"); got != "telegram:42" {
		t.Fatalf("header should beat fallback: got %q", got)
	}
}

func TestRequestOperatorIDOrSingleTenant_AuthOffEmptyFallbackUsesDefault(t *testing.T) {
	// Pass-through helper for callers that don't carry a config
	// at all: the default `local:dev` stamps in.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	if got := RequestOperatorIDOrSingleTenant(req, ""); got != defaultSingleTenantOperatorID {
		t.Fatalf("empty fallback should default: got %q, want %q", got, defaultSingleTenantOperatorID)
	}
}

func TestRequestOperatorIDOrSingleTenant_AuthOnAnonymousReturnsEmpty(t *testing.T) {
	// Critical fail-closed property — under auth-on, an
	// anonymous caller must NOT get the fallback identity.
	// That would let any unauthenticated request impersonate
	// `local:dev` and write rows under that operator.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	if got := RequestOperatorIDOrSingleTenant(req, "local:dev"); got != "" {
		t.Fatalf("auth-on anonymous must NOT get fallback: got %q", got)
	}
}

func TestRequestOperatorIDOrSingleTenant_AuthOnPrincipalWins(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-test")
	ctx = context.WithValue(ctx, apiKeyIDKey, "key_42")
	req = req.WithContext(ctx)
	if got := RequestOperatorIDOrSingleTenant(req, "local:dev"); got != "api_key_id:key_42" {
		t.Fatalf("verified principal should win over fallback: got %q", got)
	}
}

func TestSingleTenantOperatorIDFromConfig(t *testing.T) {
	if got := SingleTenantOperatorIDFromConfig(nil); got != defaultSingleTenantOperatorID {
		t.Fatalf("nil config should yield default; got %q", got)
	}
	if got := SingleTenantOperatorIDFromConfig(&config.Config{}); got != defaultSingleTenantOperatorID {
		t.Fatalf("empty config should yield default; got %q", got)
	}
	cfg := &config.Config{}
	cfg.API.SingleTenantOperatorID = "  tenant-a:root  "
	if got := SingleTenantOperatorIDFromConfig(cfg); got != "tenant-a:root" {
		t.Fatalf("configured value should win + be trimmed; got %q", got)
	}
}
