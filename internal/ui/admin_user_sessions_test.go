package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// stubSessionRepo implements the UISessionRepository methods the viewer
// touches; the rest panic via the embedded nil interface.
type stubSessionRepo struct {
	persistence.UISessionRepository
	active  []*persistence.UISession
	revoked []string
}

func (s *stubSessionRepo) ListActiveByUser(_ context.Context, userID string) ([]*persistence.UISession, error) {
	var out []*persistence.UISession
	for _, ses := range s.active {
		if ses.UserID == userID {
			out = append(out, ses)
		}
	}
	return out, nil
}

func (s *stubSessionRepo) RevokeSessionForUser(_ context.Context, userID, sessionID string) error {
	for i, ses := range s.active {
		if ses.ID == sessionID && ses.UserID == userID {
			s.revoked = append(s.revoked, sessionID)
			s.active = append(s.active[:i], s.active[i+1:]...)
			return nil
		}
	}
	return persistence.ErrSessionNotFound
}

func sessionsServer(idRepo *stubUsersIdentityRepo, sess *stubSessionRepo, audit *stubAdminAuditRepo) *Server {
	return NewServer(
		WithIdentityRepository(idRepo),
		WithUISessionRepository(sess),
		WithAdminAuditRepository(audit),
	)
}

func sess(id, user, ip, ua string) *persistence.UISession {
	now := time.Now().UTC()
	return &persistence.UISession{
		ID: id, UserID: user, Provider: "github", IP: ip, UserAgent: ua,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(168 * time.Hour),
	}
}

