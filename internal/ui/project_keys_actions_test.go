package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

func newKeysActionRequest(t *testing.T, form url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(t, req.ParseForm())
	return req
}

// TestHandleProjectKeysAction_CreateMissingNameErrors — name is
// required; the data carries an error rather than minting an
// unnamed key.
func TestHandleProjectKeysAction_CreateMissingNameErrors(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	srv := NewServer(WithAPIKeyRepository(repo))
	req := newKeysActionRequest(t, url.Values{"action": {"create"}})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Equal(t, "name is required", data.Error)
	assert.Empty(t, repo.rows, "no key should have been minted")
}

// TestHandleProjectKeysAction_RotateMissingKeyIDErrors — the form
// must carry key_id.
func TestHandleProjectKeysAction_RotateMissingKeyIDErrors(t *testing.T) {
	srv := NewServer(WithAPIKeyRepository(&uiMemAPIKeyRepo{}))
	req := newKeysActionRequest(t, url.Values{"action": {"rotate"}})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Equal(t, "key_id required", data.Error)
}

// TestHandleProjectKeysAction_RotateKeyNotInProjectErrors — IDOR
// guard: a key_id from another project must NOT be rotated.
func TestHandleProjectKeysAction_RotateKeyNotInProjectErrors(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	// Seed a key belonging to project p2.
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:        "akey-foreign",
		ProjectID: "p2",
		Name:      "foreign",
		KeyHash:   "x",
	})
	srv := NewServer(WithAPIKeyRepository(repo))
	req := newKeysActionRequest(t, url.Values{
		"action": {"rotate"},
		"key_id": {"akey-foreign"},
	})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Equal(t, "key not found in this project", data.Error)
}

// TestHandleProjectKeysAction_RotateRevokedErrors — a key that's
// already revoked can't be rotated.
func TestHandleProjectKeysAction_RotateRevokedErrors(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-1", ProjectID: "p1", Name: "old", KeyHash: "h",
	})
	require.NoError(t, repo.Revoke(context.Background(), "akey-1"))
	srv := NewServer(WithAPIKeyRepository(repo))
	req := newKeysActionRequest(t, url.Values{
		"action": {"rotate"},
		"key_id": {"akey-1"},
	})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Equal(t, "cannot rotate a revoked key", data.Error)
}

// TestHandleProjectKeysAction_RotateHappyMintsFreshAndRevokesOld
// — the rotate path leaves the project with one active key + one
// revoked, and the response data carries the new secret.
func TestHandleProjectKeysAction_RotateHappyMintsFreshAndRevokesOld(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	priorSecret, _ := apikey.Generate("p1")
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-old", ProjectID: "p1", Name: "prod",
		KeyHash:   apikey.Hash(priorSecret),
		KeyPrefix: apikey.DisplayPrefix(priorSecret),
	})
	srv := NewServer(WithAPIKeyRepository(repo))
	req := newKeysActionRequest(t, url.Values{
		"action": {"rotate"},
		"key_id": {"akey-old"},
	})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Empty(t, data.Error, "rotate must succeed (Error=%q)", data.Error)
	assert.Contains(t, data.Success, "Rotated key prod")
	assert.NotEmpty(t, data.NewSecret, "new secret must be returned for the banner")

	// Repo state: 2 rows, the old one revoked, the new one fresh.
	rows, _ := repo.ListByProject(context.Background(), "p1")
	require.Len(t, rows, 2)
	revokedCount := 0
	for _, r := range rows {
		if r.RevokedAt != nil {
			revokedCount++
		}
	}
	assert.Equal(t, 1, revokedCount, "exactly one row should be revoked after rotate")
}

