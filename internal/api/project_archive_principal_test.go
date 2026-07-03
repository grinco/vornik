// Regression test for S4 (audit 2026-07-03): apiArchivePrincipal must not let
// the caller-controlled X-Operator-Id header set the audit-row principal when
// auth is enabled. The header is honored only in the auth-disabled single-
// tenant mode.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIArchivePrincipal_HeaderIgnoredWhenAuthEnabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/archive", nil)
	req.Header.Set("X-Operator-Id", "spoofed-operator")
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))

	if got := apiArchivePrincipal(req); got != "" {
		t.Fatalf("auth enabled: X-Operator-Id header must be ignored, got %q", got)
	}
}

func TestAPIArchivePrincipal_HeaderHonoredWhenAuthDisabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/archive", nil)
	req.Header.Set("X-Operator-Id", "local-operator")
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))

	if got := apiArchivePrincipal(req); got != "local-operator" {
		t.Fatalf("auth disabled: X-Operator-Id header should set principal, got %q", got)
	}
}
