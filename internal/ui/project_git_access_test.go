// Tests for the project Git-access panel (Task 3.2: git-over-HTTPS UI).
// Covers:
//   - Panel rendered only when project.Git.Enabled is true (gating).
//   - Full clone URL when publicBaseURL is set; relative path + hint when empty.
//   - Forge caveat present for forge-backed projects, absent for non-forge.
//   - per-key enable-push / disable-push toggle actions (happy path).
//   - IDOR guard: foreign key_id rejected, no UpdateAllowPush call.
//   - buildGitAccessPanel unit: auth-off flag, URL derivation.

package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// spyAPIKeyRepo is a spy over uiMemAPIKeyRepo that records UpdateAllowPush calls.
type spyAPIKeyRepo struct {
	uiMemAPIKeyRepo
	allowPushCalls []allowPushCall
}

type allowPushCall struct {
	id    string
	allow bool
}

func (s *spyAPIKeyRepo) UpdateAllowPush(_ context.Context, id string, allowed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowPushCalls = append(s.allowPushCalls, allowPushCall{id: id, allow: allowed})
	for _, r := range s.rows {
		if r.ID == id {
			r.AllowPush = allowed
			return nil
		}
	}
	return persistence.ErrAPIKeyNotFound
}

// gitMinimalProject returns a registry.Project with a given projectID and
// Git.Enabled flag, suitable for panel tests.
func gitMinimalProject(projectID string, gitEnabled bool) *registry.Project {
	return &registry.Project{
		ID:  projectID,
		Git: registry.ProjectGit{Enabled: gitEnabled},
	}
}

// gitForgeProject returns a project with a forge provider configured.
func gitForgeProject(projectID string) *registry.Project {
	return &registry.Project{
		ID:  projectID,
		Git: registry.ProjectGit{Enabled: true},
		Forge: registry.ProjectForge{
			Provider: "github",
		},
	}
}

// renderProjectDetailGit renders the project_detail.html template with the
// given data and returns the body as a string. Uses a real Server (so
// templates are loaded).
func renderProjectDetailGit(t *testing.T, data ProjectDetailData) string {
	t.Helper()
	s := NewServer()
	var buf bytes.Buffer
	require.NoError(t, s.templates.ExecuteTemplate(&buf, "project_detail.html", data))
	return buf.String()
}

// ---------------------------------------------------------------------------
// Panel gating: shown only when Git.Enabled
// ---------------------------------------------------------------------------

// TestGitAccessPanel_RendersBothStates pins the always-render behavior:
// the panel is present in BOTH states (discoverability when off). The enabled
// arm shows the clone URL + a Disable control; the disabled arm shows a muted
// hint + an "Enable git access" control (and NO clone URL).
func TestGitAccessPanel_RendersBothStates(t *testing.T) {
	// Git.Enabled=true → enabled arm: clone URL + Disable control.
	dataOn := ProjectDetailData{
		Title:       "Project: myproj",
		CurrentPage: "projects",
		Project:     gitMinimalProject("myproj", true),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess: GitAccessPanel{
			Show:           true,
			Enabled:        true,
			RelativePath:   "/api/v1/git/myproj.git",
			BaseURLMissing: true,
		},
	}
	bodyOn := renderProjectDetailGit(t, dataOn)
	assert.Contains(t, bodyOn, "Git access", "panel header should appear when enabled")
	assert.Contains(t, bodyOn, "/api/v1/git/myproj.git", "clone URL should appear in the enabled arm")
	assert.Contains(t, bodyOn, `value="false"`, "enabled arm should carry a Disable button (enabled=false)")
	assert.Contains(t, bodyOn, "/ui/projects/myproj/git/toggle", "toggle form action should be present")

	// Git.Enabled=false → disabled arm: hint + Enable button, NO clone URL.
	dataOff := ProjectDetailData{
		Title:       "Project: myproj",
		CurrentPage: "projects",
		Project:     gitMinimalProject("myproj", false),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess: GitAccessPanel{
			Show:    true,
			Enabled: false,
		},
	}
	bodyOff := renderProjectDetailGit(t, dataOff)
	assert.Contains(t, bodyOff, "Git access", "panel header should appear even when disabled")
	assert.Contains(t, bodyOff, "Enable git access", "disabled arm should offer an Enable control")
	assert.Contains(t, bodyOff, `value="true"`, "Enable button should POST enabled=true")
	assert.NotContains(t, bodyOff, "/api/v1/git/myproj.git", "clone URL must not render in the disabled arm")
}

