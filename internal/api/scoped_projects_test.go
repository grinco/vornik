package api

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestRequestScopedProjects(t *testing.T) {
	// Auth off → single-tenant, never scoped.
	rOff := httptest.NewRequest("GET", "/", nil)
	rOff = rOff.WithContext(context.WithValue(rOff.Context(), authEnabledKey, false))
	if _, ok := RequestScopedProjects(rOff); ok {
		t.Error("auth-off request must be unscoped")
	}

	// Auth on + explicit projects → scoped, sorted.
	rScoped := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(context.Background(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, []string{"snake", "janka"})
	rScoped = rScoped.WithContext(ctx)
	got, ok := RequestScopedProjects(rScoped)
	if !ok || len(got) != 2 || got[0] != "janka" || got[1] != "snake" {
		t.Fatalf("scoped: got %v ok=%v, want [janka snake] true", got, ok)
	}

	// Auth on + empty allowlist → legacy all-access (unscoped).
	rAll := httptest.NewRequest("GET", "/", nil)
	rAll = rAll.WithContext(context.WithValue(context.Background(), authEnabledKey, true))
	if _, ok := RequestScopedProjects(rAll); ok {
		t.Error("auth-on with no project list must be unscoped (all-access)")
	}
}
