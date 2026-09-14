package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// accountsStubRepo is the api-side in-memory IdentityRepository slice the
// account routes exercise (create, grant, revoke, disable, unlink, list).
type accountsStubRepo struct {
	persistence.IdentityRepository
	users    map[string]*persistence.User
	access   map[string]string
	projects map[string][]string
	disabled map[string]bool
	bindings map[string]string
	revoked  map[string]bool
}

func newAccountsStubRepo() *accountsStubRepo {
	return &accountsStubRepo{users: map[string]*persistence.User{}, access: map[string]string{}, projects: map[string][]string{},
		disabled: map[string]bool{}, bindings: map[string]string{}, revoked: map[string]bool{}}
}

func (m *accountsStubRepo) CreateUser(_ context.Context, u *persistence.User) error {
	m.users[u.ID] = u
	return nil
}
func (m *accountsStubRepo) SetUserAccess(_ context.Context, id, role string, projects []string) error {
	if _, ok := m.users[id]; !ok {
		return persistence.ErrUserNotFound
	}
	if role == "user" && len(projects) == 0 {
		return persistence.ErrNoProjects
	}
	m.access[id], m.projects[id] = role, projects
	return nil
}
func (m *accountsStubRepo) RemoveUserAccess(_ context.Context, id string) error {
	if m.access[id] == "admin" && m.adminCount() == 1 {
		return persistence.ErrLastAdmin
	}
	delete(m.access, id)
	return nil
}
func (m *accountsStubRepo) adminCount() int {
	n := 0
	for id, r := range m.access {
		if r == "admin" && !m.disabled[id] {
			n++
		}
	}
	return n
}
func (m *accountsStubRepo) SetUserDisabled(_ context.Context, id string, d bool) error {
	if _, ok := m.users[id]; !ok {
		return persistence.ErrUserNotFound
	}
	m.disabled[id] = d
	return nil
}
func (m *accountsStubRepo) RevokeIdentity(_ context.Context, ch, ext string) error {
	k := ch + ":" + ext
	if _, ok := m.bindings[k]; !ok {
		return persistence.ErrIdentityNotFound
	}
	m.revoked[k] = true
	return nil
}
func (m *accountsStubRepo) ListUsers(context.Context) ([]persistence.UserAdminView, error) {
	var out []persistence.UserAdminView
	for id, u := range m.users {
		v := persistence.UserAdminView{UserID: id, DisplayName: u.DisplayName, Role: m.access[id], Projects: m.projects[id], Disabled: m.disabled[id]}
		for k, uid := range m.bindings {
			if uid == id && !m.revoked[k] {
				ch, ext, _ := strings.Cut(k, ":")
				v.Identities = append(v.Identities, persistence.UserIdentityRef{Channel: ch, ExternalID: ext})
			}
		}
		out = append(out, v)
	}
	return out, nil
}

type accountsStubAudit struct {
	rows []*persistence.AdminAuditEntry
}

func (a *accountsStubAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.rows = append(a.rows, e)
	return nil
}
func (a *accountsStubAudit) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return a.rows, nil
}

func newAccountsServer(t *testing.T) (*Server, *accountsStubRepo, *accountsStubAudit) {
	t.Helper()
	repo, audit := newAccountsStubRepo(), &accountsStubAudit{}
	cfg := config.DefaultConfig()
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAccountsService(authz.NewAccounts(repo, audit)))
	return s, repo, audit
}

// withCapabilityKey stamps auth-enabled + the break-glass key (the CE
// explicit operator capability).
func withCapabilityKey(req *http.Request) *http.Request {
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-operator")
	return req.WithContext(ctx)
}

func accountsReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return withCapabilityKey(r)
}

