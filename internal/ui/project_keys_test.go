package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// uiMemAPIKeyRepo mirrors the in-memory repo used by the api
// package's handler tests. Kept separate so the two packages can
// evolve independently — and so the UI tests don't take a
// circular import on internal/api.
type uiMemAPIKeyRepo struct {
	mu   sync.Mutex
	rows []*persistence.APIKey
}

func (m *uiMemAPIKeyRepo) Create(_ context.Context, k *persistence.APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *k
	m.rows = append(m.rows, &cp)
	return nil
}
func (m *uiMemAPIKeyRepo) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.KeyHash == h && r.RevokedAt == nil {
			cp := *r
			return &cp, nil
		}
	}
	return nil, persistence.ErrAPIKeyNotFound
}
func (m *uiMemAPIKeyRepo) ListByProject(_ context.Context, p string) ([]*persistence.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.APIKey
	for _, r := range m.rows {
		if r.ProjectID == p {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *uiMemAPIKeyRepo) ListCompanionByProject(_ context.Context, p string) ([]*persistence.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*persistence.APIKey
	for _, r := range m.rows {
		if r.ProjectID == p && r.ClientKind != "" {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *uiMemAPIKeyRepo) TouchLastUsed(_ context.Context, _ string) error { return nil }
func (m *uiMemAPIKeyRepo) Revoke(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range m.rows {
		if r.ID == id && r.RevokedAt == nil {
			r.RevokedAt = &now
		}
	}
	return nil
}
func (m *uiMemAPIKeyRepo) UpdateAllowedWorkflows(_ context.Context, id string, allowed []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			cp := append([]string(nil), allowed...)
			r.AllowedWorkflows = cp
			return nil
		}
	}
	return nil
}
func (m *uiMemAPIKeyRepo) UpdateAllowPush(_ context.Context, id string, allowed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			r.AllowPush = allowed
			return nil
		}
	}
	return persistence.ErrAPIKeyNotFound
}
func (m *uiMemAPIKeyRepo) RevokeByName(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range m.rows {
		if r.Name == name && r.RevokedAt == nil {
			r.RevokedAt = &now
		}
	}
	return nil
}

// TestProjectKeys_GET_ActiveFilterHidesRevoked — the endless-list fix:
// the default view shows only active keys (one-time task keys get
// revoked and otherwise pile up forever); ?status=all reveals them.
func TestProjectKeys_GET_ActiveFilterHidesRevoked(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	now := time.Now().UTC()
	revokedAt := now.Add(-time.Hour)
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-active", ProjectID: "assistant", Name: "live-key",
		KeyPrefix: "live", CreatedAt: now,
	})
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-revoked", ProjectID: "assistant", Name: "spent-task-key",
		KeyPrefix: "spnt", CreatedAt: now.Add(-2 * time.Hour), RevokedAt: &revokedAt,
	})
	server := NewServer(WithAPIKeyRepository(repo))

	// Default (active): the revoked key must NOT render.
	req := httptest.NewRequest(http.MethodGet, "/projects/assistant/keys", nil)
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "live-key")
	assert.NotContains(t, body, "spent-task-key")

	// status=all: both render.
	reqAll := httptest.NewRequest(http.MethodGet, "/projects/assistant/keys?status=all", nil)
	recAll := httptest.NewRecorder()
	server.ProjectKeys(recAll, reqAll, "assistant")
	allBody := recAll.Body.String()
	assert.Contains(t, allBody, "live-key")
	assert.Contains(t, allBody, "spent-task-key")
}

