package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeKeyRepo lets us assert lookup behavior without a DB.
// Implements the full persistence.APIKeyRepository interface; panics on
// methods that are not exercised by gitHTTPAuth tests.
type fakeKeyRepo struct {
	key *persistence.APIKey
	err error
}

func (f fakeKeyRepo) LookupActiveByHash(_ context.Context, _ string) (*persistence.APIKey, error) {
	return f.key, f.err
}
func (f fakeKeyRepo) Create(_ context.Context, _ *persistence.APIKey) error {
	panic("fakeKeyRepo: Create not implemented")
}
func (f fakeKeyRepo) ListByProject(_ context.Context, _ string) ([]*persistence.APIKey, error) {
	panic("fakeKeyRepo: ListByProject not implemented")
}
func (f fakeKeyRepo) ListCompanionByProject(_ context.Context, _ string) ([]*persistence.APIKey, error) {
	panic("fakeKeyRepo: ListCompanionByProject not implemented")
}
func (f fakeKeyRepo) TouchLastUsed(_ context.Context, _ string) error {
	panic("fakeKeyRepo: TouchLastUsed not implemented")
}
func (f fakeKeyRepo) Revoke(_ context.Context, _ string) error {
	panic("fakeKeyRepo: Revoke not implemented")
}
func (f fakeKeyRepo) UpdateAllowedWorkflows(_ context.Context, _ string, _ []string) error {
	panic("fakeKeyRepo: UpdateAllowedWorkflows not implemented")
}
func (f fakeKeyRepo) UpdateAllowPush(_ context.Context, _ string, _ bool) error {
	panic("fakeKeyRepo: UpdateAllowPush not implemented")
}
func (f fakeKeyRepo) RevokeByName(_ context.Context, _ string) error {
	panic("fakeKeyRepo: RevokeByName not implemented")
}

// TestGitHTTPAuth_AuthDisabled covers the auth_enabled=false fast path.
// The middleware must call next (HTTP 200) and stamp a nil *persistence.APIKey
// under gitKeyCtxKey and "anonymous" under gitRemoteUserCtxKey — even when no
// credentials are supplied.
func TestGitHTTPAuth_AuthDisabled(t *testing.T) {
	repo := fakeKeyRepo{} // no key; LookupActiveByHash must never be called
	mw := gitHTTPAuth(repo, func(string) bool { return false }, false /* authEnabled=false */)

	var (
		gotKey        *persistence.APIKey
		gotRemoteUser string
		nextCalled    bool
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		// Read the two context values the middleware is expected to stamp.
		gotKey, _ = r.Context().Value(gitKeyCtxKey{}).(*persistence.APIKey)
		gotRemoteUser, _ = r.Context().Value(gitRemoteUserCtxKey{}).(string)
		w.WriteHeader(http.StatusOK)
	})

	h := mw(gitServiceUpload, next)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/git/proj_any.git/info/refs?service=git-upload-pack", nil)
	// Deliberately no credentials.
	req.SetPathValue("projectID", "proj_any")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !nextCalled {
		t.Fatal("next handler was not called")
	}
	if gotKey != nil {
		t.Fatalf("context key = %v, want nil", gotKey)
	}
	if gotRemoteUser != "anonymous" {
		t.Fatalf("REMOTE_USER = %q, want \"anonymous\"", gotRemoteUser)
	}
}