func TestFriendlyUserAgent(t *testing.T) {
	cases := []struct{ ua, want string }{
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36", "Chrome on macOS"},
		{"Mozilla/5.0 (Windows NT 10.0) Gecko/20100101 Firefox/121.0", "Firefox on Windows"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15) Version/17.0 Safari/605.1", "Safari on macOS"},
		{"Mozilla/5.0 (Windows NT 10.0) Chrome/120 Edg/120.0", "Edge on Windows"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) Version/17.0 Mobile Safari/604.1", "Safari on iOS"},
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := friendlyUserAgent(c.ua); got != c.want {
			t.Errorf("friendlyUserAgent(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
}

func TestAdminUserSessions_NotWired(t *testing.T) {
	s := NewServer() // no repos
	rec := httptest.NewRecorder()
	s.AdminUserSessions(rec, httptest.NewRequest(http.MethodGet, "/admin/users/u/sessions", nil), "u")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "identity core") {
		t.Fatalf("expected identity-core notice, code=%d", rec.Code)
	}
}

func TestAdminUserSessions_RendersAndMarksCurrent(t *testing.T) {
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{
		sess("sess_a", "user_admin", "192.0.2.10", "Mozilla/5.0 (Macintosh) Chrome/120 Safari/537"),
		sess("sess_b", "user_admin", "10.0.0.5", "Mozilla/5.0 (Windows NT 10.0) Firefox/121"),
	}}
	s := sessionsServer(idRepo, sr, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/users/user_admin/sessions", nil)
	req = req.WithContext(api.ContextWithSessionID(req.Context(), "sess_a"))
	s.AdminUserSessions(rec, req, "user_admin")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	for _, want := range []string{"Chrome on macOS", "Firefox on Windows", "192.0.2.10", "this session", "protected"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// The current session (sess_a) must NOT offer a terminate form.
	if strings.Contains(body, `/sessions/sess_a/terminate"`) {
		t.Error("current session must not have a terminate button")
	}
	if !strings.Contains(body, `/sessions/sess_b/terminate"`) {
		t.Error("non-current session should have a terminate button")
	}
}

func TestAdminUserSessions_TerminateOwned(t *testing.T) {
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{sess("sess_b", "user_admin", "10.0.0.5", "")}}
	audit := &stubAdminAuditRepo{}
	s := sessionsServer(idRepo, sr, audit)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/sessions/sess_b/terminate", nil)
	s.AdminUserSessionTerminate(rec, req, "user_admin", "sess_b")
	if len(sr.revoked) != 1 || sr.revoked[0] != "sess_b" {
		t.Fatalf("revoked = %v, want [sess_b]", sr.revoked)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "user.session_revoke" {
		t.Errorf("audit = %+v, want one user.session_revoke", audit.rows)
	}
}

func TestAdminUserSessions_TerminateForeignRejected(t *testing.T) {
	// sess_x belongs to a different user — must not be revocable via
	// user_admin's page.
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{
		sess("sess_b", "user_admin", "", ""),
		sess("sess_x", "user_await", "", ""),
	}}
	s := sessionsServer(idRepo, sr, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/sessions/sess_x/terminate", nil)
	s.AdminUserSessionTerminate(rec, req, "user_admin", "sess_x")
	if len(sr.revoked) != 0 {
		t.Fatalf("foreign session must not be revoked, got %v", sr.revoked)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code=%d, want 404", rec.Code)
	}
}

func TestAdminUserSessions_TerminateCurrentRefused(t *testing.T) {
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{sess("sess_a", "user_admin", "", "")}}
	s := sessionsServer(idRepo, sr, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/sessions/sess_a/terminate", nil)
	req = req.WithContext(api.ContextWithSessionID(req.Context(), "sess_a"))
	s.AdminUserSessionTerminate(rec, req, "user_admin", "sess_a")
	if len(sr.revoked) != 0 {
		t.Fatalf("own current session must not be revoked, got %v", sr.revoked)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notice=") {
		t.Errorf("expected refusal notice, got %q", loc)
	}
}

// The current-session guard protects the CALLER's session, not the
// target user's. Admin (current sess_admin) terminating-all on Bob
// revokes all of Bob's sessions including Bob's own current one.
func TestAdminUserSessions_TerminateAllCrossUserRevokesTargetCurrent(t *testing.T) {
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{
		sess("sess_bob1", "user_await", "", ""),
		sess("sess_bob2", "user_await", "", ""),
	}}
	s := sessionsServer(idRepo, sr, &stubAdminAuditRepo{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_await/sessions/terminate-all", nil)
	// Caller's own session belongs to a DIFFERENT user (the admin).
	req = req.WithContext(api.ContextWithSessionID(req.Context(), "sess_admin"))
	s.AdminUserSessionsTerminateAll(rec, req, "user_await")
	if len(sr.revoked) != 2 {
		t.Fatalf("cross-user terminate-all should revoke all target sessions, got %v", sr.revoked)
	}
}

func TestAdminUserSessions_RouterDispatch(t *testing.T) {
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{sess("sess_b", "user_admin", "10.0.0.5", "")}}
	s := sessionsServer(idRepo, sr, &stubAdminAuditRepo{})

	// GET list.
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/users/user_admin/sessions", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("router did not dispatch the sessions list (code=%d)", rec.Code)
	}

	// POST terminate.
	rec = httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/sessions/sess_b/terminate", nil)))
	if len(sr.revoked) != 1 || sr.revoked[0] != "sess_b" {
		t.Fatalf("router did not dispatch terminate: revoked=%v", sr.revoked)
	}
}

func TestAdminUserSessions_TerminateAllKeepsCurrent(t *testing.T) {
	idRepo := &stubUsersIdentityRepo{users: twoUserFixture()}
	sr := &stubSessionRepo{active: []*persistence.UISession{
		sess("sess_a", "user_admin", "", ""), // current
		sess("sess_b", "user_admin", "", ""),
		sess("sess_c", "user_admin", "", ""),
	}}
	audit := &stubAdminAuditRepo{}
	s := sessionsServer(idRepo, sr, audit)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/users/user_admin/sessions/terminate-all", nil)
	req = req.WithContext(api.ContextWithSessionID(req.Context(), "sess_a"))
	s.AdminUserSessionsTerminateAll(rec, req, "user_admin")
	// sess_b and sess_c revoked; sess_a (current) kept.
	if len(sr.revoked) != 2 {
		t.Fatalf("revoked = %v, want sess_b + sess_c (2)", sr.revoked)
	}
	for _, id := range sr.revoked {
		if id == "sess_a" {
			t.Error("terminate-all must not revoke the caller's current session")
		}
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "user.session_revoke_all" {
		t.Errorf("audit = %+v, want one user.session_revoke_all", audit.rows)
	}
}