// TestGitAccessPanel_DisabledArm_NoGitBlock confirms the disabled arm renders
// for a project whose YAML has no git: block at all (Git.Enabled false, panel
// still shown so the operator can enable it).
func TestGitAccessPanel_DisabledArm_NoGitBlock(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: plain",
		CurrentPage: "projects",
		Project:     gitMinimalProject("plain", false),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess:   GitAccessPanel{Show: true, Enabled: false},
	}
	body := renderProjectDetailGit(t, data)
	assert.Contains(t, body, "Enable git access", "disabled arm should render for a project with no git block")
}

// ---------------------------------------------------------------------------
// Clone URL: full when publicBaseURL set, relative+hint when empty
// ---------------------------------------------------------------------------

func TestGitAccessPanel_CloneURL_FullWhenBaseURLSet(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: myproj",
		CurrentPage: "projects",
		Project:     gitMinimalProject("myproj", true),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess: GitAccessPanel{
			Show:         true,
			Enabled:      true,
			RelativePath: "/api/v1/git/myproj.git",
			CloneURL:     "https://<key-name>@vornik.example.com/api/v1/git/myproj.git",
		},
	}
	body := renderProjectDetailGit(t, data)
	assert.Contains(t, body, "https://&lt;key-name&gt;@vornik.example.com/api/v1/git/myproj.git",
		"full clone URL should render (HTML-escaped angle brackets)")
	assert.NotContains(t, body, "server.public_base_url",
		"hint must not appear when URL is set")
}

func TestGitAccessPanel_CloneURL_RelativePathAndHintWhenBaseURLMissing(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: myproj",
		CurrentPage: "projects",
		Project:     gitMinimalProject("myproj", true),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess: GitAccessPanel{
			Show:           true,
			Enabled:        true,
			RelativePath:   "/api/v1/git/myproj.git",
			BaseURLMissing: true,
		},
	}
	body := renderProjectDetailGit(t, data)
	assert.Contains(t, body, "/api/v1/git/myproj.git", "relative path should appear")
	assert.Contains(t, body, "server.public_base_url", "hint should appear when base URL missing")
}

// ---------------------------------------------------------------------------
// Forge caveat: present for forge-backed, absent for non-forge
// ---------------------------------------------------------------------------

func TestGitAccessPanel_ForgeCaveat_PresentForForgeBacked(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: fp",
		CurrentPage: "projects",
		Project:     gitForgeProject("fp"),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess: GitAccessPanel{
			Show:           true,
			Enabled:        true,
			RelativePath:   "/api/v1/git/fp.git",
			BaseURLMissing: true,
			IsForgeBacked:  true,
		},
	}
	body := renderProjectDetailGit(t, data)
	assert.Contains(t, body, "syncs from its forge",
		"forge caveat should appear for forge-backed project")
}

func TestGitAccessPanel_ForgeCaveat_AbsentForNonForge(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: plain",
		CurrentPage: "projects",
		Project:     gitMinimalProject("plain", true),
		TaskCounts:  map[persistence.TaskStatus]int64{},
		GitAccess: GitAccessPanel{
			Show:           true,
			Enabled:        true,
			RelativePath:   "/api/v1/git/plain.git",
			BaseURLMissing: true,
			IsForgeBacked:  false,
		},
	}
	body := renderProjectDetailGit(t, data)
	assert.NotContains(t, body, "syncs from its forge",
		"forge caveat must not appear for non-forge project")
}

// ---------------------------------------------------------------------------
// buildGitAccessPanel unit tests
// ---------------------------------------------------------------------------

