package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubUsersIdentityRepo implements just the IdentityRepository methods
// /ui/admin/users touches; the rest panic via the embedded nil
// interface (never called by the handlers under test).
type stubUsersIdentityRepo struct {
	persistence.IdentityRepository
	users []persistence.UserAdminView

	setAccess []struct {
		userID, role string
		projects     []string
	}
	removed  []string
	disabled []struct {
		userID string
		off    bool
	}
	revoked []struct{ channel, externalID string }

	setAccessErr error
}

func (s *stubUsersIdentityRepo) ListUsers(context.Context) ([]persistence.UserAdminView, error) {
	return s.users, nil
}

func (s *stubUsersIdentityRepo) SetUserAccess(_ context.Context, userID, role string, projects []string) error {
	s.setAccess = append(s.setAccess, struct {
		userID, role string
		projects     []string
	}{userID, role, projects})
	return s.setAccessErr
}

func (s *stubUsersIdentityRepo) RemoveUserAccess(_ context.Context, userID string) error {
	s.removed = append(s.removed, userID)
	return nil
}

func (s *stubUsersIdentityRepo) SetUserDisabled(_ context.Context, userID string, disabled bool) error {
	s.disabled = append(s.disabled, struct {
		userID string
		off    bool
	}{userID, disabled})
	return nil
}

func (s *stubUsersIdentityRepo) RevokeIdentity(_ context.Context, channel, externalID string) error {
	s.revoked = append(s.revoked, struct{ channel, externalID string }{channel, externalID})
	return nil
}

func usersServer(repo *stubUsersIdentityRepo, audit *stubAdminAuditRepo) *Server {
	return NewServer(
		WithIdentityRepository(repo),
		WithAdminAuditRepository(audit),
		WithProjectRegistry(registry.New()),
	)
}

// awaiting + admin fixture used by several tests.
func twoUserFixture() []persistence.UserAdminView {
	return []persistence.UserAdminView{
		{UserID: "user_admin", DisplayName: "Vadim", Role: "admin",
			Identities: []persistence.UserIdentityRef{{Channel: "github", ExternalID: "5553528", Display: "@grinco"}}},
		{UserID: "user_await", DisplayName: "@janka-lang", Role: "",
			Identities: []persistence.UserIdentityRef{{Channel: "github", ExternalID: "295941391", Display: "@janka-lang"}}},
	}
}

func TestAdminUsers_NotWired(t *testing.T) {
	s := NewServer() // no identity repo
	rec := httptest.NewRecorder()
	s.AdminUsers(rec, httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "identity core") {
		t.Errorf("expected identity-core-required notice, body=%q", rec.Body.String())
	}
}

func TestAdminUsers_RendersAwaitingFirstWithBadge(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.AdminUsers(rec, httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"@janka-lang", "Vadim", "awaiting"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("body missing %q", want)
		}
	}
	// Awaiting user must render before the approved admin (sorted first).
	if i, j := strings.Index(body, "@janka-lang"), strings.Index(body, "Vadim"); i < 0 || j < 0 || i > j {
		t.Errorf("awaiting user not rendered first (janka=%d vadim=%d)", i, j)
	}
}

