package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// newCompanionServer builds a server primed for companion-handler
// tests: an in-memory APIKey repo, a project registry seeded by
// seedRegistry (alpha + beta projects, wf-alpha + wf-beta workflows),
// and a no-op logger. Auth is disabled in the request context (via
// withAuthDisabled) so the admin gate's "single-operator local
// install" branch lets the call through without an admin key.
func newCompanionServer(t *testing.T) (*Server, *memAPIKeyRepo) {
	t.Helper()
	repo := &memAPIKeyRepo{}
	srv := &Server{
		logger:          zerolog.Nop(),
		apiKeyRepo:      repo,
		projectRegistry: seedRegistry(t),
	}
	return srv, repo
}

// withAuthDisabled wraps a request with the auth-disabled context
// flag. requireAdminGate then short-circuits to "allow" — the local
// single-operator semantic the rest of the daemon uses.
func withAuthDisabled(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
}

func TestCompanionGrant_HappyPath_MintsScopedKey(t *testing.T) {
	srv, repo := newCompanionServer(t)
	cap := 25.0
	body := companionGrantRequest{
		ProjectID:        "alpha",
		SessionLabel:     "vadim/laptop",
		ClientKind:       "claude-code",
		AllowedWorkflows: []string{"wf-alpha"},
		BudgetCapUSD:     &cap,
	}
	raw, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var resp companionGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "alpha", resp.ProjectID)
	assert.Equal(t, "claude-code", resp.ClientKind)
	assert.Equal(t, "vadim/laptop", resp.SessionLabel)
	assert.Equal(t, []string{"wf-alpha"}, resp.AllowedWorkflows)
	require.NotNil(t, resp.BudgetCapUSD)
	assert.Equal(t, 25.0, *resp.BudgetCapUSD)
	assert.True(t, strings.HasPrefix(resp.Secret, "sk-vornik-"+apikey.ShortProjectTag("alpha")+"."),
		"secret format: got %q", resp.Secret)
	assert.NotContains(t, resp.Secret, "alpha", "secret must not leak the raw project name")
	assert.Equal(t, resp.Secret[:apikey.PrefixDisplayLen], resp.KeyPrefix)

	// Exactly one row persisted; hash matches the returned secret.
	require.Len(t, repo.rows, 1)
	stored := repo.rows[0]
	assert.Equal(t, apikey.Hash(resp.Secret), stored.KeyHash,
		"stored hash must match returned secret's hash")
	assert.Equal(t, "claude-code", stored.ClientKind)
	require.NotNil(t, stored.BudgetCapUSD)
	assert.Equal(t, 25.0, *stored.BudgetCapUSD)
}

func TestCompanionGrant_RejectsMissingClientKind(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"alpha"}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "clientKind is required")
}

func TestCompanionGrant_RejectsUnknownClientKind(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"alpha","clientKind":"claude-cli"}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNKNOWN_CLIENT",
		"typo'd clientKind (claude-cli instead of claude-code) must fail loudly")
}

func TestCompanionGrant_RejectsUnknownProject(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"does-not-exist","clientKind":"claude-code"}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "PROJECT_NOT_FOUND")
}

func TestCompanionGrant_RejectsEmptyAllowedWorkflows(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"alpha","clientKind":"claude-code","allowedWorkflows":[]}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"empty allowedWorkflows is a footgun — must error rather than mint a useless key")
	assert.Contains(t, rec.Body.String(), "omit the field entirely")
}

func TestCompanionGrant_RejectsUnknownWorkflow(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"alpha","clientKind":"claude-code","allowedWorkflows":["does-not-exist"]}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNKNOWN_WORKFLOW",
		"workflow id must resolve at grant time, not at first delegate() call")
}

func TestCompanionGrant_RejectsDuplicateWorkflow(t *testing.T) {
	srv, _ := newCompanionServer(t)
	body := `{"projectId":"alpha","clientKind":"claude-code","allowedWorkflows":["wf-alpha","wf-alpha"]}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "duplicate")
}

func TestCompanionGrant_RejectsNonPositiveBudget(t *testing.T) {
	srv, _ := newCompanionServer(t)
	for _, raw := range []string{
		`{"projectId":"alpha","clientKind":"claude-code","budgetCapUsd":0}`,
		`{"projectId":"alpha","clientKind":"claude-code","budgetCapUsd":-5}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(raw))
		rec := httptest.NewRecorder()
		srv.CompanionGrant(rec, withAuthDisabled(req))

		assert.Equalf(t, http.StatusBadRequest, rec.Code, "input: %s", raw)
		// JSON encoder escapes '>' to > in the message field;
		// match on the part of the message that survives encoding.
		assert.Containsf(t, rec.Body.String(), "budgetCapUsd must be",
			"input: %s", raw)
		assert.Containsf(t, rec.Body.String(), "VALIDATION_ERROR",
			"input: %s", raw)
	}
}

func TestCompanionGrant_DefaultsName_WhenSessionLabelOmitted(t *testing.T) {
	srv, repo := newCompanionServer(t)
	body := `{"projectId":"alpha","clientKind":"claude-code"}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/companion/grant", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Len(t, repo.rows, 1)
	// Name fallback shape: "companion-<client>-<YYYYMMDD-HHMMSS>".
	assert.Truef(t, strings.HasPrefix(repo.rows[0].Name, "companion-claude-code-"),
		"fallback name should be companion-claude-code-<timestamp>, got %q", repo.rows[0].Name)
}

func TestCompanionGrant_RejectsWrongMethod(t *testing.T) {
	srv, _ := newCompanionServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/companion/grant", nil)
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, withAuthDisabled(req))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestCompanionKeysList_FiltersToCompanionRows — only rows with a
// non-empty ClientKind appear, regardless of how many other keys
// the project has. The companion admin view never leaks legacy
// non-companion keys into its rendering.
func TestCompanionKeysList_FiltersToCompanionRows(t *testing.T) {
	srv, repo := newCompanionServer(t)
	// One companion key, one legacy key; both for project "alpha".
	cap := 10.0
	require.NoError(t, repo.Create(context.Background(), &persistence.APIKey{
		ID: "k-legacy", ProjectID: "alpha", Name: "legacy-key",
		KeyHash: "h1", KeyPrefix: "sk-vornik-al",
	}))
	require.NoError(t, repo.Create(context.Background(), &persistence.APIKey{
		ID: "k-co", ProjectID: "alpha", Name: "session-1",
		KeyHash: "h2", KeyPrefix: "sk-vornik-al",
		ClientKind: "claude-code", SessionLabel: "vadim/laptop",
		AllowedWorkflows: []string{"wf-alpha"}, BudgetCapUSD: &cap,
	}))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/companion/keys?projectId=alpha", nil)
	rec := httptest.NewRecorder()
	srv.CompanionKeysList(rec, withAuthDisabled(req))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp companionKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Keys, 1, "legacy non-companion key must not surface")
	assert.Equal(t, "k-co", resp.Keys[0].ID)
	assert.Equal(t, "claude-code", resp.Keys[0].ClientKind)
	assert.Equal(t, []string{"wf-alpha"}, resp.Keys[0].AllowedWorkflows)
}

func TestCompanionKeysList_RejectsMissingProjectId(t *testing.T) {
	srv, _ := newCompanionServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/companion/keys", nil)
	rec := httptest.NewRecorder()
	srv.CompanionKeysList(rec, withAuthDisabled(req))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "projectId query parameter is required")
}