func TestBuildGitAccessPanel_FullURL(t *testing.T) {
	project := gitMinimalProject("proj1", true)
	// Plain request context: IsAuthEnabledFromContext returns true by default
	// (no authEnabledKey set → default=true).
	req := httptest.NewRequest(http.MethodGet, "/projects/proj1", nil)

	panel := buildGitAccessPanel(project, "proj1", "https://vornik.example.com", req)
	assert.True(t, panel.Show, "panel should always render")
	assert.True(t, panel.Enabled, "Enabled mirrors project.Git.Enabled")
	assert.Equal(t, "/api/v1/git/proj1.git", panel.RelativePath)
	assert.Equal(t, "https://<key-name>@vornik.example.com/api/v1/git/proj1.git", panel.CloneURL)
	assert.False(t, panel.BaseURLMissing)
	assert.False(t, panel.IsForgeBacked)
	assert.False(t, panel.AuthDisabled)
}

// TestBuildGitAccessPanel_TrailingSlashBaseURL verifies FIX 3: a
// public_base_url with a trailing slash does not yield a double slash in the
// derived clone URL.
func TestBuildGitAccessPanel_TrailingSlashBaseURL(t *testing.T) {
	project := gitMinimalProject("proj1", true)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj1", nil)

	panel := buildGitAccessPanel(project, "proj1", "https://vornik.example.com/", req)
	assert.Equal(t, "https://<key-name>@vornik.example.com/api/v1/git/proj1.git", panel.CloneURL,
		"trailing slash in base URL must not produce a double slash")
	assert.NotContains(t, panel.CloneURL, "com//", "no double slash before the path")
}

// TestBuildGitAccessPanel_HTTPBaseURL covers the http:// (non-TLS) base-URL
// derivation branch.
func TestBuildGitAccessPanel_HTTPBaseURL(t *testing.T) {
	project := gitMinimalProject("proj-h", true)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj-h", nil)

	panel := buildGitAccessPanel(project, "proj-h", "http://localhost:8080", req)
	assert.Equal(t, "http://<key-name>@localhost:8080/api/v1/git/proj-h.git", panel.CloneURL,
		"http:// base URL should derive an http clone URL")
}

func TestBuildGitAccessPanel_RelativeWhenBaseURLEmpty(t *testing.T) {
	project := gitMinimalProject("proj2", true)
	req := httptest.NewRequest(http.MethodGet, "/projects/proj2", nil)

	panel := buildGitAccessPanel(project, "proj2", "", req)
	assert.True(t, panel.Enabled)
	assert.Equal(t, "/api/v1/git/proj2.git", panel.RelativePath)
	assert.Empty(t, panel.CloneURL)
	assert.True(t, panel.BaseURLMissing)
}

// TestBuildGitAccessPanel_DisabledProject confirms the panel is still built
// (Show=true) but reports Enabled=false when the project has git disabled.
func TestBuildGitAccessPanel_DisabledProject(t *testing.T) {
	project := gitMinimalProject("off-proj", false)
	req := httptest.NewRequest(http.MethodGet, "/projects/off-proj", nil)

	panel := buildGitAccessPanel(project, "off-proj", "https://vornik.example.com", req)
	assert.True(t, panel.Show, "panel should render even when git is disabled")
	assert.False(t, panel.Enabled, "Enabled should be false for a git-disabled project")
}

func TestBuildGitAccessPanel_ForgeDetected(t *testing.T) {
	project := gitForgeProject("forge-proj")
	req := httptest.NewRequest(http.MethodGet, "/projects/forge-proj", nil)

	panel := buildGitAccessPanel(project, "forge-proj", "https://base.example.com", req)
	assert.True(t, panel.IsForgeBacked)
}

func TestBuildGitAccessPanel_AuthDisabled(t *testing.T) {
	project := gitMinimalProject("proj3", true)
	req := authOffUIRequest(http.MethodGet, "/projects/proj3")

	panel := buildGitAccessPanel(project, "proj3", "https://vornik.example.com", req)
	assert.True(t, panel.AuthDisabled, "auth-off context should set AuthDisabled=true")
}

// ---------------------------------------------------------------------------
// Per-key push toggle: enable-push / disable-push actions
// ---------------------------------------------------------------------------

func TestProjectKeys_EnablePush_InvokesUpdateAllowPush(t *testing.T) {
	repo := &spyAPIKeyRepo{}
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:        "akey-x",
		ProjectID: "project-x",
		Name:      "op-key",
		KeyPrefix: "opk",
		CreatedAt: time.Now().UTC(),
	})
	server := NewServer(WithAPIKeyRepository(repo))

	form := url.Values{}
	form.Set("action", "enable-push")
	form.Set("key_id", "akey-x")
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/project-x/keys",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "project-x")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.allowPushCalls, 1, "UpdateAllowPush should be called exactly once")
	assert.Equal(t, "akey-x", repo.allowPushCalls[0].id)
	assert.True(t, repo.allowPushCalls[0].allow, "enable-push should pass allow=true")
}

