package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/templates"
)

// templateRig builds a Server wired with a fresh catalog and a
// temp configs dir for materialisation. Returns the server + the
// dir so per-test assertions can read what got written.
func templateRig(t *testing.T) (*Server, *templates.Catalog, string, string) {
	t.Helper()

	// Templates dir + a minimal manifest with one source file.
	tplDir := filepath.Join(t.TempDir(), "tpl")
	require.NoError(t, os.MkdirAll(filepath.Join(tplDir, "demo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "demo", "template.yaml"), []byte(`
displayName: "Demo"
description: "Test template"
domain: "general"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z][a-z0-9-]{1,20}[a-z0-9]"}
  - {name: greeting, type: string, label: "Greet", default: "hello"}
files:
  - {source: project.yaml.tmpl, target: "projects/{{.projectId}}.yaml"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "demo", "project.yaml.tmpl"),
		[]byte("projectId: {{.projectId}}\ngreeting: {{.greeting}}\n"), 0o644))

	cat, err := templates.Load(tplDir)
	require.NoError(t, err)

	// Configs dir for materialisation output.
	configsDir := t.TempDir()
	srv := &Server{
		projectTemplates: cat,
		configsDir:       configsDir,
		adminConfig:      config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}},
	}
	return srv, cat, tplDir, configsDir
}

func templateAdminReq(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-admin"))
}

func TestListProjectTemplates_HappyPath(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/project-templates", nil)
	rec := httptest.NewRecorder()
	srv.ListProjectTemplates(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Templates []projectTemplateSummary `json:"templates"`
		Total     int                      `json:"total"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, 1, resp.Total)
	require.Len(t, resp.Templates, 1)
	assert.Equal(t, "demo", resp.Templates[0].Slug)
	assert.Equal(t, "Demo", resp.Templates[0].DisplayName)
	require.Len(t, resp.Templates[0].Parameters, 2, "both manifest params should surface in the payload")
	assert.Equal(t, "projectId", resp.Templates[0].Parameters[0].Name)
}

func TestListProjectTemplates_NotConfigured(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/project-templates", nil)
	rec := httptest.NewRecorder()
	srv.ListProjectTemplates(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"missing catalog must return 503 so operators know to install templates rather than seeing an empty 200")
	assert.Contains(t, rec.Body.String(), "TEMPLATES_NOT_CONFIGURED")
}

func TestListProjectTemplates_MethodGuard(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/project-templates", nil)
	rec := httptest.NewRecorder()
	srv.ListProjectTemplates(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestCreateProjectFromTemplate_HappyPath(t *testing.T) {
	srv, _, _, configsDir := templateRig(t)

	body, _ := json.Marshal(map[string]any{
		"slug": "demo",
		"parameters": map[string]string{
			"projectId": "my-project",
			"greeting":  "hi",
		},
	})
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	var resp createProjectFromTemplateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "demo", resp.Slug)
	require.Len(t, resp.FilesWritten, 1)
	assert.Equal(t, "projects/my-project.yaml", resp.FilesWritten[0])

	// File actually exists on disk with substituted content.
	got, rerr := os.ReadFile(filepath.Join(configsDir, "projects", "my-project.yaml"))
	require.NoError(t, rerr)
	assert.Equal(t, "projectId: my-project\ngreeting: hi\n", string(got))
}

func TestCreateProjectFromTemplate_RequiresAdmin(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"demo","parameters":{"projectId":"x"}}`)))
	req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-project"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestCreateProjectFromTemplate_NotConfigured(t *testing.T) {
	srv := &Server{adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}} // no catalog
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"demo","parameters":{}}`))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "TEMPLATES_NOT_CONFIGURED")
}

func TestCreateProjectFromTemplate_NoConfigsDir(t *testing.T) {
	// Catalog wired but configsDir empty — guards against a
	// misconfigured deployment writing files into the daemon's
	// CWD silently.
	srv, cat, _, _ := templateRig(t)
	srv.configsDir = "" // simulate missing config
	_ = cat
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"demo","parameters":{"projectId":"x"}}`))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "CONFIGS_DIR_NOT_CONFIGURED")
}

func TestCreateProjectFromTemplate_UnknownSlug(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"nope","parameters":{}}`))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNKNOWN_TEMPLATE")
}

func TestCreateProjectFromTemplate_InvalidParameterMappedTo400(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	// projectId pattern requires lowercase + hyphens; uppercase fails.
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"demo","parameters":{"projectId":"INVALID"}}`))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

func TestCreateProjectFromTemplate_RefusesOverwrite(t *testing.T) {
	srv, _, _, configsDir := templateRig(t)

	// Pre-place a file at the target path. The handler must NOT
	// silently clobber it — operator data loss is unacceptable.
	targetDir := filepath.Join(configsDir, "projects")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "existing.yaml"),
		[]byte("existing user content\n"), 0o644))

	body, _ := json.Marshal(map[string]any{
		"slug":       "demo",
		"parameters": map[string]string{"projectId": "existing"},
	})
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "FILE_EXISTS")

	// And the user's file is untouched.
	got, _ := os.ReadFile(filepath.Join(targetDir, "existing.yaml"))
	assert.Equal(t, "existing user content\n", string(got),
		"refused overwrite must NOT mutate the existing file")
}

func TestCreateProjectFromTemplate_ConcurrentSameTargetOneWinner(t *testing.T) {
	srv, _, _, configsDir := templateRig(t)

	const attempts = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make(chan int, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body, _ := json.Marshal(map[string]any{
				"slug": "demo",
				"parameters": map[string]string{
					"projectId": "race-project",
					"greeting":  "hello-" + string(rune('a'+i)),
				},
			})
			req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template", bytes.NewReader(body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.CreateProjectFromTemplate(rec, req)
			statuses <- rec.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(statuses)

	created := 0
	conflicts := 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	assert.Equal(t, 1, created, "exclusive create must allow exactly one winner")
	assert.Equal(t, attempts-1, conflicts)
	got, err := os.ReadFile(filepath.Join(configsDir, "projects", "race-project.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "projectId: race-project\n")
}

func TestCreateProjectFromTemplate_MissingSlugRejected(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"parameters":{}}`))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "slug is required")
}

func TestCreateProjectFromTemplate_BadJSON(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		strings.NewReader("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateProjectFromTemplate_MethodGuard(t *testing.T) {
	srv, _, _, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/from-template", nil)
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
