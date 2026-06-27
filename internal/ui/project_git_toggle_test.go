// Tests for the project Git-access enable/disable toggle
// (POST /ui/projects/{id}/git/toggle). See
// https://docs.vornik.io
//
// Covers:
//   - enable: POST enabled=true writes git.enabled=true, reloads, redirects.
//   - disable: POST enabled=false writes git.enabled=false, redirects.
//   - idempotent no-op: enabled=true on an already-enabled project skips the
//     write+reload (short-circuit) and still redirects.
//   - admin gate: non-admin POST (auth on) → 403, no write; auth-off → allowed.
//   - non-POST method → 405.
//   - the git field guard rejects an out-of-`git` top-level patch key.

package ui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/registry"
)

// errReloadBoom is a sentinel reload failure for the reload-failure path test.
var errReloadBoom = errors.New("boom: reload failed")

// countingReloader is a ConfigReloader that counts Reload() calls and can be
// made to fail, so the toggle tests can assert the short-circuit skips the
// reload and that a reload failure surfaces as a 500.
type countingReloader struct {
	reg      *registry.Registry
	dir      string
	calls    int
	failWith error
}

func (c *countingReloader) Reload() error {
	c.calls++
	if c.failWith != nil {
		return c.failWith
	}
	return c.reg.Load(c.dir)
}

// authOffUIPost builds an auth-disabled POST request carrying a form body,
// routed through AuthMiddleware{Enabled:false} so the auth-off context bit is
// stamped exactly as production would (uiRequireAdminMutation then allows it).
func authOffUIPost(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: false})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { captured = r }),
	).ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil {
		return req
	}
	return captured
}

// writeGitEnabledFixture overwrites the seeded project YAML with a git block
// set to the given enabled value.
func writeGitEnabledFixture(t *testing.T, dir, projectID string, enabled bool) {
	t.Helper()
	val := "false"
	if enabled {
		val = "true"
	}
	yaml := "projectId: " + projectID + "\n" +
		"displayName: " + projectID + "\n" +
		"swarmId: test-swarm\n" +
		"defaultWorkflowId: test-wf\n" +
		"git:\n  enabled: " + val + "\n"
	if err := os.WriteFile(filepath.Join(dir, "projects", projectID+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write git fixture: %v", err)
	}
}

func TestProjectGitToggle_Enable(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp") // active, no git block
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reg.GetProject("gp").Git.Enabled {
		t.Fatalf("fixture should start with git disabled")
	}
	rl := &countingReloader{reg: reg, dir: dir}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(rl))

	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/gp/git/toggle",
		strings.NewReader("enabled=true")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/projects/gp?git_enabled=1" {
		t.Errorf("redirect Location=%q, want /ui/projects/gp?git_enabled=1", loc)
	}
	if !reg.GetProject("gp").Git.Enabled {
		t.Errorf("Git.Enabled should be true after enable toggle")
	}
	if rl.calls == 0 {
		t.Errorf("expected the registry to be reloaded after the write")
	}
}

func TestProjectGitToggle_Disable(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	writeGitEnabledFixture(t, dir, "gp", true)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reg.GetProject("gp").Git.Enabled {
		t.Fatalf("fixture should start with git enabled")
	}
	rl := &countingReloader{reg: reg, dir: dir}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(rl))

	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/gp/git/toggle",
		strings.NewReader("enabled=false")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/projects/gp?git_enabled=0" {
		t.Errorf("redirect Location=%q, want /ui/projects/gp?git_enabled=0", loc)
	}
	if reg.GetProject("gp").Git.Enabled {
		t.Errorf("Git.Enabled should be false after disable toggle")
	}
}

func TestProjectGitToggle_IdempotentNoOp(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	writeGitEnabledFixture(t, dir, "gp", true)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	rl := &countingReloader{reg: reg, dir: dir}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(rl))

	// Desired state (true) already matches → short-circuit, no write/reload.
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/gp/git/toggle",
		strings.NewReader("enabled=true")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/projects/gp?git_enabled=1" {
		t.Errorf("redirect Location=%q, want git_enabled=1", loc)
	}
	if rl.calls != 0 {
		t.Errorf("no-op toggle must NOT reload the registry; got %d reload(s)", rl.calls)
	}
	if !reg.GetProject("gp").Git.Enabled {
		t.Errorf("Git.Enabled should remain true")
	}
}

func TestProjectGitToggle_RejectsNonAdmin(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(&countingReloader{reg: reg, dir: dir}))

	req := httptest.NewRequest(http.MethodPost, "/projects/gp/git/toggle",
		strings.NewReader("enabled=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// auth ON, project-scoped (non-admin) → must be refused.
	req = req.WithContext(api.ContextWithScopeForTesting(req.Context(), "gp"))
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if reg.GetProject("gp").Git.Enabled {
		t.Errorf("non-admin POST must not have flipped Git.Enabled")
	}
}

func TestProjectGitToggle_AuthOffAllowed(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(&countingReloader{reg: reg, dir: dir}))

	req := authOffUIPost("/projects/gp/git/toggle", "enabled=true")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("auth-off should be allowed: want 303, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !reg.GetProject("gp").Git.Enabled {
		t.Errorf("Git.Enabled should be true after auth-off enable toggle")
	}
}