func decodeAccount(t *testing.T, body string) accountJSON {
	t.Helper()
	var out struct {
		Account accountJSON `json:"account"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out.Account
}

// Review R2 gate: a project-scoped tenant key is 404 (no existence leak); an
// unrestricted key with NO explicit capability is 403 — an empty project
// list is not an operator capability; the break-glass key and an admin
// session pass; auth-off passes (trusted local operator).
func TestOperatorAccounts_CapabilityGate(t *testing.T) {
	s, _, _ := newAccountsServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operator/accounts", nil)
	req = req.WithContext(context.WithValue(ContextWithProjectScope(req.Context(), "proj-a"), authEnabledKey, true))
	s.OperatorAccounts(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("project tenant: want 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/operator/accounts", nil)
	req = req.WithContext(context.WithValue(context.WithValue(req.Context(), authEnabledKey, true), apiKeyKey, "sk-unrestricted-but-plain"))
	s.OperatorAccounts(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "OPERATOR_CAPABILITY_REQUIRED") {
		t.Fatalf("unrestricted plain key: want 403 OPERATOR_CAPABILITY_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.OperatorAccounts(rec, accountsReq(http.MethodGet, "/api/v1/operator/accounts", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("break-glass key: want 200, got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/operator/accounts", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	s.OperatorAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth off: want 200, got %d", rec.Code)
	}
}

func TestOperatorAccounts_Lifecycle(t *testing.T) {
	s, repo, audit := newAccountsServer(t)

	rec := httptest.NewRecorder()
	s.OperatorAccounts(rec, accountsReq(http.MethodPost, "/api/v1/operator/accounts", `{"displayName":"Ada","role":"user","projects":["a"]}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	acc := decodeAccount(t, rec.Body.String())
	if acc.Role != "user" || len(acc.Projects) != 1 {
		t.Fatalf("created account: %+v", acc)
	}
	repo.bindings["telegram:7"] = acc.ID

	rec = httptest.NewRecorder()
	s.OperatorAccountItem(rec, accountsReq(http.MethodPost, "/api/v1/operator/accounts/"+acc.ID+"/grant", `{"role":"admin"}`))
	if rec.Code != http.StatusOK || decodeAccount(t, rec.Body.String()).Role != "admin" {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}

	// Sole admin cannot be revoked (repository guard surfaces as 409).
	rec = httptest.NewRecorder()
	s.OperatorAccountItem(rec, accountsReq(http.MethodPost, "/api/v1/operator/accounts/"+acc.ID+"/revoke", ""))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "LAST_ADMIN") {
		t.Fatalf("last-admin revoke: want 409 LAST_ADMIN, got %d %s", rec.Code, rec.Body.String())
	}

	// Crafted unlink of a binding the account does not own → 400.
	rec = httptest.NewRecorder()
	s.OperatorAccountItem(rec, accountsReq(http.MethodPost, "/api/v1/operator/accounts/"+acc.ID+"/unlink", `{"channel":"telegram","externalId":"999"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unowned unlink: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.OperatorAccountItem(rec, accountsReq(http.MethodPost, "/api/v1/operator/accounts/"+acc.ID+"/unlink", `{"channel":"telegram","externalId":"7"}`))
	if rec.Code != http.StatusOK || len(decodeAccount(t, rec.Body.String()).Identities) != 0 {
		t.Fatalf("owned unlink: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.OperatorAccountItem(rec, accountsReq(http.MethodPost, "/api/v1/operator/accounts/"+acc.ID+"/disable", ""))
	if rec.Code != http.StatusOK || !decodeAccount(t, rec.Body.String()).Disabled {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.OperatorAccountItem(rec, accountsReq(http.MethodGet, "/api/v1/operator/accounts/nope", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: want 404, got %d", rec.Code)
	}

	// Every mutation is audited with the STABLE key principal, never the
	// bearer.
	if len(audit.rows) != 4 {
		t.Fatalf("audit rows = %d, want 4", len(audit.rows))
	}
	for _, row := range audit.rows {
		if strings.Contains(row.Principal, "sk-operator") {
			t.Fatalf("audit principal carries the bearer secret: %q", row.Principal)
		}
		if !strings.HasPrefix(row.Principal, "api_key_sha256:") {
			t.Fatalf("audit principal must be the key fingerprint, got %q", row.Principal)
		}
	}
}

func TestOperatorAccounts_UnwiredIs503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operator/accounts", nil)
	s.OperatorAccounts(rec, req.WithContext(context.WithValue(req.Context(), authEnabledKey, false)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: want 503, got %d", rec.Code)
	}
}

// The CE routes must never live under /api/v1/admin/ (the EE admin-gate
// prefix) — same invariant the proposals routes pin.
func TestOperatorAccountsRoute_NotUnderAdminPrefix(t *testing.T) {
	for _, p := range []string{"/api/v1/operator/accounts", "/api/v1/operator/accounts/"} {
		if strings.HasPrefix(p, adminRoutePrefix) {
			t.Fatalf("%s is under the admin prefix", p)
		}
	}
}
