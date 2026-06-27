package api

// Coverage tests for the memory-firewall admin handlers that the
// existing memory_firewall_chunk_policy_test.go leaves uncovered:
//   - AdminMemoryFirewallEvaluations (JSON list: filters, defaults,
//     503/400/500 branches)
//   - AdminMemoryFirewallMode (daemon + per-project override)
//   - the WithMemoryFirewallMode / WithMemoryFirewallProjectModeFn
//     server options
//   - error branches in the CSV + chunk-policy handlers
//
// Reuses the stubEvaluationsRepo / stubFirewallEditor / newFirewallServer
// / withAdminKeyContext / ptr helpers from the sibling test files.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// fwCovErrRepo lets a test force ListRecent to fail so the 500
// branch of the JSON + CSV handlers is exercised.
type fwCovErrRepo struct {
	stubEvaluationsRepo
	listErr error
}

func (r *fwCovErrRepo) ListRecent(_ context.Context, _, _ string, _ time.Time, _ int) ([]memoryfirewall.EvaluationRow, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.rows, nil
}

func fwCovServer(repo persistence.MemoryPolicyEvaluationRepository) *Server {
	return NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryPolicyEvaluations(repo),
	)
}

func TestAdminMemoryFirewallEvaluations_NoRepo_503(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}))
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/evaluations?project_id=p1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluations(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "FIREWALL_DISABLED")
}

func TestAdminMemoryFirewallEvaluations_WrongMethod_405(t *testing.T) {
	s := fwCovServer(&stubEvaluationsRepo{})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/memory/policy/evaluations?project_id=p1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluations(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestAdminMemoryFirewallEvaluations_MissingProject_400(t *testing.T) {
	s := fwCovServer(&stubEvaluationsRepo{})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/evaluations", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluations(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "project_id query param required")
}

func TestAdminMemoryFirewallEvaluations_ListError_500(t *testing.T) {
	repo := &fwCovErrRepo{listErr: errors.New("db down")}
	s := fwCovServer(repo)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/evaluations?project_id=p1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluations(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "db down")
}

// Exercises the happy path including the decision filter, an
// explicit RFC3339 since, and a custom limit — covers the query
// parsing branches.
func TestAdminMemoryFirewallEvaluations_OK_WithFilters(t *testing.T) {
	repo := &stubEvaluationsRepo{
		rows: []memoryfirewall.EvaluationRow{
			{ID: "ev1", ProjectID: "p1", ChunkID: "c1", Decision: memoryfirewall.DecisionAllow, PolicyDigest: "d1"},
		},
	}
	s := fwCovServer(repo)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/memory/policy/evaluations?project_id=p1&decision=allow&limit=25&since=2026-01-01T00:00:00Z",
			nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluations(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		Count   int `json:"count"`
		Filters struct {
			ProjectID string `json:"project_id"`
			Decision  string `json:"decision"`
			Limit     int    `json:"limit"`
		} `json:"filters"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 1, out.Count)
	assert.Equal(t, "p1", out.Filters.ProjectID)
	assert.Equal(t, "allow", out.Filters.Decision)
	assert.Equal(t, 25, out.Filters.Limit)
}

// since=YYYY-MM-DD (the vornikctl CLI convention) + an
// out-of-range limit (ignored, falls back to default 50).
func TestAdminMemoryFirewallEvaluations_DateOnlySince_DefaultsLimit(t *testing.T) {
	repo := &stubEvaluationsRepo{}
	s := fwCovServer(repo)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/memory/policy/evaluations?project_id=p1&since=2026-02-15&limit=99999",
			nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluations(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		Filters struct {
			Limit int `json:"limit"`
		} `json:"filters"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 50, out.Filters.Limit, "out-of-range limit falls back to default")
}

func TestAdminMemoryFirewallMode_WrongMethod_405(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}))
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodDelete, "/api/v1/admin/memory/policy/mode", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallMode(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// No mode option wired → reports "unknown".
func TestAdminMemoryFirewallMode_Unknown(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}))
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/mode", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallMode(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		Mode       string `json:"mode"`
		DaemonMode string `json:"daemon_mode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "unknown", out.Mode)
	assert.Equal(t, "unknown", out.DaemonMode)
}

// WithMemoryFirewallMode wired → daemon mode reported.
func TestAdminMemoryFirewallMode_DaemonModeReported(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryFirewallMode(memoryfirewall.EnforcementEnforce),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/mode", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallMode(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		Mode       string `json:"mode"`
		DaemonMode string `json:"daemon_mode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, string(memoryfirewall.EnforcementEnforce), out.DaemonMode)
	assert.Equal(t, string(memoryfirewall.EnforcementEnforce), out.Mode)
}