// TestProjectKeys_GET_Paginates — more than one page of active keys
// renders the shared pagination control and limits the window.
func TestProjectKeys_GET_Paginates(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	now := time.Now().UTC()
	for i := 0; i < defaultPerPage+5; i++ {
		_ = repo.Create(context.Background(), &persistence.APIKey{
			ID:        "akey-" + strconv.Itoa(i),
			ProjectID: "assistant",
			Name:      "key-" + strconv.Itoa(i),
			KeyPrefix: "pre" + strconv.Itoa(i),
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	server := NewServer(WithAPIKeyRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/projects/assistant/keys", nil)
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")
	body := rec.Body.String()
	// Page 1 shows the Next control and the total, not all rows.
	assert.Contains(t, body, "Next")
	assert.Contains(t, body, "Page 1 / 2")
	assert.Contains(t, body, "of "+strconv.Itoa(defaultPerPage+5)) // 30 active
	// Newest-first: key-0 (most recent) on page 1; the oldest is not.
	assert.Contains(t, body, "key-0")
	assert.NotContains(t, body, "key-"+strconv.Itoa(defaultPerPage+4))
}

// TestProjectKeys_GET_RendersListAndCreateForm — the page must
// render an empty-state when no keys exist (so the operator sees
// the create form) and a populated table when keys do exist. We
// check the rendered HTML contains the prefix + the create form's
// action endpoint.
func TestProjectKeys_GET_RendersListAndCreateForm(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	secret, _ := apikey.Generate("assistant")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "ha-key",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	server := NewServer(WithAPIKeyRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/projects/assistant/keys", nil)
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Form posts to the same URL.
	assert.Contains(t, body, `action="/ui/projects/assistant/keys"`)
	// Prefix surfaces in the table.
	assert.Contains(t, body, apikey.DisplayPrefix(secret))
	// Raw secret MUST NOT leak in the list-only render.
	assert.NotContains(t, body, secret)
	// Hash MUST NOT leak either.
	assert.NotContains(t, body, apikey.Hash(secret))
}

// TestProjectKeys_POST_Create_RendersSecretBanner — submitting
// the create form returns the page WITH `NewSecret` populated. The
// secret renders inside the amber "shown once" banner; the table
// gains the new row.
func TestProjectKeys_POST_Create_RendersSecretBanner(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	server := NewServer(WithAPIKeyRepository(repo))

	form := url.Values{}
	form.Set("action", "create")
	form.Set("name", "ha-key")
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/assistant/keys",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The secret banner is rendered.
	assert.Contains(t, body, "Secret — shown once. Copy it now.")
	// A row landed in the repo.
	rows, _ := repo.ListByProject(context.Background(), "assistant")
	require.Len(t, rows, 1)
	assert.Equal(t, "ha-key", rows[0].Name)
	// Stored hash != raw secret (defense-in-depth on persistence).
	for _, r := range rows {
		assert.NotEqual(t, "", r.KeyHash)
		assert.NotContains(t, r.KeyHash, "sk-vornik")
	}
}

// TestProjectKeys_POST_Revoke_IdempotentAndIDORSafe — revoke
// succeeds for a key in the project; rejects keys that belong to
// another project (the form's `key_id` is operator-trusted only
// to the extent that we check membership before acting).
func TestProjectKeys_POST_Revoke_IdempotentAndIDORSafe(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	secret, _ := apikey.Generate("project-b")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-b", ProjectID: "project-b", Name: "x",
		KeyHash: apikey.Hash(secret), KeyPrefix: apikey.DisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
	})
	server := NewServer(WithAPIKeyRepository(repo))

	// Attempt cross-project revoke: project-a UI tries to revoke
	// akey-b. Must fail (error banner) and NOT touch the row.
	form := url.Values{}
	form.Set("action", "revoke")
	form.Set("key_id", "akey-b")
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/project-a/keys",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "project-a")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "key not found in this project")
	rows, _ := repo.ListByProject(context.Background(), "project-b")
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].RevokedAt, "cross-project revoke leaked through")
}

// TestProjectKeys_NoRepoReturns503 — the page is feature-flag-able
// via the repo being nil. Static-keys-only deployments must see
// a clean "not configured" response, not a 500.
func TestProjectKeys_NoRepoReturns503(t *testing.T) {
	server := NewServer() // no apikey repo
	req := httptest.NewRequest(http.MethodGet, "/projects/assistant/keys", nil)
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestProjectKeys_D2_ScopedUserCannotMintKeys is the regression test for
// audit finding D2 (2026-06-10): a project-scoped RoleUser browser
// session (auth ON, NOT admin) could POST create/rotate/revoke and mint
// long-lived bearer credentials. The mutating POST now requires admin
// scope and returns 403 for a non-admin session. Fails pre-fix (the POST
// minted a key + 200'd), passes post-fix.
func TestProjectKeys_D2_ScopedUserCannotMintKeys(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	server := NewServer(WithAPIKeyRepository(repo))

	form := url.Values{}
	form.Set("action", "create")
	form.Set("name", "sneaky-key")
	// auth ON + project scope, no admin → RoleUser-equivalent context.
	req := httptest.NewRequest(http.MethodPost, "/projects/assistant/keys",
		strings.NewReader(form.Encode()))
	req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "assistant"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")

	assert.Equal(t, http.StatusForbidden, rec.Code)
	rows, _ := repo.ListByProject(context.Background(), "assistant")
	assert.Empty(t, rows, "no key may be minted by a non-admin session")
}

// TestProjectKeys_D2_AdminCanMintKeys — an admin session passes the gate.
func TestProjectKeys_D2_AdminCanMintKeys(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	server := NewServer(WithAPIKeyRepository(repo))

	form := url.Values{}
	form.Set("action", "create")
	form.Set("name", "admin-key")
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/assistant/keys",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")

	require.Equal(t, http.StatusOK, rec.Code)
	rows, _ := repo.ListByProject(context.Background(), "assistant")
	require.Len(t, rows, 1)
}

// TestProjectKeys_D2_AuthOffCanMintKeys — single-tenant (auth off) is
// implicitly trusted and may mint keys.
func TestProjectKeys_D2_AuthOffCanMintKeys(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	server := NewServer(WithAPIKeyRepository(repo))

	form := url.Values{}
	form.Set("action", "create")
	form.Set("name", "homelab-key")
	req := authOffUIRequest(http.MethodPost, "/projects/assistant/keys")
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.ContentLength = int64(len(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")

	require.Equal(t, http.StatusOK, rec.Code)
	rows, _ := repo.ListByProject(context.Background(), "assistant")
	require.Len(t, rows, 1)
}

// TestProjectKeys_D2_ScopedUserCanStillView — the read-only GET list
// remains available to project members (mutations gated, reads not).
func TestProjectKeys_D2_ScopedUserCanStillView(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	server := NewServer(WithAPIKeyRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/projects/assistant/keys", nil)
	req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "assistant"))
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "assistant")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestRenderKeyRows_StatusAndNullDisplay — the row-rendering
// helper collapses revocation + nil times into the operator-
// readable strings the template expects (status "active" /
// "revoked", "—" for unset times).
func TestRenderKeyRows_StatusAndNullDisplay(t *testing.T) {
	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	revoked := now.Add(-1 * time.Hour)
	last := now.Add(-5 * time.Minute)
	expires := now.Add(30 * 24 * time.Hour)
	in := []*persistence.APIKey{
		{ID: "a", Name: "active", KeyPrefix: "p", CreatedAt: now,
			LastUsedAt: &last, ExpiresAt: &expires},
		{ID: "r", Name: "revoked", KeyPrefix: "p", CreatedAt: now,
			RevokedAt: &revoked},
	}
	got := renderKeyRows(in)
	require.Len(t, got, 2)
	assert.Equal(t, "active", got[0].Status)
	assert.NotEqual(t, "—", got[0].LastUsed)
	assert.NotEqual(t, "—", got[0].Expires)
	assert.Equal(t, "revoked", got[1].Status)
	assert.Equal(t, "—", got[1].LastUsed) // nil → "—"
	assert.Equal(t, "—", got[1].Expires)
}
