package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// stubRateLimitAPIKeyRepo is the minimal APIKeyRepository fake the
// ratelimit-status handler needs — ListByProject only. Other methods
// panic if hit so a future refactor can't silently exercise an
// unmocked path.
type stubRateLimitAPIKeyRepo struct {
	keys []*persistence.APIKey
	err  error
}

func (s *stubRateLimitAPIKeyRepo) Create(context.Context, *persistence.APIKey) error {
	panic("unexpected Create")
}
func (s *stubRateLimitAPIKeyRepo) LookupActiveByHash(context.Context, string) (*persistence.APIKey, error) {
	panic("unexpected LookupActiveByHash")
}
func (s *stubRateLimitAPIKeyRepo) ListByProject(_ context.Context, _ string) ([]*persistence.APIKey, error) {
	return s.keys, s.err
}
func (s *stubRateLimitAPIKeyRepo) ListCompanionByProject(context.Context, string) ([]*persistence.APIKey, error) {
	panic("unexpected ListCompanionByProject")
}
func (s *stubRateLimitAPIKeyRepo) TouchLastUsed(context.Context, string) error { return nil }
func (s *stubRateLimitAPIKeyRepo) Revoke(context.Context, string) error        { return nil }
func (s *stubRateLimitAPIKeyRepo) UpdateAllowedWorkflows(context.Context, string, []string) error {
	return nil
}
func (s *stubRateLimitAPIKeyRepo) UpdateAllowPush(context.Context, string, bool) error {
	return nil
}
func (s *stubRateLimitAPIKeyRepo) RevokeByName(context.Context, string) error { return nil }

func intPtr(v int) *int { return &v }

