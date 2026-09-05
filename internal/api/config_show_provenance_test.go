package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// The provenance view redacts by the same key rule as the plain dump and keeps
// the origin and source beside the redacted value — the variable NAME that
// supplied a password is not a secret (resolved-config provenance design §4.3).
func TestGetConfig_ProvenanceRedactsByKeyAndKeepsOrigin(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Database.Password = "hunter2"
	cfg.Database.Host = "db.internal"
	prov := &config.Provenance{Path: "/etc/vornik/config.yaml", Values: map[string]config.ValueOrigin{
		"database.password": {Origin: config.OriginEnv, Source: "VORNIK_DATABASE_PASSWORD"},
		"database.host":     {Origin: config.OriginFile, Source: "config.yaml"},
		"gateway.address":   {Origin: config.OriginUnset},
	}}
	var holder config.SnapshotHolder
	holder.Store(cfg, prov)
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(&config.Config{}), WithConfigSnapshot(&holder))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config?provenance=true", nil))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var view ProvenanceView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	assert.Equal(t, "/etc/vornik/config.yaml", view.ConfigPath)
	pw := view.Values["database.password"]
	assert.NotEqual(t, "hunter2", pw.Value, "the password value must be redacted")
	assert.Equal(t, config.OriginEnv, pw.Origin)
	assert.Equal(t, "VORNIK_DATABASE_PASSWORD", pw.Source, "the origin survives redaction")
	assert.Equal(t, "db.internal", view.Values["database.host"].Value)
	assert.Equal(t, config.OriginUnset, view.Values["gateway.address"].Origin)
	assert.NotContains(t, rec.Body.String(), "hunter2")
}

// The dump is served from the reload-written snapshot, not the boot-time
// pointer: before 2026-09-03 `config show` after a hot reload showed the
// config the daemon was no longer running, and nothing said so.
func TestGetConfig_ServesTheSnapshotNotTheBootPointer(t *testing.T) {
	boot := config.DefaultConfig()
	boot.Logging.Level = "boot"
	reloaded := config.DefaultConfig()
	reloaded.Logging.Level = "reloaded"
	var holder config.SnapshotHolder
	holder.Store(reloaded, nil)
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(boot), WithConfigSnapshot(&holder))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// The plain dump keeps its shape (json tags where present, Go names
	// otherwise — a pre-existing property of this surface), so assert on the
	// value rather than on a key path.
	assert.Contains(t, rec.Body.String(), `"reloaded"`)
	assert.NotContains(t, rec.Body.String(), `"boot"`)

	// No provenance recorded → an honest 503, not an empty map.
	req = authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config?provenance=1", nil))
	rec = httptest.NewRecorder()
	server.GetConfig(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "PROVENANCE_UNAVAILABLE")
}

// `?trees=true` returns the registry's whole-tree index — which file supplied
// each object and which files the loader refused — from the LIVE registry the
// per-object commands read (design §4.2).
func TestGetConfig_TreesListSourcesAndRejectedFiles(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p1.yaml"),
		[]byte("projectId: p1\ndisplayName: p1\nswarmId: s1\ndefaultWorkflowId: w1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "typo.yaml"),
		[]byte("projectId: typo\ndisplayName: t\nswarmId: s1\ndefaultWorkflowId: w1\nforge:\n  mention_handel: x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "s1.md"),
		[]byte("---\nswarmId: s1\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "w1.md"),
		[]byte("---\nworkflowId: w1\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))

	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(config.DefaultConfig()), WithProjectRegistry(reg))
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config?trees=true", nil))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var view ProvenanceView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.NotNil(t, view.Trees)
	assert.Empty(t, view.Values, "trees alone carries no per-key values")
	var sawProject, sawWorkflow bool
	for _, s := range view.Trees.Sources {
		if s.Kind == "project" && s.ID == "p1" && s.Path == filepath.Join("projects", "p1.yaml") {
			sawProject = true
		}
		if s.Kind == "workflow" && s.ID == "w1" {
			sawWorkflow = true
		}
	}
	assert.True(t, sawProject && sawWorkflow, "sources: %+v", view.Trees.Sources)
	require.Len(t, view.Trees.Rejected, 1)
	assert.Contains(t, view.Trees.Rejected[0].Error, "mention_handel")
}
