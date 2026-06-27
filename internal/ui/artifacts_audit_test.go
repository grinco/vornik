package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
)

// auditArtifactsSetup creates a workspace root with one project that
// has a single artifact file, and returns a Server wired to that root.
func auditArtifactsSetup(t *testing.T, projectID string) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	artDir := filepath.Join(root, projectID, "artifacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "note.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	s := &Server{projectWorkspaceRoot: root, logger: zerolog.Nop()}
	return s, "note.txt"
}

// auditScopedReq builds a request whose context carries a scoped key
// for allowedProject — i.e. auth is on and the key may NOT touch any
// other project.
func auditScopedReq(method, target, allowedProject string, body url.Values) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	ctx := api.ContextWithScopeForTesting(req.Context(), allowedProject)
	return req.WithContext(ctx)
}

// TestProjectArtifacts_ScopeMismatchHidesOtherTenant asserts that a
// scoped key for project "alpha" cannot list project "victim"'s
// workspace artifacts — the handler must 404 (not 200, not 403).
func TestProjectArtifacts_ScopeMismatchHidesOtherTenant(t *testing.T) {
	s, _ := auditArtifactsSetup(t, "victim")
	req := auditScopedReq(http.MethodGet, "/ui/projects/victim/artifacts", "alpha", nil)
	rec := httptest.NewRecorder()

	s.ProjectArtifacts(rec, req, "victim")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("listing other-tenant project: want 404, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestProjectArtifactView_ScopeMismatchHidesOtherTenant asserts that a
// scoped key for "alpha" cannot read raw bytes of "victim"'s file.
func TestProjectArtifactView_ScopeMismatchHidesOtherTenant(t *testing.T) {
	s, rel := auditArtifactsSetup(t, "victim")
	req := auditScopedReq(http.MethodGet,
		"/ui/projects/victim/artifacts/raw?path="+url.QueryEscape(rel), "alpha", nil)
	rec := httptest.NewRecorder()

	s.ProjectArtifactView(rec, req, "victim")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("reading other-tenant file: want 404, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("other-tenant file contents leaked: %q", rec.Body.String())
	}
}

// TestProjectArtifactDelete_ScopeMismatchHidesOtherTenant asserts that
// a scoped key for "alpha" cannot delete "victim"'s file; the file must
// survive the rejected request.
func TestProjectArtifactDelete_ScopeMismatchHidesOtherTenant(t *testing.T) {
	s, rel := auditArtifactsSetup(t, "victim")
	form := url.Values{"path": {rel}}
	req := auditScopedReq(http.MethodPost, "/ui/projects/victim/artifacts/delete", "alpha", form)
	rec := httptest.NewRecorder()

	s.ProjectArtifactDelete(rec, req, "victim")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleting other-tenant file: want 404, got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(s.projectWorkspaceRoot, "victim", "artifacts", rel)); err != nil {
		t.Fatalf("other-tenant file was deleted despite scope mismatch: %v", err)
	}
}

// TestProjectArtifacts_InScopeStillAllowed is a guardrail: a key scoped
// to the target project still gets a normal 200 listing (the fix must
// not break the legitimate path).
func TestProjectArtifacts_InScopeStillAllowed(t *testing.T) {
	s, _ := auditArtifactsSetup(t, "alpha")
	req := auditScopedReq(http.MethodGet, "/ui/projects/alpha/artifacts", "alpha", nil)
	rec := httptest.NewRecorder()

	// render() needs templates; guard against the handler reaching that
	// far only on the success path. If it 404s here the scope check
	// over-rejected.
	defer func() { _ = recover() }()
	s.ProjectArtifacts(rec, req, "alpha")

	if rec.Code == http.StatusNotFound {
		t.Fatalf("in-scope listing was rejected as 404")
	}
}