// TestHandleProjectKeysAction_RotatePreservesCompanionScope is the
// regression test for the 2026-06-27 incident: rotating a companion key
// via the web UI silently dropped ClientKind (and the other scope/limit
// columns), demoting it to a plain key that the companion MCP endpoint
// then rejected with "bearer token is not a companion-scoped key". The
// UI rotate action now mints the replacement via APIKey.RotatedCopy —
// the same carry-over path the REST handler uses — so every scope,
// limit, and capability attribute survives the rotation.
func TestHandleProjectKeysAction_RotatePreservesCompanionScope(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	priorSecret, _ := apikey.Generate("companion-example")
	rps := 5
	budget := 12.5
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-comp", ProjectID: "companion-example", Name: "vadim/laptop",
		KeyHash: apikey.Hash(priorSecret), KeyPrefix: apikey.DisplayPrefix(priorSecret),
		ClientKind: "claude-code", SessionLabel: "vadim/laptop",
		MemoryRead: true, MemoryWrite: true,
		AllowedWorkflows: []string{"companion-architectural-review"},
		BudgetCapUSD:     &budget, RateLimitRPS: &rps, AllowPush: true,
	})
	srv := NewServer(WithAPIKeyRepository(repo))
	req := newKeysActionRequest(t, url.Values{"action": {"rotate"}, "key_id": {"akey-comp"}})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "companion-example")
	require.Empty(t, data.Error, "rotate must succeed (Error=%q)", data.Error)

	rows, _ := repo.ListByProject(context.Background(), "companion-example")
	var fresh *persistence.APIKey
	for _, r := range rows {
		if r.RevokedAt == nil {
			fresh = r
		}
	}
	require.NotNil(t, fresh, "a fresh active key must exist after rotate")
	assert.Equal(t, "claude-code", fresh.ClientKind, "companion ClientKind must survive rotation")
	assert.True(t, fresh.MemoryRead, "MemoryRead must survive rotation")
	assert.True(t, fresh.MemoryWrite, "MemoryWrite must survive rotation")
	assert.Equal(t, []string{"companion-architectural-review"}, fresh.AllowedWorkflows)
	assert.True(t, fresh.AllowPush, "AllowPush must survive rotation")
	require.NotNil(t, fresh.BudgetCapUSD)
	assert.Equal(t, 12.5, *fresh.BudgetCapUSD)
	require.NotNil(t, fresh.RateLimitRPS)
	assert.Equal(t, 5, *fresh.RateLimitRPS)
	assert.Equal(t, "vadim/laptop", fresh.SessionLabel)
	// Fresh identity — not the prior row resurfaced.
	assert.NotEqual(t, "akey-comp", fresh.ID)
	assert.Nil(t, fresh.RevokedAt)
	assert.Nil(t, fresh.LastUsedAt)
}

// TestHandleProjectKeysAction_RevokeMissingKeyIDErrors
func TestHandleProjectKeysAction_RevokeMissingKeyIDErrors(t *testing.T) {
	srv := NewServer(WithAPIKeyRepository(&uiMemAPIKeyRepo{}))
	req := newKeysActionRequest(t, url.Values{"action": {"revoke"}})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Equal(t, "key_id required", data.Error)
}

// TestHandleProjectKeysAction_RevokeKeyNotInProjectErrors — IDOR
// guard on the revoke path.
func TestHandleProjectKeysAction_RevokeKeyNotInProjectErrors(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-foreign", ProjectID: "p2", Name: "x", KeyHash: "x",
	})
	srv := NewServer(WithAPIKeyRepository(repo))
	req := newKeysActionRequest(t, url.Values{
		"action": {"revoke"},
		"key_id": {"akey-foreign"},
	})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Equal(t, "key not found in this project", data.Error)
}

// TestHandleProjectKeysAction_UnknownActionErrors — defensive
// catch-all on the form switch.
func TestHandleProjectKeysAction_UnknownActionErrors(t *testing.T) {
	srv := NewServer(WithAPIKeyRepository(&uiMemAPIKeyRepo{}))
	req := newKeysActionRequest(t, url.Values{"action": {"explode"}})
	data := &ProjectKeysData{}
	srv.handleProjectKeysAction(req, data, "p1")
	assert.Contains(t, data.Error, "unknown action explode")
}