// Per-project override resolves to a concrete mode → effective mode
// is the project mode.
func TestAdminMemoryFirewallMode_ProjectOverride_Found(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryFirewallMode(memoryfirewall.EnforcementAdvisory),
		WithMemoryFirewallProjectModeFn(func(projectID string) (memoryfirewall.EnforcementMode, bool) {
			if projectID == "p1" {
				return memoryfirewall.EnforcementEnforce, true
			}
			return "", false
		}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/mode?project_id=p1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallMode(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		Mode        string `json:"mode"`
		DaemonMode  string `json:"daemon_mode"`
		ProjectID   string `json:"project_id"`
		ProjectMode string `json:"project_mode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "p1", out.ProjectID)
	assert.Equal(t, string(memoryfirewall.EnforcementEnforce), out.ProjectMode)
	assert.Equal(t, string(memoryfirewall.EnforcementEnforce), out.Mode, "effective mode = project override")
	assert.Equal(t, string(memoryfirewall.EnforcementAdvisory), out.DaemonMode, "daemon mode unchanged")
}

// Per-project override returns ok=false → project_mode is empty
// (inherit).
func TestAdminMemoryFirewallMode_ProjectOverride_Inherit(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithMemoryFirewallMode(memoryfirewall.EnforcementAdvisory),
		WithMemoryFirewallProjectModeFn(func(string) (memoryfirewall.EnforcementMode, bool) {
			return "", false
		}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/mode?project_id=other", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallMode(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out struct {
		ProjectID   string `json:"project_id"`
		ProjectMode string `json:"project_mode"`
		Mode        string `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "other", out.ProjectID)
	assert.Equal(t, "", out.ProjectMode)
	assert.Equal(t, string(memoryfirewall.EnforcementAdvisory), out.Mode, "inherits daemon mode")
}

// CSV: 503 when repo not wired.
func TestAdminMemoryFirewallEvaluationsCSV_NoRepo_503(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}))
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/evaluations.csv?project_id=p1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsCSV(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// CSV: 400 when project_id missing.
func TestAdminMemoryFirewallEvaluationsCSV_MissingProject_400(t *testing.T) {
	s := fwCovServer(&stubEvaluationsRepo{})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/evaluations.csv", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsCSV(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// CSV: ListRecent error → 500.
func TestAdminMemoryFirewallEvaluationsCSV_ListError_500(t *testing.T) {
	repo := &fwCovErrRepo{listErr: errors.New("boom")}
	s := fwCovServer(repo)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/evaluations.csv?project_id=p1&since=2026-01-01&limit=5", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsCSV(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// CSV: wrong method → 405.
func TestAdminMemoryFirewallEvaluationsCSV_WrongMethod_405(t *testing.T) {
	s := fwCovServer(&stubEvaluationsRepo{})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/admin/memory/policy/evaluations.csv?project_id=p1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallEvaluationsCSV(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ChunkPolicy: malformed JSON body → 400.
func TestAdminMemoryFirewallChunkPolicy_BadJSON_400(t *testing.T) {
	s := newFirewallServer(&stubFirewallEditor{
		existing: map[string]ChunkPolicyRow{"c1": {ChunkID: "c1"}},
	})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/c1",
			bytes.NewBufferString(`{not json`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid JSON")
}

// ChunkPolicy: wrong method → 405.
func TestAdminMemoryFirewallChunkPolicy_WrongMethod_405(t *testing.T) {
	s := newFirewallServer(&stubFirewallEditor{})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/memory/policy/chunks/c1", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ChunkPolicy: LoadChunkPolicies error → 500.
func TestAdminMemoryFirewallChunkPolicy_LoadError_500(t *testing.T) {
	s := newFirewallServer(&stubFirewallEditor{loadErr: errors.New("load fail")})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/c1",
			bytes.NewBufferString(`{}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "load fail")
}

// ChunkPolicy: UpdateChunkPolicy error → 500. Also exercises the
// full set of partial-update assignment branches.
func TestAdminMemoryFirewallChunkPolicy_UpdateError_500_AllFields(t *testing.T) {
	s := newFirewallServer(&stubFirewallEditor{
		existing:  map[string]ChunkPolicyRow{"c1": {ChunkID: "c1"}},
		updateErr: errors.New("update fail"),
	})
	exp := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	purposes := []string{"operational"}
	roles := []string{"coder"}
	body, _ := json.Marshal(chunkPolicyUpdateRequest{
		TenantID:           ptr("tenant-z"),
		SensitivityTier:    ptr("restricted"),
		ProvenanceSource:   ptr("user_upload"),
		ProvenanceProducer: ptr("op-1"),
		ProvenanceTrust:    ptr(3),
		ProvenanceURL:      ptr("https://example.com"),
		FirewallExpiresAt:  &exp,
		PermittedRoles:     &roles,
		AllowedPurposes:    &purposes,
	})
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/memory/policy/chunks/c1",
			bytes.NewBuffer(body)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminMemoryFirewallChunkPolicy(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "update fail")
}