func TestProjectGitToggle_ReloadFailure(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	rl := &countingReloader{reg: reg, dir: dir, failWith: errReloadBoom}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(rl))

	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/gp/git/toggle",
		strings.NewReader("enabled=true")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 on reload failure, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	// The atomic write committed before the reload failed → a .bak-<ts>
	// backup of the prior YAML must exist for manual recovery.
	baks, _ := filepath.Glob(filepath.Join(dir, "projects", "gp.yaml.bak-*"))
	if len(baks) == 0 {
		t.Errorf("expected a .bak backup file after a write+reload-failure, found none")
	}
}

func TestProjectGitToggle_ParseFormError(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewServer(WithProjectRegistry(reg), WithConfigReloader(&countingReloader{reg: reg, dir: dir}))

	// A malformed query escape (%zz) makes r.ParseForm() fail → 400.
	req := withAdminUI(httptest.NewRequest(http.MethodPost, "/projects/gp/git/toggle?x=%zz", nil))
	rec := httptest.NewRecorder()
	s.ProjectGitToggle(rec, req, "gp")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400 on ParseForm error, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestApplyProjectPatches_WriteFailure(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewServer(WithProjectRegistry(reg))

	// Make the projects/ dir read-only so the atomic temp-file create fails.
	projDir := filepath.Join(dir, "projects")
	if err := os.Chmod(projDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(projDir, 0o755) })

	err := s.applyProjectPatches("gp", gitPatchGuard, []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
	})
	if err == nil || !strings.Contains(err.Error(), "write project yaml") {
		t.Fatalf("want write-project-yaml error on read-only dir, got %v", err)
	}
}

func TestProjectGitToggle_MethodNotAllowed(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/projects/gp/git/toggle", nil)
	rec := httptest.NewRecorder()
	s.ProjectGitToggle(rec, req, "gp")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: want 405, got %d", rec.Code)
	}
}

// TestApplyProjectPatches_GuardAndErrorBranches covers the shared helper's
// pre-write guards and the projectReg.Load reload fallback (no configReloader).
func TestApplyProjectPatches_GuardAndErrorBranches(t *testing.T) {
	// Invalid project id (path separator) is rejected before any IO.
	s0 := NewServer()
	if err := s0.applyProjectPatches("a/b", gitPatchGuard, []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
	}); err == nil || !strings.Contains(err.Error(), "invalid project id") {
		t.Errorf("invalid id: want 'invalid project id' error, got %v", err)
	}

	// No registry wired → configDir empty → refused before IO.
	if err := s0.applyProjectPatches("gp", gitPatchGuard, []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
	}); err == nil || !strings.Contains(err.Error(), "config directory") {
		t.Errorf("no configdir: want config-directory error, got %v", err)
	}

	dir := t.TempDir()
	seedProjectFixture(t, dir, "gp")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	// projectReg wired but NO configReloader → exercises the Load fallback.
	s := NewServer(WithProjectRegistry(reg))

	// Guard refusal: a stray non-git key is refused before the write.
	if err := s.applyProjectPatches("gp", gitPatchGuard, []yamlPatch{
		{Path: []string{"projectId"}, Value: "renamed"},
	}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("guard: want 'refused' error, got %v", err)
	}

	// Happy path through the projectReg.Load fallback branch.
	if err := s.applyProjectPatches("gp", gitPatchGuard, []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
	}); err != nil {
		t.Fatalf("fallback reload path: unexpected error %v", err)
	}
	if !reg.GetProject("gp").Git.Enabled {
		t.Errorf("fallback path should have flipped Git.Enabled via projectReg.Load")
	}

	// Missing YAML file → read error.
	if err := s.applyProjectPatches("does-not-exist", gitPatchGuard, []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
	}); err == nil || !strings.Contains(err.Error(), "read project yaml") {
		t.Errorf("missing file: want read error, got %v", err)
	}
}

// TestGitPatchGuard_OnlyGitKey pins the git toggle's field-allowlist: it may
// write only the top-level `git:` key — never project config or identity.
func TestGitPatchGuard_OnlyGitKey(t *testing.T) {
	if !gitPatchGuard.Allows("git") {
		t.Fatal("git guard must allow the git key")
	}
	for _, protected := range []string{"projectId", "swarmId", "lifecycle", "autonomy"} {
		if gitPatchGuard.Allows(protected) {
			t.Errorf("git path must NOT be able to write %q", protected)
		}
	}
	if err := gitPatchGuard.Check(topLevelPatchKeys([]yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
	})); err != nil {
		t.Errorf("a git-only patch set must pass the guard: %v", err)
	}
	stray := []yamlPatch{
		{Path: []string{"git", "enabled"}, Value: true},
		{Path: []string{"projectId"}, Value: "renamed"},
	}
	if err := gitPatchGuard.Check(topLevelPatchKeys(stray)); err == nil {
		t.Error("a git patch set touching projectId must be refused")
	}
}
