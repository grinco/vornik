package api

// Regression tests, incident 2026-06-07: the operator's admin key —
// listed in admin.allowed_keys but ALSO present as a DB api_keys row
// bound to project "assistant" (DB lookup takes precedence over the
// static map) — was stamped projectIDKey=["assistant"] like any
// scoped key, so requestAllowsProject denied it on every other
// project ("Access denied to project" on a companion-example task's
// route). Third recurrence of the "missing admin-class bypass" class
// (reminders API + reminders UI were f16ae834). The centralized fix
// lives at stamp time: an admin-class key stamps NO project
// restriction, the exact rule session-admins already get
// (Principal.Projects ["*"] → stamp nothing), so the route gate and
// every row-level filter inherit the bypass from one place.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// adminClassAuthConfigs returns the chain-path AuthConfig variant. The legacy
// inline path was retired once use_chain became unconditional, so the former
// inline/chain equivalence pin collapses to the single surviving path.
func adminClassAuthConfigs(lookup APIKeyLookup, adminKey string) map[string]AuthConfig {
	adminCfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey}}
	return map[string]AuthConfig{"chain": {
		Enabled:         true,
		APIKeyLookup:    lookup,
		AdminKeyChecker: adminCfg.IsAdminKey,
	}}
}

func TestAuthMiddleware_AdminClassDBKey_NotProjectScoped(t *testing.T) {
	key, err := apikey.Generate("assistant")
	require.NoError(t, err)
	row := &persistence.APIKey{
		ID:        "akey-admin",
		ProjectID: "assistant",
		Name:      "memetic-admin",
		KeyHash:   apikey.Hash(key),
		KeyPrefix: apikey.DisplayPrefix(key),
		CreatedAt: time.Now(),
	}

	for name, cfg := range adminClassAuthConfigs(&stubAPIKeyLookup{row: row}, key) {
		t.Run(name, func(t *testing.T) {
			mw := AuthMiddleware(cfg)

			var ctxProjectList []string
			var hadProjectList, allowsForeign bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxProjectList, hadProjectList = r.Context().Value(projectIDKey).([]string)
				allowsForeign = requestAllowsProject(r, "companion-example")
				w.WriteHeader(http.StatusOK)
			})

			req := newAuthRequest(t, "/api/v1/projects/companion-example/tasks/t1")
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.False(t, hadProjectList,
				"admin-class key must stamp NO project restriction (got %v) — same rule as session-admins", ctxProjectList)
			assert.True(t, allowsForeign,
				"admin-class key must pass the project gate for every project")
		})
	}
}

// Guard: a DB key NOT in admin.allowed_keys keeps its single-project
// scope — the bypass must not widen ordinary keys.
func TestAuthMiddleware_NonAdminDBKey_StaysScoped(t *testing.T) {
	key, err := apikey.Generate("assistant")
	require.NoError(t, err)
	row := &persistence.APIKey{
		ID:        "akey-plain",
		ProjectID: "assistant",
		Name:      "ha-key",
		KeyHash:   apikey.Hash(key),
		KeyPrefix: apikey.DisplayPrefix(key),
		CreatedAt: time.Now(),
	}

	for name, cfg := range adminClassAuthConfigs(&stubAPIKeyLookup{row: row}, "sk-some-other-admin-key") {
		t.Run(name, func(t *testing.T) {
			mw := AuthMiddleware(cfg)

			var ctxProjectList []string
			var allowsForeign, allowsOwn bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxProjectList, _ = r.Context().Value(projectIDKey).([]string)
				allowsForeign = requestAllowsProject(r, "companion-example")
				allowsOwn = requestAllowsProject(r, "assistant")
				w.WriteHeader(http.StatusOK)
			})

			req := newAuthRequest(t, "/api/v1/projects/assistant/tasks/t1")
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			mw(handler).ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, []string{"assistant"}, ctxProjectList,
				"non-admin DB key keeps its bound-project stamp")
			assert.True(t, allowsOwn)
			assert.False(t, allowsForeign,
				"non-admin key must NOT inherit the admin bypass")
		})
	}
}

// BuildAuthConfig must wire the admin checker from cfg.Admin so both
// the api router and the UI subtree (same builder) inherit the
// bypass without per-call-site plumbing — the centralization that
// feedback rule "3× same bug class → extract a primitive" demands.
func TestBuildAuthConfig_WiresAdminKeyChecker(t *testing.T) {
	cfg := &config.Config{}
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}
	out := BuildAuthConfig(cfg)
	require.NotNil(t, out.AdminKeyChecker)
	assert.True(t, out.AdminKeyChecker("sk-admin"))
	assert.False(t, out.AdminKeyChecker("sk-not-admin"))

	// Admin gate disabled → no checker → no bypass anywhere.
	cfg.Admin.Enabled = false
	assert.Nil(t, BuildAuthConfig(cfg).AdminKeyChecker)
}
