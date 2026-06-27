package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/registry"
)

func TestProjectConfigSaveValidatesBeforeWriting(t *testing.T) {
	root := writeUIRegistryFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &mockConfigReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	path := filepath.Join(root, "projects", "project-1.yaml")
	require.NoError(t, os.Chmod(path, 0o600))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := url.Values{}
	form.Set("content", strings.Replace(string(before), "swarmId: swarm-1", "swarmId: missing-swarm", 1))
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/project-1/config", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.ProjectConfigSave(rec, req, "project-1")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.Equal(t, 0, reloader.calls)
	assert.Contains(t, rec.Body.String(), "Validation failed")
	backups, err := filepath.Glob(filepath.Join(root, "projects", "project-1.yaml.bak-*"))
	require.NoError(t, err)
	assert.Empty(t, backups)
}

func TestProjectConfigSaveWritesAndReloads(t *testing.T) {
	root := writeUIRegistryFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &mockConfigReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	path := filepath.Join(root, "projects", "project-1.yaml")
	require.NoError(t, os.Chmod(path, 0o600))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	updated := strings.Replace(string(before), "displayName: Project One", "displayName: Renamed Project", 1)

	form := url.Values{}
	form.Set("content", updated)
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/project-1/config", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.ProjectConfigSave(rec, req, "project-1")

	assert.Equal(t, http.StatusOK, rec.Code)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, updated, string(after))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, 1, reloader.calls)
	assert.Contains(t, rec.Body.String(), "saved and reloaded")
	backups, err := filepath.Glob(filepath.Join(root, "projects", "project-1.yaml.bak-*"))
	require.NoError(t, err)
	require.Len(t, backups, 1)
	backupInfo, err := os.Stat(backups[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), backupInfo.Mode().Perm())
	backup, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, string(before), string(backup))
}

// TestProjectConfigSave_D2_ScopedUserDenied is the regression test for
// audit finding D2 (2026-06-10): a project-scoped RoleUser browser
// session (auth ON, NOT admin) could rewrite project YAML — autonomy
// gates, tool allowlists, rate limits. ProjectConfigSave now requires
// admin scope and 403s a non-admin session WITHOUT touching the file.
// Fails pre-fix (the YAML was rewritten + 200/reload), passes post-fix.
func TestProjectConfigSave_D2_ScopedUserDenied(t *testing.T) {
	root := writeUIRegistryFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &mockConfigReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	path := filepath.Join(root, "projects", "project-1.yaml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	updated := strings.Replace(string(before), "displayName: Project One", "displayName: Hijacked", 1)

	form := url.Values{}
	form.Set("content", updated)
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/config", strings.NewReader(form.Encode()))
	req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "project-1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.ProjectConfigSave(rec, req, "project-1")

	assert.Equal(t, http.StatusForbidden, rec.Code)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "non-admin session must not rewrite the YAML")
	assert.Equal(t, 0, reloader.calls)
}

// TestProjectConfigSave_D2_AuthOffAllowed — single-tenant (auth off) is
// implicitly trusted and may rewrite project YAML.
func TestProjectConfigSave_D2_AuthOffAllowed(t *testing.T) {
	root := writeUIRegistryFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &mockConfigReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	path := filepath.Join(root, "projects", "project-1.yaml")
	require.NoError(t, os.Chmod(path, 0o600))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	updated := strings.Replace(string(before), "displayName: Project One", "displayName: Homelab", 1)

	form := url.Values{}
	form.Set("content", updated)
	req := authOffUIRequest(http.MethodPost, "/projects/project-1/config")
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.ContentLength = int64(len(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	server.ProjectConfigSave(rec, req, "project-1")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, reloader.calls)
}

type mockConfigReloader struct {
	calls int
}

func (m *mockConfigReloader) Reload() error {
	m.calls++
	return nil
}

func writeUIRegistryFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project-1.yaml"), []byte(`
id: project-1
projectId: project-1
displayName: Project One
swarmId: swarm-1
defaultWorkflowId: workflow-1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-1.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: coder
    model: test-model
    systemPrompt: code
    runtime:
      image: test-image
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "workflow-1.md"), []byte(`---
workflowId: workflow-1
entrypoint: build
steps:
  build:
    type: agent
    prompt: "do work"
    role: coder
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	return root
}
