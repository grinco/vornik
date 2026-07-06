package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/registry"
)

// --- fakes satisfying projectdoctor.Deps' narrow interfaces, mirroring
// the ones in internal/projectdoctor's own tests (Task 7) but exported
// only within this package. ---

type setupFakeResolver struct {
	proj *registry.Project
	err  error
}

func (f setupFakeResolver) ResolveProjectConfig(_ string) (*registry.Project, *registry.Swarm, *registry.Workflow, error) {
	return f.proj, nil, nil, f.err
}

// setupFakeSecretStore backs both SecretReader and SecretWriter with a
// single shared map, so a SetSecret call is immediately visible to the
// next Has() check — exercising the real "fix flips green" flow.
type setupFakeSecretStore struct {
	values map[string]bool
}

func (f *setupFakeSecretStore) Has(name string) bool { return f.values[name] }
func (f *setupFakeSecretStore) Set(name, _ string) error {
	f.values[name] = true
	return nil
}

// setupFakeSmoke records Trigger calls and reflects them in Latest, the
// way the real SmokeRunner would report the task it just enqueued as
// running.
type setupFakeSmoke struct {
	triggeredPrompt string
	last            projectdoctor.SmokeStatus
	hasLast         bool
}

func (f *setupFakeSmoke) Trigger(_ context.Context, _, prompt string) (string, error) {
	f.triggeredPrompt = prompt
	f.last = projectdoctor.SmokeStatus{TaskID: "task_smoke_1", Status: projectdoctor.StatusYellow, Detail: "running", Running: true}
	f.hasLast = true
	return "task_smoke_1", nil
}

func (f *setupFakeSmoke) Latest(_ string) (projectdoctor.SmokeStatus, bool) { return f.last, f.hasLast }

func TestProjectSetup_RendersChecklist(t *testing.T) {
	proj := &registry.Project{ID: "p1"}
	doctor := projectdoctor.New(projectdoctor.Deps{Registry: setupFakeResolver{proj: proj}})
	srv := NewServer(WithProjectDoctor(doctor))

	req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/setup", nil)
	rr := httptest.NewRecorder()
	srv.ProjectSetup(rr, req, "p1")

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	body := rr.Body.String()
	for _, title := range []string{
		"Configuration valid",
		"Secrets present",
		"Chat model responds",
		"MCP servers",
		"Schedule armed",
		"Smoke test",
	} {
		assert.Contains(t, body, title)
	}
}

func TestProjectSetup_SecretFixFlipsGreen(t *testing.T) {
	proj := &registry.Project{
		ID:          "p1",
		Permissions: registry.ProjectPermissions{Secrets: []string{"TOK"}},
	}
	store := &setupFakeSecretStore{values: map[string]bool{}}
	doctor := projectdoctor.New(projectdoctor.Deps{
		Registry:     setupFakeResolver{proj: proj},
		Secrets:      store,
		SecretWriter: store,
	})
	srv := NewServer(WithProjectDoctor(doctor))

	form := url.Values{"name": {"TOK"}, "value": {"s3cr3t"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/p1/setup/secrets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectSetupSecret(rr, req, "p1")

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, "pill-ok", "secret check must flip to green after the fix")
	assert.NotContains(t, body, "missing")
	assert.True(t, store.Has("TOK"))
}

// TestProjectSetup_SecretFix_RejectsUndeclaredName is the UI-layer
// regression for the companion review finding (2026-07-04): posting a
// secret name the project doesn't declare must fail with 400 instead
// of silently writing through the store.
func TestProjectSetup_SecretFix_RejectsUndeclaredName(t *testing.T) {
	proj := &registry.Project{
		ID:          "p1",
		Permissions: registry.ProjectPermissions{Secrets: []string{"TOK"}},
	}
	store := &setupFakeSecretStore{values: map[string]bool{}}
	doctor := projectdoctor.New(projectdoctor.Deps{
		Registry:     setupFakeResolver{proj: proj},
		Secrets:      store,
		SecretWriter: store,
	})
	srv := NewServer(WithProjectDoctor(doctor))

	form := url.Values{"name": {"EVIL"}, "value": {"s3cr3t"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/p1/setup/secrets", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectSetupSecret(rr, req, "p1")

	require.Equal(t, http.StatusBadRequest, rr.Code, "body=%s", rr.Body.String())
	assert.False(t, store.Has("EVIL"), "undeclared secret must never reach the writer")
}

func TestProjectSetup_SmokeButtonTriggers(t *testing.T) {
	proj := &registry.Project{ID: "p1", Autonomy: registry.ProjectAutonomy{Goal: "track pricing"}}
	smoke := &setupFakeSmoke{}
	doctor := projectdoctor.New(projectdoctor.Deps{
		Registry: setupFakeResolver{proj: proj},
		Smoke:    smoke,
	})
	srv := NewServer(WithProjectDoctor(doctor))

	req := httptest.NewRequest(http.MethodPost, "/ui/projects/p1/setup/smoke", nil)
	rr := httptest.NewRecorder()
	srv.ProjectSetupSmoke(rr, req, "p1")

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	assert.Equal(t, "track pricing", smoke.triggeredPrompt, "TriggerSmoke must fire the doctor's smoke runner")
	body := rr.Body.String()
	assert.Contains(t, body, "task_smoke_1")
	// The button's Tailwind classes always contain the literal substring
	// "disabled" (disabled:opacity-40 etc.) regardless of state, so
	// asserting on that alone would pass even if the guard were broken.
	// Assert on the button's own state-driven label/attribute instead:
	// running must show "Running…" and must not show the enabled label,
	// and the disabled attribute itself must be present right before
	// the class list.
	assert.Contains(t, body, "Running…", "the smoke button must show a running state while a run is in flight")
	assert.NotContains(t, body, "Run smoke test", "the enabled label must not render while a run is in flight")
	assert.Regexp(t, `(?s)hx-confirm="[^"]*"\s*disabled\s*class=`, body, "the smoke button must carry the disabled attribute while a run is in flight")
}

func TestProjectSetup_UnknownProject(t *testing.T) {
	doctor := projectdoctor.New(projectdoctor.Deps{Registry: setupFakeResolver{err: errors.New("boom")}})
	srv := NewServer(WithProjectDoctor(doctor))

	req := httptest.NewRequest(http.MethodGet, "/ui/projects/ghost/setup", nil)
	rr := httptest.NewRecorder()
	srv.ProjectSetup(rr, req, "ghost")

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, "Configuration valid")
	assert.Contains(t, body, "pill-danger", "an unresolved project must show config_valid red")
	assert.NotContains(t, body, "Secrets present", "an unresolved project runs config_valid only")
}
