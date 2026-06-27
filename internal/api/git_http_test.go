package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// gitRegistryWithProject builds a *registry.Registry containing a single
// project with the given ID and Git.Enabled flag, by loading minimal YAML
// fixtures. Used by the Git.Enabled serving-path gate tests (FIX 1).
func gitRegistryWithProject(t *testing.T, projectID string, gitEnabled bool) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "swarms"), 0o755); err != nil {
		t.Fatalf("mkdir swarms: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: fake-agent
---
`), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf
entrypoint: run
steps:
  run:
    type: agent
    prompt: "work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	projYAML := "projectId: " + projectID + "\n" +
		"displayName: P\n" +
		"swarmId: swarm-1\n" +
		"defaultWorkflowId: wf\n" +
		"defaultPriority: 50\n" +
		"git:\n" +
		"  enabled: " + map[bool]string{true: "true", false: "false"}[gitEnabled] + "\n"
	if err := os.WriteFile(filepath.Join(root, "projects", projectID+".yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}
	reg := registry.New()
	if err := reg.Load(root); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	// Sanity: confirm the flag round-tripped as written.
	p := reg.GetProject(projectID)
	if p == nil {
		t.Fatalf("project %q not loaded", projectID)
	}
	if p.Git.Enabled != gitEnabled {
		t.Fatalf("project %q Git.Enabled = %v, want %v", projectID, p.Git.Enabled, gitEnabled)
	}
	return reg
}

// mustGit runs a git command in dir (empty = no Dir override), fataling on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// gitWriteFile writes content to path, creating parent dirs as needed.
// (Named gitWriteFile to avoid colliding with document_tools_test.go's writeFile.)
func gitWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("gitWriteFile mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("gitWriteFile: %v", err)
	}
}

// minimalConfigWithWorkspace returns a *config.Config with
// ProjectWorkspacePath set to root.
func minimalConfigWithWorkspace(root string) *config.Config {
	cfg := &config.Config{}
	cfg.Runtime.ProjectWorkspacePath = root
	return cfg
}

// newTestServerWithWorkspaceRoot builds a minimal *Server with
// ProjectWorkspacePath set to root and auth disabled (anonymous context
// stamped at the test-router level).
func newTestServerWithWorkspaceRoot(t *testing.T, root string) *Server {
	t.Helper()
	return NewServer(WithConfig(minimalConfigWithWorkspace(root)))
}

// withAnonymousGitCtx stamps the anonymous git context values so GitHTTPBackend
// can read REMOTE_USER without a real auth middleware.
func withAnonymousGitCtx(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, gitKeyCtxKey{}, nil)
	ctx = context.WithValue(ctx, gitRemoteUserCtxKey{}, "anonymous")
	return ctx
}

// gitTestRouter registers the Slice-1 read-path routes on a new mux and
// returns it.  Auth is bypassed by stamping anonymous context values.
//
// Go's net/http mux (1.22+) doesn't allow literal dots inside a wildcard
// segment (e.g. {projectID}.git), so we use a catch-all on the /api/v1/git/
// prefix and parse the project ID ourselves by stripping the ".git" suffix.
func gitTestRouter(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/git/", func(w http.ResponseWriter, r *http.Request) {
		// Extract projectID from the path: /api/v1/git/<id>.git/...
		// Strip the leading prefix, then take the first path segment and
		// remove the ".git" suffix.
		const prefix = "/api/v1/git/"
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		// rest = "proj_clone.git/info/refs" or "proj_clone.git/git-upload-pack"
		seg := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			seg = rest[:idx]
		}
		projectID := strings.TrimSuffix(seg, ".git")
		r.SetPathValue("projectID", projectID)
		ctx := withAnonymousGitCtx(r.Context())
		srv.GitHTTPBackend(w, r.WithContext(ctx))
	})
	return mux
}

// TestGitHTTPBackend_CloneReadOnly is the primary integration test.
// It creates a temp workspace repo with one commit, routes an HTTP server
// through GitHTTPBackend, and runs a real git clone against it — verifying
// the committed file is present in the clone.
func TestGitHTTPBackend_CloneReadOnly(t *testing.T) {
	root := t.TempDir()
	proj := "proj_clone"
	repo := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repo)
	gitWriteFile(t, filepath.Join(repo, "README.md"), "hello")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root) // *Server with ProjectWorkspacePath=root, auth disabled
	ts := httptest.NewServer(gitTestRouter(srv))   // registers the Slice-1 routes
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("clone failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err != nil {
		t.Fatalf("expected README.md in clone: %v", err)
	}
}

// TestGitHTTPBackend_UnknownProject verifies the handler returns 404 when the
// project ID does not map to an existing workspace directory.
func TestGitHTTPBackend_UnknownProject(t *testing.T) {
	root := t.TempDir()
	// No repo created under root.
	srv := newTestServerWithWorkspaceRoot(t, root)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/git/proj_missing.git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("projectID", "proj_missing")
	ctx := withAnonymousGitCtx(req.Context())
	rec := httptest.NewRecorder()
	srv.GitHTTPBackend(rec, req.WithContext(ctx))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown project, got %d", rec.Code)
	}
}

// TestGitHTTPBackend_TraversalRejected verifies that path-traversal project IDs
// are rejected with 404 before reaching the filesystem.
func TestGitHTTPBackend_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithWorkspaceRoot(t, root)

	for _, id := range []string{"../etc", "..", "a/b", "a\x00b"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/git/x.git/info/refs?service=git-upload-pack", nil)
		req.SetPathValue("projectID", id)
		ctx := withAnonymousGitCtx(req.Context())
		rec := httptest.NewRecorder()
		srv.GitHTTPBackend(rec, req.WithContext(ctx))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("traversal ID %q: expected 404, got %d", id, rec.Code)
		}
	}
}

// TestGitWorkspaceRoot exercises the gitWorkspaceRoot helper directly.
func TestGitWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithWorkspaceRoot(t, root)

	got, err := srv.gitWorkspaceRoot("proj_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(root, "proj_abc")
	if got != want {
		t.Fatalf("gitWorkspaceRoot = %q, want %q", got, want)
	}

	// Traversal should fail.
	if _, err := srv.gitWorkspaceRoot("../evil"); err == nil {
		t.Fatal("expected error for traversal ID")
	}
}

// TestGitWorkspaceRoot_NilConfig covers the unconfigured-server branch.
func TestGitWorkspaceRoot_NilConfig(t *testing.T) {
	srv := NewServer() // no config → ProjectWorkspacePath == ""
	if _, err := srv.gitWorkspaceRoot("proj_abc"); err == nil {
		t.Fatal("expected error when ProjectWorkspacePath is not configured")
	}
}

// TestGitSafeJoinUnder covers all branches of gitSafeJoinUnder.
func TestGitSafeJoinUnder(t *testing.T) {
	base := t.TempDir()

	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{
			name:    "valid child",
			rel:     "subdir",
			wantErr: false,
		},
		{
			name:    "valid nested child",
			rel:     "a/b/c",
			wantErr: false,
		},
		{
			name:    "traversal dot dot",
			rel:     "../escape",
			wantErr: true,
		},
		{
			name:    "double dot only",
			rel:     "..",
			wantErr: true,
		},
		{
			name:    "resolves to base itself",
			rel:     "subdir/..",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gitSafeJoinUnder(base, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for rel=%q, got path %q", tc.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for rel=%q: %v", tc.rel, err)
			}
			want := filepath.Join(base, tc.rel)
			if got != want {
				t.Fatalf("gitSafeJoinUnder = %q, want %q", got, want)
			}
		})
	}
}

// TestParseCGIOutput covers all branches of parseCGIOutput.
func TestParseCGIOutput(t *testing.T) {
	tests := []struct {
		name       string
		raw        []byte
		wantStatus int
		wantHdr    map[string]string // key→first value
		wantBody   []byte
		wantErrStr string // non-empty means expect error containing this
	}{
		{
			name:       "normal CRLF separator with body",
			raw:        []byte("Content-Type: text/plain\r\n\r\nhello body"),
			wantStatus: 200,
			wantHdr:    map[string]string{"Content-Type": "text/plain"},
			wantBody:   []byte("hello body"),
		},
		{
			name:       "LF-only separator fallback",
			raw:        []byte("Content-Type: application/octet-stream\n\nbinary"),
			wantStatus: 200,
			wantHdr:    map[string]string{"Content-Type": "application/octet-stream"},
			wantBody:   []byte("binary"),
		},
		{
			name:       "Status header 404",
			raw:        []byte("Status: 404 Not Found\r\nContent-Type: text/plain\r\n\r\nnot found"),
			wantStatus: 404,
			wantHdr:    map[string]string{"Content-Type": "text/plain"},
			wantBody:   []byte("not found"),
		},
		{
			name:       "Status header 301 no reason phrase",
			raw:        []byte("Status: 301\r\n\r\n"),
			wantStatus: 301,
			wantBody:   []byte{},
		},
		{
			name:       "no blank line at all — all header section, empty body",
			raw:        []byte("Content-Type: text/plain"),
			wantStatus: 200,
			wantHdr:    map[string]string{"Content-Type": "text/plain"},
			wantBody:   []byte{},
		},
		{
			name:       "malformed header line no colon",
			raw:        []byte("MissingColon\r\n\r\n"),
			wantErrStr: "malformed CGI header line",
		},
		{
			name:       "invalid Status value non-numeric",
			raw:        []byte("Status: abc Bad\r\n\r\n"),
			wantErrStr: "invalid CGI Status value",
		},
		{
			name:       "multiple headers",
			raw:        []byte("Content-Type: text/html\r\nX-Custom: val\r\n\r\nbody"),
			wantStatus: 200,
			wantHdr:    map[string]string{"Content-Type": "text/html", "X-Custom": "val"},
			wantBody:   []byte("body"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, hdrs, body, err := parseCGIOutput(tc.raw)
			if tc.wantErrStr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrStr)
				}
				if !strings.Contains(err.Error(), tc.wantErrStr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.wantBody != nil && string(body) != string(tc.wantBody) {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
			for k, v := range tc.wantHdr {
				if got := hdrs.Get(k); got != v {
					t.Fatalf("header %q = %q, want %q", k, got, v)
				}
			}
		})
	}
}

// TestBuildGitCGIEnv covers the conditional branches in buildGitCGIEnv.
func TestBuildGitCGIEnv(t *testing.T) {
	t.Run("Git-Protocol header forwarded", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Git-Protocol", "version=2")
		ctx := withAnonymousGitCtx(req.Context())
		env := buildGitCGIEnv(req.WithContext(ctx), "/ws", "/proj/info/refs")
		if !containsEnv(env, "GIT_PROTOCOL=version=2") {
			t.Fatalf("expected GIT_PROTOCOL=version=2 in env, got: %v", env)
		}
	})

	t.Run("Content-Length from r.ContentLength when header absent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("data"))
		// Clear the Content-Length header that httptest sets but keep r.ContentLength.
		req.Header.Del("Content-Length")
		req.ContentLength = 4
		ctx := withAnonymousGitCtx(req.Context())
		env := buildGitCGIEnv(req.WithContext(ctx), "/ws", "/proj/git-upload-pack")
		if !containsEnv(env, "CONTENT_LENGTH=4") {
			t.Fatalf("expected CONTENT_LENGTH=4 in env, got: %v", env)
		}
	})

	t.Run("Content-Type forwarded", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		ctx := withAnonymousGitCtx(req.Context())
		env := buildGitCGIEnv(req.WithContext(ctx), "/ws", "/proj/git-upload-pack")
		if !containsEnv(env, "CONTENT_TYPE=application/x-git-upload-pack-request") {
			t.Fatalf("expected CONTENT_TYPE in env, got: %v", env)
		}
	})

	t.Run("no REMOTE_USER context falls back to anonymous", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		// No gitRemoteUserCtxKey in context.
		env := buildGitCGIEnv(req, "/ws", "/proj/info/refs")
		if !containsEnv(env, "REMOTE_USER=anonymous") {
			t.Fatalf("expected REMOTE_USER=anonymous in env, got: %v", env)
		}
	})

	t.Run("negative ContentLength not emitted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Del("Content-Length")
		req.ContentLength = -1
		ctx := withAnonymousGitCtx(req.Context())
		env := buildGitCGIEnv(req.WithContext(ctx), "/ws", "/proj/info/refs")
		for _, e := range env {
			if strings.HasPrefix(e, "CONTENT_LENGTH=") {
				t.Fatalf("unexpected CONTENT_LENGTH in env: %v", env)
			}
		}
	})
}

// containsEnv reports whether the env slice contains the given entry.
func containsEnv(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

// TestGitHTTPBackend_InvalidProjectID verifies that a project ID that passes
// sanitizeGitProjectID but resolves to a non-existent directory returns 404.
func TestGitHTTPBackend_InvalidProjectID(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithWorkspaceRoot(t, root)

	// "nonexistent" is a valid sanitized ID but has no directory on disk.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/git/nonexistent.git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("projectID", "nonexistent")
	ctx := withAnonymousGitCtx(req.Context())
	rec := httptest.NewRecorder()
	srv.GitHTTPBackend(rec, req.WithContext(ctx))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent repo dir, got %d", rec.Code)
	}
}

// TestGitHTTPBackend_NotARepo verifies that when git http-backend is asked
// about a directory that exists but is not a git repository, it returns 404
// (git emits a CGI "Status: 404" response rather than exiting non-zero).
func TestGitHTTPBackend_NotARepo(t *testing.T) {
	root := t.TempDir()
	proj := "not_a_repo"
	// Create the directory but do NOT init a git repo.
	if err := os.MkdirAll(filepath.Join(root, proj), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	srv := newTestServerWithWorkspaceRoot(t, root)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/git/not_a_repo.git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("projectID", proj)
	ctx := withAnonymousGitCtx(req.Context())
	rec := httptest.NewRecorder()
	srv.GitHTTPBackend(rec, req.WithContext(ctx))

	// git-http-backend returns a CGI "Status: 404 Not Found" (exit 0) for
	// a non-repo directory, which the handler forwards as HTTP 404.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-repo dir (CGI 404), got %d", rec.Code)
	}
}

// TestGitHTTPBackend_CanceledContext verifies that a cancelled context causes
// the handler to return silently (no panic, no body written).
func TestGitHTTPBackend_CanceledContext(t *testing.T) {
	root := t.TempDir()
	proj := "proj_cancel"
	// Create a valid git repo so the handler reaches the exec step.
	repo := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repo)
	gitWriteFile(t, filepath.Join(repo, "f.txt"), "x")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/git/proj_cancel.git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("projectID", proj)

	cancelCtx, cancel := context.WithCancel(req.Context())
	cancel() // cancel immediately
	ctx := withAnonymousGitCtx(cancelCtx)
	rec := httptest.NewRecorder()
	srv.GitHTTPBackend(rec, req.WithContext(ctx))

	// With a cancelled context, git may or may not have run.
	// Either the exec fails due to cancellation (no body written = 200 default,
	// or 500 if context was not detected), or it succeeds before the cancel lands.
	// The invariant is: no panic, and if it wrote a 500 it must not be a path leak.
	body := rec.Body.String()
	if strings.Contains(body, root) {
		t.Fatalf("path leak in response body: %s", body)
	}
}

// TestGitHTTPBackend_ExecFails verifies that when the git binary fails with a
// non-context error the handler returns 500 with "git http-backend error".
// We achieve this by placing a fake "git" wrapper (that exits 1) first in PATH.
func TestGitHTTPBackend_ExecFails(t *testing.T) {
	root := t.TempDir()
	proj := "proj_execfail"
	// Create the directory so the stat check passes.
	if err := os.MkdirAll(filepath.Join(root, proj), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write a fake git that exits 1 immediately.
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	gitWriteFile(t, fakeGit, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Prepend our fake git to PATH so exec.Command("git",...) picks it up.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := newTestServerWithWorkspaceRoot(t, root)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/git/proj_execfail.git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("projectID", proj)
	ctx := withAnonymousGitCtx(req.Context())
	rec := httptest.NewRecorder()
	srv.GitHTTPBackend(rec, req.WithContext(ctx))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when git exits non-zero, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "git http-backend error") {
		t.Fatalf("expected 'git http-backend error' in body, got: %s", rec.Body.String())
	}
}

// fakeAdminAuditRepo is an in-memory stub of persistence.AdminAuditRepository
// that records every Insert call for test assertions.
type fakeAdminAuditRepo struct {
	mu      sync.Mutex
	entries []*persistence.AdminAuditEntry
}

func (f *fakeAdminAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAdminAuditRepo) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*persistence.AdminAuditEntry, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func (f *fakeAdminAuditRepo) snapshot() []*persistence.AdminAuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*persistence.AdminAuditEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

// gitRegisteredRouter builds a router that mirrors the production wiring:
// catch-all /api/v1/git/ prefix + parseGitRequest dispatch +
// gitHTTPAuth(svc, ...) + GitHTTPBackend. It uses the SAME parseGitRequest
// helper registerGitRoutes uses, so routing stays in lockstep. Auth is bypassed
// (authEnabled=false) so tests don't need real DB-backed keys. The supplied
// fakeAdminAuditRepo captures audit rows.
func gitRegisteredRouter(srv *Server, auditRepo *fakeAdminAuditRepo) http.Handler {
	// Wire the audit repo onto the server so GitHTTPBackend can write rows.
	srv.adminAuditRepo = auditRepo

	mux := http.NewServeMux()
	// Auth disabled → gitHTTPAuth stamps anonymous context and calls next.
	authMW := gitHTTPAuth(nil, func(string) bool { return false }, false)
	uploadHandler := authMW(gitServiceUpload, http.HandlerFunc(srv.GitHTTPBackend))
	receiveHandler := authMW(gitServiceReceive, http.HandlerFunc(srv.GitHTTPBackend))

	mux.HandleFunc("/api/v1/git/", func(w http.ResponseWriter, r *http.Request) {
		projectID, svc, ok := parseGitRequest(r.URL.Path, r.URL.Query().Get("service"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		r.SetPathValue("projectID", projectID)
		if svc == gitServiceReceive {
			receiveHandler.ServeHTTP(w, r)
			return
		}
		uploadHandler.ServeHTTP(w, r)
	})
	return mux
}

// TestGitHTTPRegistered_RouteAndAudit is the integration test for Task 1.4.
// It drives info/refs?service=git-upload-pack through the registered (production-
// wired) router and asserts:
//
//	(a) 200 + Content-Type: application/x-git-upload-pack-advertisement
//	(b) exactly one audit row with Action=="git.upload-pack" and Target==projectID
func TestGitHTTPRegistered_RouteAndAudit(t *testing.T) {
	root := t.TempDir()
	proj := "proj_audit"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "hello")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root)
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	// (a) Check info/refs response.
	resp, err := ts.Client().Get(ts.URL + "/api/v1/git/" + proj + ".git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info/refs: expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/x-git-upload-pack-advertisement") {
		t.Fatalf("info/refs: Content-Type = %q, want application/x-git-upload-pack-advertisement", ct)
	}

	// (b) Exactly one audit row with expected fields.
	rows := auditRepo.snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "git.upload-pack" {
		t.Fatalf("audit Action = %q, want %q", row.Action, "git.upload-pack")
	}
	if row.Target != proj {
		t.Fatalf("audit Target = %q, want %q", row.Target, proj)
	}
	if row.Source != "git" {
		t.Fatalf("audit Source = %q, want %q", row.Source, "git")
	}
	if row.Principal != "anonymous" {
		t.Fatalf("audit Principal = %q, want %q", row.Principal, "anonymous")
	}
	// After field must be valid JSON with "service" key.
	var afterMap map[string]any
	if err := json.Unmarshal([]byte(row.After), &afterMap); err != nil {
		t.Fatalf("audit After is not valid JSON: %v (got %q)", err, row.After)
	}
	if afterMap["service"] != "upload-pack" {
		t.Fatalf("audit After[service] = %q, want %q", afterMap["service"], "upload-pack")
	}
}

// TestGitHTTPRegistered_InvalidServicePath verifies that paths other than
// info/refs and git-upload-pack return 404 (dumb protocol + push disabled).
func TestGitHTTPRegistered_InvalidServicePath(t *testing.T) {
	root := t.TempDir()
	proj := "proj_badpath"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "hello")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root)
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	// Dumb-protocol object paths + HEAD remain 404. git-receive-pack is NO
	// LONGER here — Task 2.4 makes it a routable (push) endpoint.
	for _, badPath := range []string{
		"/api/v1/git/" + proj + ".git/HEAD",
		"/api/v1/git/" + proj + ".git/objects/info/packs",
	} {
		resp, err := ts.Client().Get(ts.URL + badPath)
		if err != nil {
			t.Fatalf("GET %s: %v", badPath, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("path %s: expected 404, got %d", badPath, resp.StatusCode)
		}
	}
}

// TestGitHTTPRegistered_AuthDisabledPassthrough verifies that gitHTTPAuth
// with authEnabled=false passes the request through to the next handler
// without requiring credentials.
func TestGitHTTPRegistered_AuthDisabledPassthrough(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithWorkspaceRoot(t, root)
	authMW := gitHTTPAuth(nil, func(string) bool { return false }, false /* disabled */)
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := authMW(gitServiceUpload, next)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/git/proj.git/info/refs", nil)
	req.SetPathValue("projectID", "proj")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called {
		t.Fatal("expected next handler to be called when auth is disabled")
	}
	_ = srv // suppress unused warning
}

// TestGitHTTPRegistered_ServiceParamGate verifies the info/refs service-param
// gate after Task 2.4: BOTH git-upload-pack and git-receive-pack advertisements
// route through (auth disabled → 200), while dumb protocol (no/unknown service
// param) is still rejected with 404 at the Go layer (defense in depth).
func TestGitHTTPRegistered_ServiceParamGate(t *testing.T) {
	root := t.TempDir()
	proj := "proj_recvpack"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "hello")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root)
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	tests := []struct {
		name string
		url  string
		want int
	}{
		{
			name: "receive-pack advertisement now routes (push enabled)",
			url:  "/api/v1/git/" + proj + ".git/info/refs?service=git-receive-pack",
			want: http.StatusOK,
		},
		{
			name: "no service param (dumb protocol) rejected",
			url:  "/api/v1/git/" + proj + ".git/info/refs",
			want: http.StatusNotFound,
		},
		{
			name: "unknown service param rejected",
			url:  "/api/v1/git/" + proj + ".git/info/refs?service=git-bogus",
			want: http.StatusNotFound,
		},
		{
			name: "upload-pack advertisement still passes",
			url:  "/api/v1/git/" + proj + ".git/info/refs?service=git-upload-pack",
			want: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Get(ts.URL + tc.url)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.url, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("url %s: expected %d, got %d", tc.url, tc.want, resp.StatusCode)
			}
		})
	}
}

// TestGitHTTPBackend_AuditKeyedPrincipal verifies that writeGitAudit sets
// Principal to the API key's ID when a non-nil *persistence.APIKey is stamped
// on the request context (Fix A1: cover the key != nil branch).
func TestGitHTTPBackend_AuditKeyedPrincipal(t *testing.T) {
	root := t.TempDir()
	proj := "proj_keyed"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "hello")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root)
	auditRepo := &fakeAdminAuditRepo{}
	srv.adminAuditRepo = auditRepo

	// Stamp a non-nil APIKey onto the request context, mirroring what
	// gitHTTPAuth does when authEnabled=true and the key validates.
	fakeKey := &persistence.APIKey{ID: "akey_x", ProjectID: proj, Name: "n"}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/git/"+proj+".git/info/refs?service=git-upload-pack", nil)
	req.SetPathValue("projectID", proj)
	ctx := context.WithValue(req.Context(), gitKeyCtxKey{}, fakeKey)
	ctx = context.WithValue(ctx, gitRemoteUserCtxKey{}, "n")

	rec := httptest.NewRecorder()
	srv.GitHTTPBackend(rec, req.WithContext(ctx))

	rows := auditRepo.snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	if rows[0].Principal != "akey_x" {
		t.Fatalf("audit Principal = %q, want %q", rows[0].Principal, "akey_x")
	}
}

// TestGitHTTPBackend_GitDisabled404 verifies FIX 1: when a wired registry
// reports the project's Git.Enabled=false (the default), the serving path
// returns 404 for info/refs, clone (upload-pack), and push (receive-pack)
// BEFORE any git exec — regardless of the on-disk repo existing.
func TestGitHTTPBackend_GitDisabled404(t *testing.T) {
	root := t.TempDir()
	proj := "proj_gitoff"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "hello")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	reg := gitRegistryWithProject(t, proj, false /* Git.Enabled=false */)
	srv := NewServer(WithConfig(minimalConfigWithWorkspace(root)), WithProjectRegistry(reg))
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	// info/refs (both services) and the upload-pack RPC must all 404.
	urls := []string{
		"/api/v1/git/" + proj + ".git/info/refs?service=git-upload-pack",
		"/api/v1/git/" + proj + ".git/info/refs?service=git-receive-pack",
	}
	for _, u := range urls {
		resp, err := ts.Client().Get(ts.URL + u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("url %s: expected 404 (Git disabled), got %d", u, resp.StatusCode)
		}
	}

	// A real clone must also fail (no exec, 404).
	dst := filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", dst).CombinedOutput()
	if err == nil {
		t.Fatalf("expected clone to fail when Git disabled, but it succeeded\n%s", out)
	}
}

// TestGitHTTPBackend_GitEnabledWorks verifies FIX 1: with a wired registry
// reporting Git.Enabled=true the serving path behaves normally (clone works).
func TestGitHTTPBackend_GitEnabledWorks(t *testing.T) {
	root := t.TempDir()
	proj := "proj_giton"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "README.md"), "hi")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	reg := gitRegistryWithProject(t, proj, true /* Git.Enabled=true */)
	srv := NewServer(WithConfig(minimalConfigWithWorkspace(root)), WithProjectRegistry(reg))
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("clone failed with Git enabled: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "README.md")); statErr != nil {
		t.Fatalf("expected README.md in clone: %v", statErr)
	}
}

// TestGitHTTPBackend_GitDisabledUnknownProject404 verifies the gate also 404s
// when the registry has NO such project (GetProject returns nil).
func TestGitHTTPBackend_GitDisabledUnknownProject404(t *testing.T) {
	root := t.TempDir()
	proj := "proj_present"
	repoDir := filepath.Join(root, "proj_absent")
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "x")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	// Registry knows proj_present (enabled), but the on-disk repo we target is
	// proj_absent — not in the registry → GetProject(nil) → 404.
	reg := gitRegistryWithProject(t, proj, true)
	srv := NewServer(WithConfig(minimalConfigWithWorkspace(root)), WithProjectRegistry(reg))
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/git/proj_absent.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for project absent from registry, got %d", resp.StatusCode)
	}
}

// TestGitHTTPBackend_AuditNoDoubleWrite verifies that a single request
// results in exactly one audit row (not two).
func TestGitHTTPBackend_AuditNoDoubleWrite(t *testing.T) {
	root := t.TempDir()
	proj := "proj_nodbl"
	repoDir := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", repoDir)
	gitWriteFile(t, filepath.Join(repoDir, "f.txt"), "x")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repoDir, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "init")

	srv := newTestServerWithWorkspaceRoot(t, root)
	auditRepo := &fakeAdminAuditRepo{}
	router := gitRegisteredRouter(srv, auditRepo)
	ts := httptest.NewServer(router)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/git/" + proj + ".git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	rows := auditRepo.snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", len(rows))
	}
}
