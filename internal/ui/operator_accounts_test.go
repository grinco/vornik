package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

func accountsServer(repo *stubUsersIdentityRepo, audit *stubAdminAuditRepo) *Server {
	return NewServer(
		WithAccountsService(authz.NewAccounts(repo, audit)),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}),
		WithProjectRegistry(registry.New()),
	)
}

func withAuthOff(r *http.Request) *http.Request {
	return r.WithContext(api.ContextWithAuthEnabled(r.Context(), false))
}

func TestOperatorAccounts_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not wired") {
		t.Fatalf("unwired page: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/u/disable", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired POST: want 503, got %d", rec.Code)
	}
}

// Review R2: the CE shell refuses a caller without an explicit capability
// even though the key is unrestricted; the break-glass key passes.
func TestOperatorAccounts_CapabilityGate(t *testing.T) {
	s := accountsServer(&stubUsersIdentityRepo{users: twoUserFixture()}, &stubAdminAuditRepo{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)
	req = req.WithContext(api.ContextWithAPIKeyForTesting(api.ContextWithAuthEnabled(req.Context(), true), "sk-plain"))
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain key: want 403, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)
	req = req.WithContext(api.ContextWithAPIKeyForTesting(api.ContextWithAuthEnabled(req.Context(), true), "sk-operator"))
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Vadim") {
		t.Fatalf("break-glass key: want 200 with the accounts, got %d", rec.Code)
	}
}

func TestOperatorAccounts_GrantAndRevokeAudited(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	audit := &stubAdminAuditRepo{}
	s := accountsServer(repo, audit)

	form := strings.NewReader("role=user&projects=proj-a")
	req := withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/user_await/grant", form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "access+updated") {
		t.Fatalf("grant: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "account.grant" || audit.rows[0].Source != "ui" {
		t.Fatalf("audit rows: %+v", audit.rows)
	}

	// A user-role grant with no project is refused before the repository.
	req = withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/user_await/grant", strings.NewReader("role=user")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "pick+at+least+one") {
		t.Fatalf("empty projects must be refused: %s", rec.Header().Get("Location"))
	}

	// Create.
	req = withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts", strings.NewReader("display_name=Ada&role=admin")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "account+created") {
		t.Fatalf("create: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Unknown action → 404; GET on an action → 405.
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/user_await/explode", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action: want 404, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts/user_await/disable", nil)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET action: want 405, got %d", rec.Code)
	}
	_ = context.Background()
}