// makeRateLimitTestRegistry builds a registry with one project that
// has explicit per-minute / per-hour caps so the handler's headroom
// arithmetic has non-trivial input to render.
func makeRateLimitTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s.md"), []byte(`---
swarmId: "s"
roles:
  - name: "r"
    runtime:
      image: "x:latest"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "w.md"), []byte(`---
workflowId: "w"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "r"
    prompt: "x"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "p1.yaml"), []byte(`projectId: "p1"
displayName: "P1"
swarmId: "s"
defaultWorkflowId: "w"
rate_limit:
  tasks_per_minute: 5
  tasks_per_hour: 50
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

// TestGetProjectRateLimitStatus_RequiresGET — only GET allowed; POST
// surfaces 405 so the JSON-error envelope stays consistent with the
// rest of the data-plane handlers.
func TestGetProjectRateLimitStatus_RequiresGET(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestGetProjectRateLimitStatus_ValidationErrorOnMissingID — empty
// projectId returns 400 VALIDATION_ERROR; mirrors every other
// project handler in the package.
func TestGetProjectRateLimitStatus_ValidationErrorOnMissingID(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

// TestGetProjectRateLimitStatus_NotFoundWhenProjectMissing — the
// registry-bound path returns 404 when GetProject yields nil. Mirrors
// GetAutonomyEvaluationSummary's contract.
func TestGetProjectRateLimitStatus_NotFoundWhenProjectMissing(t *testing.T) {
	srv := NewServer(WithProjectRegistry(registry.New()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/ghost/ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestGetProjectRateLimitStatus_ZeroCollaboratorsRendersDefaults —
// with no limiter, no metrics, and no key repo wired, the handler
// must still return 200 with a well-formed response (zero counts,
// configured caps, empty keys slice).
func TestGetProjectRateLimitStatus_ZeroCollaboratorsRendersDefaults(t *testing.T) {
	reg := makeRateLimitTestRegistry(t)
	srv := NewServer(WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp rateLimitStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "p1", resp.ProjectID)
	assert.Equal(t, 5, resp.TaskCreation.MinuteCap)
	assert.Equal(t, 50, resp.TaskCreation.HourCap)
	assert.Equal(t, 0, resp.TaskCreation.MinuteCount)
	assert.Equal(t, 5, resp.TaskCreation.MinuteHeadroom)
	assert.Equal(t, 50, resp.TaskCreation.HourHeadroom)
	assert.Empty(t, resp.Keys)
	assert.Zero(t, resp.ProjectSummary.RecentWarns)
	assert.Equal(t, int(ratelimit.StatusWindow/time.Second), resp.WindowSeconds)
}

// TestGetProjectRateLimitStatus_LiveLimiterAndMetrics — wire the
// real limiter + APIKeyLimiter + metrics, drive a few warns and
// blocks, then assert the response carries the snapshot + event
// counts the operator UI panel consumes.
func TestGetProjectRateLimitStatus_LiveLimiterAndMetrics(t *testing.T) {
	reg := makeRateLimitTestRegistry(t)
	limiter := ratelimit.New()
	keyLimiter := ratelimit.NewAPIKeyLimiter()
	metrics := ratelimit.NewMetrics(prometheus.NewRegistry())

	now := time.Now()
	// 3 task creates inside the trailing minute on p1.
	limiter.Record("p1", now.Add(-30*time.Second))
	limiter.Record("p1", now.Add(-15*time.Second))
	limiter.Record("p1", now.Add(-5*time.Second))

	// Two API keys: one with limits + a hot bucket, one without
	// limits (legacy unlimited). Drive the hot one through Allow
	// so its bucket and event ring carry recent state.
	repo := &stubRateLimitAPIKeyRepo{keys: []*persistence.APIKey{
		{ID: "k-hot", ProjectID: "p1", Name: "hot", KeyPrefix: "sk-x.ab", RateLimitRPS: intPtr(2), RateLimitBurst: intPtr(3)},
		{ID: "k-cold", ProjectID: "p1", Name: "cold", KeyPrefix: "sk-x.cd"},
	}}
	// Burst 3 — consume all three tokens, then one more (block).
	keyLimiter.Allow("k-hot", 2, 3, now)
	keyLimiter.Allow("k-hot", 2, 3, now)
	keyLimiter.Allow("k-hot", 2, 3, now)
	d := keyLimiter.Allow("k-hot", 2, 3, now)
	require.True(t, d.Blocked, "fourth Allow on burst=3 must block")
	// Record the metric events so the response carries them.
	metrics.Observe(ratelimit.ScopeAPIKey, "k-hot", ratelimit.KeyDecision{Blocked: true, Warn: true})

	srv := NewServer(
		WithProjectRegistry(reg),
		WithRateLimiter(limiter),
		WithAPIKeyLimiter(keyLimiter),
		WithRateLimitMetrics(metrics),
		WithAPIKeyRepository(repo),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp rateLimitStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// Sliding-window snapshot reflects the 3 recorded creates.
	assert.Equal(t, 3, resp.TaskCreation.MinuteCount)
	assert.Equal(t, 2, resp.TaskCreation.MinuteHeadroom, "cap 5 − 3 = 2")
	assert.Equal(t, 3, resp.TaskCreation.HourCount)
	assert.Equal(t, 47, resp.TaskCreation.HourHeadroom)

	// Per-key payload: hot key drained, cold key shows nominal
	// zero limits (legacy unlimited).
	require.Len(t, resp.Keys, 2)
	keysByID := map[string]apiKeyStatus{}
	for _, k := range resp.Keys {
		keysByID[k.KeyID] = k
	}
	hot, ok := keysByID["k-hot"]
	require.True(t, ok)
	assert.Equal(t, 2, hot.RateLimitRPS)
	assert.Equal(t, 3, hot.RateLimitBurst)
	require.NotNil(t, hot.TokensRemaining)
	// Bucket fully consumed but no time has passed between the 4
	// Allow calls — tokens land at zero (clamped).
	assert.InDelta(t, 0.0, *hot.TokensRemaining, 0.01)
	assert.Equal(t, 1, hot.Summary.RecentBlocks)
	assert.Equal(t, 1, hot.Summary.RecentWarns)
	require.NotNil(t, hot.Summary.Last429At)

	cold, ok := keysByID["k-cold"]
	require.True(t, ok)
	assert.Zero(t, cold.RateLimitRPS)
	assert.Nil(t, cold.TokensRemaining, "cold key has no allocated bucket")
}

// TestGetProjectRateLimitStatus_RevokedKeysOmitted — the panel
// must not surface revoked keys even though ListByProject returns
// them, mirroring buildRateLimitPanel's filter.
func TestGetProjectRateLimitStatus_RevokedKeysOmitted(t *testing.T) {
	reg := makeRateLimitTestRegistry(t)
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	repo := &stubRateLimitAPIKeyRepo{keys: []*persistence.APIKey{
		{ID: "active", ProjectID: "p1", Name: "active", KeyPrefix: "sk-a"},
		{ID: "revoked", ProjectID: "p1", Name: "revoked", KeyPrefix: "sk-r", RevokedAt: &past},
		{ID: "expired", ProjectID: "p1", Name: "expired", KeyPrefix: "sk-e", ExpiresAt: &past},
	}}
	srv := NewServer(WithProjectRegistry(reg), WithAPIKeyRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp rateLimitStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Keys, 1)
	assert.Equal(t, "active", resp.Keys[0].KeyID)
}

// TestGetProjectRateLimitStatus_RepoErrorDegradesGracefully — a
// repo failure surfaces as an empty Keys slice rather than a 500.
// The rest of the response (per-project counts, project summary)
// stays intact so the homepage panel keeps rendering.
func TestGetProjectRateLimitStatus_RepoErrorDegradesGracefully(t *testing.T) {
	reg := makeRateLimitTestRegistry(t)
	repo := &stubRateLimitAPIKeyRepo{err: errors.New("db down")}
	srv := NewServer(WithProjectRegistry(reg), WithAPIKeyRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/ratelimit-status", nil)
	rec := httptest.NewRecorder()
	srv.GetProjectRateLimitStatus(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp rateLimitStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Keys)
}

// TestHeadroom_UnlimitedRendersNegativeOne — cap == 0 means "no
// limit configured" and the JSON field is -1 so the UI template
// can render "unlimited" rather than "0 remaining".
func TestHeadroom_UnlimitedRendersNegativeOne(t *testing.T) {
	assert.Equal(t, -1, headroom(42, 0))
	assert.Equal(t, 5, headroom(5, 10))
	assert.Equal(t, 0, headroom(20, 10), "over-cap clamps at zero")
}

// TestBuildTaskCreationStatus_NoLimiter — nil limiter renders zero
// counts but still echoes the configured caps so the UI can
// render "0 / 5 this minute" without the daemon's limiter wired.
func TestBuildTaskCreationStatus_NoLimiter(t *testing.T) {
	tc := buildTaskCreationStatus(nil, "p1", 5, 50, time.Now())
	assert.Equal(t, 5, tc.MinuteCap)
	assert.Equal(t, 5, tc.MinuteHeadroom)
	assert.Equal(t, 0, tc.MinuteCount)
}