// TestGitHTTPAuth_AdminBypass verifies that an admin key (adminCheck returns
// true) is NOT rejected with 404 when its ProjectID differs from the path
// project — the admin bypass on line 96 of git_http_auth.go skips the
// project-match gate for admin-class keys.
func TestGitHTTPAuth_AdminBypass(t *testing.T) {
	const rawKey = "admin_secret"
	// The key belongs to a DIFFERENT project than what is in the path.
	adminKey := &persistence.APIKey{ID: "akey_admin", ProjectID: "proj_admin_home", Name: "admin"}
	repo := fakeKeyRepo{key: adminKey}
	// adminCheck returns true for our raw key.
	mw := gitHTTPAuth(repo, func(k string) bool { return k == rawKey }, true)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := mw(gitServiceUpload, next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/git/proj_other.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("git", rawKey)
	req.SetPathValue("projectID", "proj_other") // different from adminKey.ProjectID
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin bypass: status = %d, want 200 (admin key should not be 404'd on project mismatch)", rec.Code)
	}
}

// TestSanitizeRemoteUser exercises sanitizeRemoteUser edge cases directly.
func TestSanitizeRemoteUser(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty input returns key",
			input: "",
			want:  "key",
		},
		{
			name: "control and shell-meta chars stripped",
			// Contains: \r, \n (control), ` $ " ' ; \ (shell-meta), leaving only a, b, c.
			input: "a\r\nb`$\"';\\c",
			want:  "abc",
		},
		{
			name:  "clean name passes through unchanged",
			input: "my-deploy-key",
			want:  "my-deploy-key",
		},
		{
			name:  "only unsafe chars returns key",
			input: "\x01\x7f\r\n`$\"';\\",
			want:  "key",
		},
		{
			name:  "space and printable non-meta chars preserved",
			input: "hello world!",
			want:  "hello world!",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeRemoteUser(tc.input)
			if got != tc.want {
				t.Fatalf("sanitizeRemoteUser(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeGitProjectID(t *testing.T) {
	ok := []string{"proj_abc", "proj_20260101_deadbeef"}
	bad := []string{"", "..", "../etc", "a/b", "a\\b", "a\x00b", "p$(x)"}
	for _, s := range ok {
		if _, err := sanitizeGitProjectID(s); err != nil {
			t.Errorf("%q should be valid: %v", s, err)
		}
	}
	for _, s := range bad {
		if _, err := sanitizeGitProjectID(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}

// TestGitHTTPAuth_PushCapability verifies the Task 2.4 push-capability gate on
// the receive service. The capability check runs AFTER credential validation:
//   - missing creds → 401 (NOT 403): unauth must not leak that push is gated.
//   - unknown key → 401.
//   - valid read-only key (AllowPush=false, non-admin) → 403.
//   - valid key with AllowPush=true → 200.
//   - admin key → 200 even without AllowPush.
func TestGitHTTPAuth_PushCapability(t *testing.T) {
	const proj = "proj_push"
	readOnly := &persistence.APIKey{ID: "akey_ro", ProjectID: proj, Name: "ro", AllowPush: false}
	pusher := &persistence.APIKey{ID: "akey_push", ProjectID: proj, Name: "push", AllowPush: true}
	adminKey := &persistence.APIKey{ID: "akey_adm", ProjectID: "other", Name: "adm"}

	cases := []struct {
		name       string
		repoKey    *persistence.APIKey
		basicPass  string
		isAdmin    bool
		wantStatus int
	}{
		{"missing creds → 401", pusher, "", false, http.StatusUnauthorized},
		{"unknown key → 401", nil, "secret", false, http.StatusUnauthorized},
		{"read-only key → 403", readOnly, "secret", false, http.StatusForbidden},
		{"allow_push key → 200", pusher, "secret", false, http.StatusOK},
		{"admin key → 200", adminKey, "admin_raw", true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := fakeKeyRepo{key: tc.repoKey}
			adminCheck := func(k string) bool { return tc.isAdmin && k == "admin_raw" }
			mw := gitHTTPAuth(repo, adminCheck, true)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := mw(gitServiceReceive, next)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/git/"+proj+".git/git-receive-pack", nil)
			if tc.basicPass != "" {
				req.SetBasicAuth("git", tc.basicPass)
			}
			req.SetPathValue("projectID", proj)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// 401 must carry the WWW-Authenticate challenge (git CLI relies on it).
			if tc.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 missing WWW-Authenticate challenge")
			}
		})
	}
}

func TestGitHTTPAuth_Read(t *testing.T) {
	proj := "proj_abc"
	valid := &persistence.APIKey{ID: "akey_1", ProjectID: proj, Name: "laptop"}
	cases := []struct {
		name       string
		repoKey    *persistence.APIKey
		basicPass  string
		pathProj   string
		wantStatus int
	}{
		{"valid read key", valid, "secret", proj, http.StatusOK},
		{"missing creds", valid, "", proj, http.StatusUnauthorized},
		{"unknown key", nil, "secret", proj, http.StatusUnauthorized},
		{"cross-project → 404", valid, "secret", "proj_other", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := fakeKeyRepo{key: tc.repoKey}
			mw := gitHTTPAuth(repo, func(string) bool { return false }, true)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := mw(gitServiceUpload, next)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/git/"+tc.pathProj+".git/info/refs?service=git-upload-pack", nil)
			if tc.basicPass != "" {
				req.SetBasicAuth("git", tc.basicPass)
			}
			// projectID is injected by the router; emulate it:
			req.SetPathValue("projectID", tc.pathProj)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}