func TestProjectKeys_DisablePush_InvokesUpdateAllowPush(t *testing.T) {
	repo := &spyAPIKeyRepo{}
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:        "akey-y",
		ProjectID: "project-y",
		Name:      "push-key",
		KeyPrefix: "psh",
		CreatedAt: time.Now().UTC(),
		AllowPush: true,
	})
	server := NewServer(WithAPIKeyRepository(repo))

	form := url.Values{}
	form.Set("action", "disable-push")
	form.Set("key_id", "akey-y")
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/project-y/keys",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "project-y")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.allowPushCalls, 1, "UpdateAllowPush should be called exactly once")
	assert.Equal(t, "akey-y", repo.allowPushCalls[0].id)
	assert.False(t, repo.allowPushCalls[0].allow, "disable-push should pass allow=false")
}

// ---------------------------------------------------------------------------
// IDOR guard: foreign key_id must be rejected
// ---------------------------------------------------------------------------

func TestProjectKeys_EnablePush_IDORRejectsForeignKey(t *testing.T) {
	repo := &spyAPIKeyRepo{}
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:        "akey-other",
		ProjectID: "project-other",
		Name:      "other-key",
		KeyPrefix: "oth",
		CreatedAt: time.Now().UTC(),
	})
	server := NewServer(WithAPIKeyRepository(repo))

	// project-attacker tries to enable push on akey-other (owned by project-other).
	form := url.Values{}
	form.Set("action", "enable-push")
	form.Set("key_id", "akey-other")
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/project-attacker/keys",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "project-attacker")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "key not found in this project",
		"IDOR guard should surface error for cross-project key_id")
	assert.Empty(t, repo.allowPushCalls,
		"UpdateAllowPush must NOT be called when IDOR check fails")
}

// ---------------------------------------------------------------------------
// AllowPush rendered in the key list
// ---------------------------------------------------------------------------

func TestProjectKeys_AllowPushRenderedInList(t *testing.T) {
	repo := &uiMemAPIKeyRepo{}
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:        "akey-push",
		ProjectID: "projp",
		Name:      "push-enabled-key",
		KeyPrefix: "psh",
		CreatedAt: time.Now().UTC(),
		AllowPush: true,
	})
	_ = repo.Create(context.Background(), &persistence.APIKey{
		ID:        "akey-nopush",
		ProjectID: "projp",
		Name:      "push-disabled-key",
		KeyPrefix: "nop",
		CreatedAt: time.Now().UTC().Add(-time.Minute),
		AllowPush: false,
	})
	server := NewServer(WithAPIKeyRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/projects/projp/keys", nil)
	rec := httptest.NewRecorder()
	server.ProjectKeys(rec, req, "projp")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The Git-push column states the CURRENT permission unambiguously, not the
	// action: "push enabled" for AllowPush=true, "push disabled" for false.
	assert.Contains(t, body, "push enabled", "AllowPush=true key should show current state 'push enabled'")
	assert.Contains(t, body, "push disabled", "AllowPush=false key should show current state 'push disabled'")
	// The action button is a clear verb naming what it DOES (the state it
	// switches TO), distinct from the current-state column.
	assert.Contains(t, body, "Disable push", "push-enabled key's action button must read 'Disable push'")
	assert.Contains(t, body, "Enable push", "push-disabled key's action button must read 'Enable push'")
	// The old ambiguous labels must be gone.
	assert.NotContains(t, body, ">allow push<", "old ambiguous 'allow push' label must be replaced")
	assert.NotContains(t, body, ">no push<", "old ambiguous 'no push' label must be replaced")
}

// ---------------------------------------------------------------------------
// WithPublicBaseURL ServerOption
// ---------------------------------------------------------------------------

func TestWithPublicBaseURL_StoresValue(t *testing.T) {
	s := NewServer(WithPublicBaseURL("https://vornik.example.com"))
	assert.Equal(t, "https://vornik.example.com", s.publicBaseURL)
}