// regression: the page rendered only the "all" checkbox and errored on
// the per-project loop because the UI funcMap overrides `index` with a
// TaskStatus-map signature; the original test used an empty registry so
// the loop never ran. This renders WITH projects and asserts a complete,
// error-free page including a pre-checked project box.
func TestAdminUsers_RendersProjectCheckboxes(t *testing.T) {
	s := usersServer(&stubUsersIdentityRepo{}, &stubAdminAuditRepo{})
	views := []persistence.UserAdminView{
		{UserID: "user_member", DisplayName: "Bob", Role: "user", Projects: []string{"janka"}},
	}
	data := AdminUsersData{
		adminCommonData: adminCommonData{Title: "Users", CurrentPage: "admin", IsAdmin: true},
		Available:       true,
		Projects:        []string{"janka", "snake"},
		Users:           buildAdminUserRows(views, []string{"janka", "snake"}),
	}
	rec := httptest.NewRecorder()
	s.render(rec, "admin_users.html", data)
	body := rec.Body.String()
	if strings.Contains(body, "Internal server error") {
		t.Fatalf("template render errored mid-page: %q", body[max(0, len(body)-300):])
	}
	if !strings.Contains(body, "</html>") {
		t.Fatal("page did not render to completion (template execution error)")
	}
	for _, want := range []string{`value="janka"`, `value="snake"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing project checkbox %q", want)
		}
	}
	// Bob has janka → that checkbox is pre-checked.
	jankaIdx := strings.Index(body, `value="janka"`)
	if jankaIdx < 0 || !strings.Contains(body[jankaIdx:jankaIdx+80], "checked") {
		t.Error("granted project 'janka' should render checked")
	}
}

// regression: the grant audit row was silently dropped because After was
// plain text written into a jsonb column. After must be valid JSON and a
// row must be recorded for every access change.
func TestAdminUsers_GrantWritesValidJSONAudit(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	audit := &stubAdminAuditRepo{}
	s := usersServer(repo, audit)
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"user"}, "projects": {"janka"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserGrant(rec, req, "user_await")
	if len(audit.rows) != 1 {
		t.Fatalf("expected exactly one audit row, got %d", len(audit.rows))
	}
	after := audit.rows[0].After
	if after == "" || !json.Valid([]byte(after)) {
		t.Fatalf("audit After must be valid JSON, got %q", after)
	}
	if !strings.Contains(after, "janka") || !strings.Contains(after, "user") {
		t.Errorf("audit After should record role+projects, got %q", after)
	}
}

// Access mutations must refuse to run when no audit sink is wired.
func TestAdminUsers_GrantRefusedWithoutAuditSink(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := NewServer(WithIdentityRepository(repo), WithProjectRegistry(registry.New())) // no audit repo
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"user"}, "projects": {"janka"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserGrant(rec, req, "user_await")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when audit sink missing", rec.Code)
	}
	if len(repo.setAccess) != 0 {
		t.Fatalf("mutation must not run without an audit sink, got %d", len(repo.setAccess))
	}
}

func TestAdminUsers_GrantUserRoleWithProject(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	audit := &stubAdminAuditRepo{}
	s := usersServer(repo, audit)
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"user"}, "projects": {"janka"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserGrant(rec, req, "user_await")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	if len(repo.setAccess) != 1 {
		t.Fatalf("SetUserAccess calls = %d, want 1", len(repo.setAccess))
	}
	got := repo.setAccess[0]
	if got.userID != "user_await" || got.role != "user" ||
		len(got.projects) != 1 || got.projects[0] != "janka" {
		t.Errorf("SetUserAccess args = %+v, want user_await/user/[janka]", got)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "user.grant" || audit.rows[0].Target != "user_await" {
		t.Errorf("audit = %+v, want one user.grant on user_await", audit.rows)
	}
}

func TestAdminUsers_GrantUserRoleNoProjectsRejected(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"user"}} // no projects
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserGrant(rec, req, "user_await")

	if len(repo.setAccess) != 0 {
		t.Fatalf("SetUserAccess should not be called, got %d", len(repo.setAccess))
	}
	// Redirects back with an error notice rather than 500-ing.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notice=") {
		t.Errorf("expected error notice in redirect, got %q", loc)
	}
}

func TestAdminUsers_GrantAdminIgnoresProjects(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"admin"}, "projects": {"janka"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserGrant(rec, req, "user_await")
	if len(repo.setAccess) != 1 || repo.setAccess[0].role != "admin" {
		t.Fatalf("SetUserAccess = %+v, want one admin grant", repo.setAccess)
	}
}

func TestAdminUsers_DisableLastAdminRejected(t *testing.T) {
	// Only one enabled admin → disabling them is refused (lockout guard).
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/disable", nil)
	s.AdminUserDisable(rec, req, "user_admin")
	if len(repo.disabled) != 0 {
		t.Fatalf("SetUserDisabled should not run for the last admin, got %d", len(repo.disabled))
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notice=") {
		t.Errorf("expected lockout notice, got %q", loc)
	}
}

func TestAdminUsers_DisableSecondAdminAllowed(t *testing.T) {
	users := twoUserFixture()
	users[1].Role = "admin" // janka is also an enabled admin now → two admins
	repo := &stubUsersIdentityRepo{users: users}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/disable", nil)
	s.AdminUserDisable(rec, req, "user_admin")
	if len(repo.disabled) != 1 || repo.disabled[0].userID != "user_admin" || !repo.disabled[0].off {
		t.Fatalf("SetUserDisabled = %+v, want one disable of user_admin", repo.disabled)
	}
}

func TestAdminUsers_RevokeIdentityForeignRejected(t *testing.T) {
	// Revoking an identity that does NOT belong to the target user is
	// rejected (a crafted form must not unlink someone else's binding).
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	form := url.Values{"channel": {"github"}, "external_id": {"5553528"}} // belongs to user_admin, not user_await
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/revoke-identity",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserRevokeIdentity(rec, req, "user_await")
	if len(repo.revoked) != 0 {
		t.Fatalf("RevokeIdentity should not run for a foreign binding, got %d", len(repo.revoked))
	}
}

func TestAdminUsers_RevokeIdentityOwnedAllowed(t *testing.T) {
	users := twoUserFixture()
	users[1].Role = "admin" // keep another admin so the guard doesn't block
	repo := &stubUsersIdentityRepo{users: users}
	audit := &stubAdminAuditRepo{}
	s := usersServer(repo, audit)
	rec := httptest.NewRecorder()
	form := url.Values{"channel": {"github"}, "external_id": {"295941391"}} // belongs to user_await
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/revoke-identity",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserRevokeIdentity(rec, req, "user_await")
	if len(repo.revoked) != 1 || repo.revoked[0].externalID != "295941391" {
		t.Fatalf("RevokeIdentity = %+v, want one revoke of 295941391", repo.revoked)
	}
}

func TestAdminLanding_AwaitingCallout(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()} // one awaiting
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.AdminLanding(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "awaiting approval") || !strings.Contains(body, "/ui/admin/users") {
		t.Errorf("landing missing awaiting callout / users link")
	}
}

func TestAdminLanding_NoCalloutWhenNoneAwaiting(t *testing.T) {
	users := twoUserFixture()
	users[1].Role = "user"
	users[1].Projects = []string{"janka"} // no awaiting users
	repo := &stubUsersIdentityRepo{users: users}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.AdminLanding(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if strings.Contains(rec.Body.String(), "awaiting approval") {
		t.Errorf("callout should be hidden when zero awaiting")
	}
}

func TestAdminUsers_RouterDispatchesList(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/users", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "@janka-lang") {
		t.Fatalf("router did not dispatch /admin/users (code=%d)", rec.Code)
	}
}

func TestAdminUsers_RouterDispatchesGrant(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"user"}, "projects": {"janka"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.adminRouter(rec, withAdminUI(req))
	if len(repo.setAccess) != 1 || repo.setAccess[0].userID != "user_await" {
		t.Fatalf("router did not dispatch grant POST: %+v", repo.setAccess)
	}
}

// regression: 2026-06-22 — @janka-lang awaiting-access GitHub login could
// not be approved via the UI (no admin users page existed); only DB edits
// worked. Drives the grant handler for an awaiting GitHub user and asserts
// the repo is asked to grant the named project.
func TestAdminUsers_Regression_JankaApprovedIntoProject(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	s := usersServer(repo, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	form := url.Values{"role": {"user"}, "projects": {"janka"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/grant",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminUserGrant(rec, req, "user_await")
	if len(repo.setAccess) != 1 || repo.setAccess[0].userID != "user_await" ||
		repo.setAccess[0].role != "user" || repo.setAccess[0].projects[0] != "janka" {
		t.Fatalf("janka not granted into project janka: %+v", repo.setAccess)
	}
}
