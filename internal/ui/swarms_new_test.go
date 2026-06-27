package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// stubReloader is a no-op ConfigReloader used by swarms/workflows
// create tests — they exercise the write-to-disk path; the
// reload-after-write path is covered separately by callers that
// already have a real reloader (project_brief_test.go, etc.).
type stubReloader struct{}

func (stubReloader) Reload() error { return nil }

func newCreateRig(t *testing.T) (*Server, string) {
	t.Helper()
	configsDir := t.TempDir()
	srv := NewServer(WithConfigsDir(configsDir), WithConfigReloader(stubReloader{}))
	return srv, configsDir
}

// TestSwarmsNew_GETRendersForm — the gateway page renders with
// the starter-role default visible (so an operator hitting Submit
// without changing anything gets a sensible single-role swarm).
func TestSwarmsNew_GETRendersForm(t *testing.T) {
	srv, _ := newCreateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/swarms/new", nil)
	rr := httptest.NewRecorder()
	srv.SwarmsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, `name="swarmId"`)
	assert.Contains(t, body, `name="displayName"`)
	assert.Contains(t, body, `name="roleName"`)
	assert.Contains(t, body, "assistant", "default role name shows in the input value")
}

// TestSwarmsCreate_HappyPath — POSTing a valid form writes a
// SWARM.md, the file parses cleanly through ParseSwarmMarkdown,
// Validate passes, and the operator is redirected to the editor.
func TestSwarmsCreate_HappyPath(t *testing.T) {
	srv, configsDir := newCreateRig(t)

	form := url.Values{
		"swarmId":     {"happy-swarm"},
		"displayName": {"Happy"},
		"roleName":    {"helper"},
	}
	req := httptest.NewRequest(http.MethodPost, "/swarms/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.SwarmsCreate(rr, req)

	require.Equal(t, http.StatusSeeOther, rr.Code, "body=%s", rr.Body.String())
	assert.Equal(t, "/ui/swarms/happy-swarm/edit", rr.Header().Get("Location"))

	swarmPath := filepath.Join(configsDir, "swarms", "happy-swarm.md")
	data, err := os.ReadFile(swarmPath)
	require.NoError(t, err)

	parsed, err := registry.ParseSwarmMarkdown(data, "happy-swarm.md")
	require.NoError(t, err, "rendered SWARM.md must parse")
	require.NoError(t, parsed.Validate("happy-swarm.md"))
	assert.Equal(t, "happy-swarm", parsed.ID)
	assert.Equal(t, "Happy", parsed.DisplayName)
	require.Len(t, parsed.Roles, 1)
	assert.Equal(t, "helper", parsed.Roles[0].Name)
	assert.Equal(t, "vornik-agent:latest", parsed.Roles[0].Runtime.Image)
	assert.NotEmpty(t, parsed.Roles[0].SystemPrompt,
		"starter must include a body subsection that fills SystemPrompt; otherwise operators get an empty prompt + no editor hint")
}

// TestSwarmsCreate_RefusesOverwrite — a second POST with the
// same ID should fail loud instead of clobbering the existing
// file. Mirrors the projects/new overwrite guard.
func TestSwarmsCreate_RefusesOverwrite(t *testing.T) {
	srv, configsDir := newCreateRig(t)

	// Pre-seed a swarm at the target path.
	require.NoError(t, os.MkdirAll(filepath.Join(configsDir, "swarms"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(configsDir, "swarms", "dup.md"),
		[]byte("---\nswarmId: dup\n---\n"), 0o644))

	form := url.Values{"swarmId": {"dup"}, "roleName": {"helper"}}
	req := httptest.NewRequest(http.MethodPost, "/swarms/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.SwarmsCreate(rr, req)

	require.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "already exists")
}

// TestSwarmsCreate_RejectsInvalidID — empty / pattern-violating
// IDs surface a form-level error and don't write anything.
func TestSwarmsCreate_RejectsInvalidID(t *testing.T) {
	srv, configsDir := newCreateRig(t)

	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"trailing-hyphen", "bad-id-"},
		{"uppercase", "BadID"},
		{"too-short", "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"swarmId": {tc.id}, "roleName": {"helper"}}
			req := httptest.NewRequest(http.MethodPost, "/swarms/new", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rr := httptest.NewRecorder()
			srv.SwarmsCreate(rr, req)

			require.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
			assert.Contains(t, rr.Body.String(), "Swarm ID must be")

			// No file should have been written for any of these.
			if tc.id != "" {
				path := filepath.Join(configsDir, "swarms", tc.id+".md")
				_, err := os.Stat(path)
				assert.True(t, os.IsNotExist(err), "no swarm file should be written on validation failure")
			}
		})
	}
}

// TestSwarmsCreate_RejectsInvalidRoleName — same shape as the ID
// guard, scoped to the role-name field.
func TestSwarmsCreate_RejectsInvalidRoleName(t *testing.T) {
	srv, _ := newCreateRig(t)
	form := url.Values{"swarmId": {"valid-swarm"}, "roleName": {"BadRole"}}
	req := httptest.NewRequest(http.MethodPost, "/swarms/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.SwarmsCreate(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "Role name must")
}

// TestSwarmsCreate_DefaultDisplayNameFallsBackToID — operators
// can omit displayName; the ID becomes the human-readable label.
func TestSwarmsCreate_DefaultDisplayNameFallsBackToID(t *testing.T) {
	srv, configsDir := newCreateRig(t)
	form := url.Values{"swarmId": {"unnamed"}, "roleName": {"helper"}}
	req := httptest.NewRequest(http.MethodPost, "/swarms/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.SwarmsCreate(rr, req)
	require.Equal(t, http.StatusSeeOther, rr.Code, "body=%s", rr.Body.String())

	data, err := os.ReadFile(filepath.Join(configsDir, "swarms", "unnamed.md"))
	require.NoError(t, err)
	parsed, err := registry.ParseSwarmMarkdown(data, "unnamed.md")
	require.NoError(t, err)
	assert.Equal(t, "unnamed", parsed.DisplayName)
}
