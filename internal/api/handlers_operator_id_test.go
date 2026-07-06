package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/auth"
)

// TestRequestOperatorIDOrSingleTenant_SessionAuthFallsBack is the
// 2026-07-04 regression: a browser session-authenticated admin (setup
// page "create session", wizard converse) carries a real Identity but
// no api-key principal, and under auth-enabled the resolver returned ""
// → 401 "operator identity required". A non-spoofable authenticated
// identity must fall back to the server-side single-tenant operator id.
func TestRequestOperatorIDOrSingleTenant_SessionAuthFallsBack(t *testing.T) {
	newReq := func(authEnabled bool, id *auth.Identity) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/setup/session", nil)
		ctx := context.WithValue(r.Context(), authEnabledKey, authEnabled)
		if id != nil {
			ctx = context.WithValue(ctx, identityKey, id)
		}
		return r.WithContext(ctx)
	}
	sessionID := &auth.Identity{Backend: "session"}

	// Auth enabled + authenticated session + config fallback set → fallback.
	if got := RequestOperatorIDOrSingleTenant(newReq(true, sessionID), "vadim"); got != "vadim" {
		t.Errorf("session-auth with fallback: got %q, want %q", got, "vadim")
	}
	// Auth enabled + authenticated session + no config fallback → default.
	if got := RequestOperatorIDOrSingleTenant(newReq(true, sessionID), ""); got != defaultSingleTenantOperatorID {
		t.Errorf("session-auth no fallback: got %q, want %q", got, defaultSingleTenantOperatorID)
	}
	// Auth enabled + NO identity (unauthenticated) → "" (still refused).
	if got := RequestOperatorIDOrSingleTenant(newReq(true, nil), "vadim"); got != "" {
		t.Errorf("unauthenticated under auth: got %q, want empty", got)
	}
	// Auth disabled → fallback (unchanged behaviour).
	if got := RequestOperatorIDOrSingleTenant(newReq(false, nil), "vadim"); got != "vadim" {
		t.Errorf("auth disabled: got %q, want %q", got, "vadim")
	}
}
