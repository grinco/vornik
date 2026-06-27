package ui

// Tests for the per-project filesystem-artifact management page at
// /ui/projects/{id}/artifacts (and its /raw + /delete companion
// endpoints). All tests stage a temp dir as the workspace root —
// nothing in this file touches the dev workspace tree.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/workspacelock"
)

// stageWorkspace builds a fake workspace tree:
//
//	<root>/<projectID>/artifacts/out/note.md
//	<root>/<projectID>/artifacts/research/quarterly.md
//	<root>/<projectID>/artifacts/.hidden        (must be hidden in list)
//	<root>/<projectID>/artifacts/data.bin       (binary at top-level)
//
// Returns the workspace root the tests pass to the handler.
func stageWorkspace(t *testing.T, projectID string) string {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, projectID, "artifacts")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "out"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "research"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "out", "note.md"), []byte("# hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "research", "quarterly.md"), []byte("Q3 report\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, ".hidden"), []byte("secret\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "data.bin"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644))
	// Mtime the .md file so the "humanise" path has a non-zero
	// reference point for the rendered table.
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(base, "out", "note.md"), past, past))
	return root
}

// TestProjectArtifacts_ListRendersFilesAndExcludesHidden — GET on
// the listing endpoint returns 200 with a table row per discovered
// file; hidden dotfiles and directories don't render as rows.
func TestProjectArtifacts_ListRendersFilesAndExcludesHidden(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifacts(w, req, "demo")

	resp := w.Result()
	body := w.Body.String()
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	// Header + every file row should show up.
	assert.Contains(t, body, "Artifacts")
	assert.Contains(t, body, "out/note.md")
	assert.Contains(t, body, "research/quarterly.md")
	assert.Contains(t, body, "data.bin")

	// Hidden files must never appear.
	assert.NotContains(t, body, ".hidden",
		"hidden dotfiles must not appear in the listing")

	// Per-row controls: a view link + a delete form for each entry.
	assert.Contains(t, body, "/ui/projects/demo/artifacts/raw?path=out%2Fnote.md")
	assert.Contains(t, body, `action="/ui/projects/demo/artifacts/delete"`)
	assert.Contains(t, body, `name="path" value="out/note.md"`)
}

// TestProjectArtifacts_ListPageSizeLimit_TruncatesAndExposesTotal —
// the shared pageSizeSelector requires the handler to (a) read
// ?limit=N, (b) clamp it via parsePageSize, (c) truncate the list,
// and (d) surface both the truncated count and the pre-truncate
// total. The "showing N of M" header copy depends on the latter
// being independent from len(Files).
func TestProjectArtifacts_ListPageSizeLimit_TruncatesAndExposesTotal(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "demo", "artifacts", "out")
	require.NoError(t, os.MkdirAll(base, 0o755))
	// 30 rows so all four allowlist values (10/20/50/100) trigger
	// different truncation behaviour.
	for i := 0; i < 30; i++ {
		path := filepath.Join(base, fmt.Sprintf("file-%02d.md", i))
		require.NoError(t, os.WriteFile(path, []byte("body\n"), 0o644))
	}
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts?limit=10", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifacts(w, req, "demo")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "showing 10 of 30",
		"header must show truncated count vs pre-truncate total")
	assert.Contains(t, body, "file-00.md", "first row in cap")
	assert.Contains(t, body, "file-09.md", "last row in cap")
	assert.NotContains(t, body, "file-10.md", "row outside cap must not render")

	// Out-of-allowlist value falls back to DefaultPageSize.
	req = httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts?limit=999", nil)
	w = httptest.NewRecorder()
	s.ProjectArtifacts(w, req, "demo")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(),
		fmt.Sprintf("showing %d of 30", DefaultPageSize),
		"out-of-allowlist limit must fall back to default")
}

// TestProjectArtifacts_ListEmptyState — no workspace dir on disk
// still returns 200 (operators land on a brand new project). Upgrade
// #5 (2026-05-26) replaces the bare "No artifacts" text with the
// shared emptyState component that carries a CTA to schedule a task.
// Pins that the CTA renders so the new empty surface guides
// operators toward the next action instead of just stating absence.
func TestProjectArtifacts_ListEmptyState(t *testing.T) {
	root := t.TempDir()
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet, "/projects/empty/artifacts", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifacts(w, req, "empty")

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "No artifacts", "headline copy must mention artifacts")
	assert.Contains(t, body, "Schedule a task", "empty-state must offer the CTA — operators shouldn't see a dead end")
	assert.Contains(t, body, "/ui/projects/empty/tasks/new", "CTA href must target this project's create-task page")
}

// TestProjectArtifacts_ListRejectsInvalidProjectID — projectID
// containing path separators must be rejected before any
// filesystem operation.
func TestProjectArtifacts_ListRejectsInvalidProjectID(t *testing.T) {
	root := t.TempDir()
	s := NewServer(WithProjectWorkspaceRoot(root))

	for _, bad := range []string{"../escape", "foo/bar", ""} {
		req := httptest.NewRequest(http.MethodGet, "/projects/"+bad+"/artifacts", nil)
		w := httptest.NewRecorder()
		s.ProjectArtifacts(w, req, bad)
		assert.NotEqual(t, http.StatusOK, w.Code, "expected non-200 for bad id %q", bad)
	}
}

// TestProjectArtifacts_ListWithoutWorkspaceRoot — when the daemon
// isn't configured with a workspace root, the page renders 503
// rather than silently returning an empty list (which would
// confuse operators expecting their out/ files).
func TestProjectArtifacts_ListWithoutWorkspaceRoot(t *testing.T) {
	s := NewServer() // no WithProjectWorkspaceRoot

	req := httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifacts(w, req, "demo")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestProjectArtifacts_ViewStreamsMarkdownAsText — GET /raw on a
// .md file streams the bytes with text/plain Content-Type so the
// browser renders it inline rather than triggering a download.
func TestProjectArtifacts_ViewStreamsMarkdownAsText(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet,
		"/projects/demo/artifacts/raw?path=out%2Fnote.md", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactView(w, req, "demo")

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")
	assert.Equal(t, "# hello\n", w.Body.String())
	// Inline disposition — no attachment.
	assert.NotContains(t, w.Header().Get("Content-Disposition"), "attachment")
}

// TestProjectArtifacts_ViewBinaryServesAsAttachment — a non-text
// extension forces an octet-stream attachment so the browser
// doesn't try to render arbitrary bytes inline.
func TestProjectArtifacts_ViewBinaryServesAsAttachment(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet,
		"/projects/demo/artifacts/raw?path=data.bin", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactView(w, req, "demo")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "data.bin")
}

// TestProjectArtifacts_ViewRejectsTraversal — any ?path containing
// `..` must be rejected with 400 before the filesystem is touched.
func TestProjectArtifacts_ViewRejectsTraversal(t *testing.T) {
	root := stageWorkspace(t, "demo")
	// Plant a sibling file the traversal might reach if validation fails.
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret"), []byte("nope"), 0o600))

	s := NewServer(WithProjectWorkspaceRoot(root))

	for _, bad := range []string{
		"../secret",
		"out/../../secret",
		"out/../../../etc/passwd",
	} {
		req := httptest.NewRequest(http.MethodGet,
			"/projects/demo/artifacts/raw?path="+url.QueryEscape(bad), nil)
		w := httptest.NewRecorder()
		s.ProjectArtifactView(w, req, "demo")
		assert.Equal(t, http.StatusBadRequest, w.Code, "expected 400 for %q", bad)
	}
}

// TestProjectArtifacts_ViewRejectsHiddenAndDirs — even when the
// path doesn't escape, viewing a directory or a hidden file is
// refused. Hidden files would let an attacker fish for secrets
// dropped beside the workspace (e.g. .env); directory reads would
// flood the response with an arbitrary blob.
func TestProjectArtifacts_ViewRejectsHiddenAndDirs(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	for _, bad := range []string{".hidden", "out"} {
		req := httptest.NewRequest(http.MethodGet,
			"/projects/demo/artifacts/raw?path="+url.QueryEscape(bad), nil)
		w := httptest.NewRecorder()
		s.ProjectArtifactView(w, req, "demo")
		assert.NotEqual(t, http.StatusOK, w.Code, "expected non-OK for %q", bad)
	}
}

// TestProjectArtifacts_ViewMissingFile — a path that doesn't exist
// returns 404. Distinct from the 400/traversal case so operator-
// facing error messages can be specific.
func TestProjectArtifacts_ViewMissingFile(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet,
		"/projects/demo/artifacts/raw?path=does/not/exist.md", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactView(w, req, "demo")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestProjectArtifacts_DeleteRemovesFile — a POST to /delete with
// a valid path removes the file off disk and redirects back to
// the listing. Subsequent GET no longer renders the row.
func TestProjectArtifacts_DeleteRemovesFile(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	form := url.Values{}
	form.Set("path", "out/note.md")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "demo")

	// Form POST → see-other redirect back to the list. The
	// handler appends `?ok=...` so the next render shows a
	// success banner; the path prefix is the load-bearing bit.
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), "/ui/projects/demo/artifacts"),
		"expected redirect back to artifacts list; got %q", w.Header().Get("Location"))

	// File is gone.
	_, err := os.Stat(filepath.Join(root, "demo", "artifacts", "out", "note.md"))
	assert.True(t, os.IsNotExist(err), "file should have been removed: err=%v", err)
}

func TestProjectArtifacts_DeleteCommitsArtifactRelativePath(t *testing.T) {
	root := stageWorkspace(t, "demo")
	projectDir := filepath.Join(root, "demo")
	runGit(t, projectDir, "init")
	runGit(t, projectDir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "artifacts")
	runGit(t, projectDir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "seed artifact")

	s := NewServer(WithProjectWorkspaceRoot(root))
	form := url.Values{}
	form.Set("path", "out/note.md")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	s.ProjectArtifactDelete(w, req, "demo")

	require.Equal(t, http.StatusSeeOther, w.Code)
	status := runGit(t, projectDir, "status", "--short")
	assert.Equal(t, "", strings.TrimSpace(status), "artifact deletion should be committed, not left as a dirty D entry")
	log := runGit(t, projectDir, "log", "-1", "--pretty=%s")
	assert.Contains(t, log, "ui: deleted artifact artifacts/out/note.md")
}

// TestProjectArtifacts_DeleteTakesWorkspaceLock asserts the delete path
// acquires the SAME shared per-project workspace lock the executor and the
// git-over-HTTPS handler take (lock-on-mutation). We inject a Locker, hold the
// project's exclusive lock from another goroutine, fire the delete, and assert
// it does NOT complete (file still present) while we hold the lock — then
// release and confirm the delete proceeds. This proves the handler blocks on
// the injected lock for that project ID.
func TestProjectArtifacts_DeleteTakesWorkspaceLock(t *testing.T) {
	root := stageWorkspace(t, "demo")
	lock := workspacelock.New()
	s := NewServer(WithProjectWorkspaceRoot(root), WithWorkspaceLock(lock))

	// Hold the project's exclusive lock so the delete must wait.
	releaseHeld := lock.Lock("demo")

	form := url.Values{}
	form.Set("path", "out/note.md")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.ProjectArtifactDelete(w, req, "demo")
		close(done)
	}()

	target := filepath.Join(root, "demo", "artifacts", "out", "note.md")

	// While we hold the lock the handler must NOT finish: it blocks on
	// lock.Lock("demo") before its git commit + the redirect write.
	// (The unlink itself happens before the lock; what the lock guards
	// is the git mutation, so the handler cannot complete until we
	// release.)
	select {
	case <-done:
		t.Fatal("delete completed while the project workspace lock was held — handler did not take the shared lock")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the lock; the handler must now proceed to completion.
	releaseHeld()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not complete after releasing the workspace lock")
	}
	wg.Wait()

	require.Equal(t, http.StatusSeeOther, w.Code)
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err), "file should have been removed after lock released: err=%v", err)
}

// TestProjectArtifacts_DeleteRejectsTraversal — POST with a
// traversal path must reject without touching disk.
func TestProjectArtifacts_DeleteRejectsTraversal(t *testing.T) {
	root := stageWorkspace(t, "demo")
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret"), []byte("nope"), 0o600))
	s := NewServer(WithProjectWorkspaceRoot(root))

	form := url.Values{}
	form.Set("path", "../secret")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "demo")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	// File outside the workspace must still exist.
	_, err := os.Stat(filepath.Join(root, "secret"))
	assert.NoError(t, err, "sibling file must survive a rejected traversal")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return string(out)
}

// TestProjectArtifacts_DeleteRejectsHiddenAndDirs — same containment
// as the view handler. Operators can't blow away the artifacts/
// subdirectory itself or any hidden config dropped beside it.
func TestProjectArtifacts_DeleteRejectsHiddenAndDirs(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	for _, bad := range []string{".hidden", "out"} {
		form := url.Values{}
		form.Set("path", bad)
		req := httptest.NewRequest(http.MethodPost,
			"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.ProjectArtifactDelete(w, req, "demo")
		assert.NotEqual(t, http.StatusSeeOther, w.Code, "should not delete %q", bad)
	}
	// .hidden and the out/ dir both still exist.
	_, err := os.Stat(filepath.Join(root, "demo", "artifacts", ".hidden"))
	assert.NoError(t, err)
	info, err := os.Stat(filepath.Join(root, "demo", "artifacts", "out"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestProjectArtifacts_DeleteRejectsGET — the delete endpoint is
// POST-only. A spurious GET (e.g. someone visiting the URL in
// their browser) must not delete anything.
func TestProjectArtifacts_DeleteRejectsGET(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet,
		"/projects/demo/artifacts/delete?path=out/note.md", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "demo")

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	_, err := os.Stat(filepath.Join(root, "demo", "artifacts", "out", "note.md"))
	assert.NoError(t, err, "GET must not remove the file")
}

// TestProjectArtifacts_RouterDispatchesToHandlers — wiring smoke
// test: the project router must dispatch /artifacts and its
// /raw + /delete children to the correct handlers. This catches
// the historical 404 the operator reported.
func TestProjectArtifacts_RouterDispatchesToHandlers(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	// list
	req := httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts", nil)
	w := httptest.NewRecorder()
	s.projectRouter(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "out/note.md")

	// view
	req = httptest.NewRequest(http.MethodGet,
		"/projects/demo/artifacts/raw?path=out%2Fnote.md", nil)
	w = httptest.NewRecorder()
	s.projectRouter(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// delete (POST)
	form := url.Values{}
	form.Set("path", "research/quarterly.md")
	req = httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	s.projectRouter(w, req)
	require.Equal(t, http.StatusSeeOther, w.Code)
	_, err := os.Stat(filepath.Join(root, "demo", "artifacts", "research", "quarterly.md"))
	assert.True(t, os.IsNotExist(err))
}

// TestProjectArtifacts_DetailLinksToArtifactsPage — the project
// detail page header must surface a link to the artifacts page
// so operators can find it. The whole point of this work was the
// "no UI affordance to inspect workspace files" report.
func TestProjectArtifacts_DetailLinksToArtifactsPage(t *testing.T) {
	s := NewServer()
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:                "demo",
			DisplayName:       "Demo",
			SwarmID:           "demo-swarm",
			DefaultWorkflowID: "demo-wf",
		},
		Swarm: &registry.Swarm{ID: "demo-swarm", LeadRole: "lead", Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "vornik-agent:latest"}},
		}},
	}
	w := httptest.NewRecorder()
	if err := s.templates.ExecuteTemplate(w, "project_detail.html", data); err != nil {
		t.Fatalf("template render failed: %v", err)
	}
	if !strings.Contains(w.Body.String(), `href="/ui/projects/demo/artifacts"`) {
		t.Errorf("project detail header must link to artifacts page. body excerpt:\n%s",
			excerptAround(w.Body.String(), "/artifacts", 80))
	}
}

// TestHumanizeSize covers the helper used by the listing template.
func TestHumanizeSize(t *testing.T) {
	cases := []struct {
		in  int64
		out string
	}{
		{0, "0 B"},
		{42, "42 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{int64(3.5 * 1024 * 1024), "3.5 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, humanizeSize(c.in), "size=%d", c.in)
	}
}

// TestListArtifactsCollectsRecursive — the underlying lister
// walks the artifacts tree, sorts by relative path, and skips
// hidden / directory entries.
func TestListArtifactsCollectsRecursive(t *testing.T) {
	root := stageWorkspace(t, "demo")
	rows, err := listArtifactFiles(root, "demo")
	require.NoError(t, err)

	var paths []string
	for _, r := range rows {
		paths = append(paths, r.RelPath)
	}
	// Hidden file omitted; everything else present, sorted.
	assert.Equal(t, []string{"data.bin", "out/note.md", "research/quarterly.md"}, paths)
	// Size + Mtime populated for at least one row.
	for _, r := range rows {
		if r.RelPath == "out/note.md" {
			assert.Equal(t, int64(len("# hello\n")), r.Size)
			assert.False(t, r.Mtime.IsZero())
		}
	}
}

// TestListArtifactsMissingRoot — listing a project with no
// artifacts/ dir on disk returns no rows + nil error. This is the
// "brand new project" path.
func TestListArtifactsMissingRoot(t *testing.T) {
	root := t.TempDir()
	rows, err := listArtifactFiles(root, "empty")
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestListArtifactsArtifactsPathIsFile — if the path that should
// hold the artifacts/ dir is actually a regular file, the lister
// reports a typed error rather than walking into it.
func TestListArtifactsArtifactsPathIsFile(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "demo")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "artifacts"), []byte("bogus"), 0o644))

	_, err := listArtifactFiles(root, "demo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// TestListArtifactsInvalidArgs covers the early returns for an
// empty workspace root and a bad project id — they both signal a
// configuration / programmer error and should fail fast without
// touching the filesystem.
func TestListArtifactsInvalidArgs(t *testing.T) {
	_, err := listArtifactFiles("", "demo")
	require.Error(t, err)

	_, err = listArtifactFiles(t.TempDir(), "../escape")
	require.Error(t, err)
}

// TestListArtifactsSkipsHiddenDirAndSymlinks — a hidden
// subdirectory has its whole subtree skipped (filepath.SkipDir
// branch); a symlink at the leaf is dropped at the d.Type() check.
func TestListArtifactsSkipsHiddenDirAndSymlinks(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "demo", "artifacts")
	require.NoError(t, os.MkdirAll(filepath.Join(base, ".cache", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, ".cache", "deep", "leaf.md"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "real.md"), []byte("ok"), 0o644))
	// Symlink to a real file outside the workspace — must not surface.
	external := filepath.Join(root, "external.md")
	require.NoError(t, os.WriteFile(external, []byte("bad"), 0o644))
	if err := os.Symlink(external, filepath.Join(base, "link.md")); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}
	rows, err := listArtifactFiles(root, "demo")
	require.NoError(t, err)
	var paths []string
	for _, r := range rows {
		paths = append(paths, r.RelPath)
	}
	// .cache/* skipped wholesale; link.md (symlink) dropped.
	assert.Equal(t, []string{"real.md"}, paths)
}

// TestResolveArtifactPath covers the explicit error branches that
// the integration tests don't reach directly — empty rel path and
// a path that cleans to ".".
func TestResolveArtifactPath(t *testing.T) {
	root := t.TempDir()
	// happy path
	require.NoError(t, os.MkdirAll(filepath.Join(root, "p", "artifacts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "p", "artifacts", "a.md"), []byte("x"), 0o644))

	full, err := resolveArtifactPath(root, "p", "a.md")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "p", "artifacts", "a.md"), full)

	for _, bad := range []string{"", "   ", ".", "./", "/"} {
		_, err := resolveArtifactPath(root, "p", bad)
		assert.Error(t, err, "expected error for %q", bad)
	}
	// empty workspace root
	_, err = resolveArtifactPath("", "p", "a.md")
	assert.Error(t, err)
	// bad project id
	_, err = resolveArtifactPath(root, "../escape", "a.md")
	assert.Error(t, err)
}

// TestProjectArtifacts_DeleteWithoutWorkspaceRoot — when the
// daemon's project_workspace_path isn't configured the delete
// endpoint returns 503 even on POST. Mirrors the list-handler
// guard so a half-wired deployment never silently no-ops a delete.
func TestProjectArtifacts_DeleteWithoutWorkspaceRoot(t *testing.T) {
	s := NewServer()
	form := url.Values{}
	form.Set("path", "out/note.md")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "demo")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestProjectArtifacts_DeleteMissingPath — POST without a path
// field is a bad request. Same shape for an empty/whitespace
// value (the handler trims before checking).
func TestProjectArtifacts_DeleteMissingPath(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	for _, payload := range []string{"", "path=", "path=%20%20"} {
		req := httptest.NewRequest(http.MethodPost,
			"/projects/demo/artifacts/delete", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.ProjectArtifactDelete(w, req, "demo")
		assert.Equal(t, http.StatusBadRequest, w.Code, "expected 400 for payload %q", payload)
	}
}

// TestProjectArtifacts_DeleteMissingFile — POST against a file
// that doesn't exist returns 404 (so the operator UI can surface
// a "row already gone" message rather than a noisy 500).
func TestProjectArtifacts_DeleteMissingFile(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	form := url.Values{}
	form.Set("path", "out/ghost.md")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "demo")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestProjectArtifacts_DeleteBadProjectID — projectID with a
// separator is rejected before the form is parsed.
func TestProjectArtifacts_DeleteBadProjectID(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodPost,
		"/projects/foo%2Fbar/artifacts/delete", strings.NewReader("path=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "foo/bar")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestProjectArtifacts_ViewMissingPath — GET on /raw without a
// path query value is a 400 so the error message is specific
// rather than a generic 404.
func TestProjectArtifacts_ViewMissingPath(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts/raw", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactView(w, req, "demo")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestProjectArtifacts_ViewWithoutWorkspaceRoot — symmetry with
// the list + delete handlers' 503 when project_workspace_path is
// unset.
func TestProjectArtifacts_ViewWithoutWorkspaceRoot(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet,
		"/projects/demo/artifacts/raw?path=out/note.md", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactView(w, req, "demo")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestProjectArtifacts_ViewBadProjectID — separator in projectID
// hits the validator before the disk is touched.
func TestProjectArtifacts_ViewBadProjectID(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	req := httptest.NewRequest(http.MethodGet,
		"/projects/foo%2Fbar/artifacts/raw?path=x", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifactView(w, req, "foo/bar")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestUrlQueryEscape covers the small inline encoder.
func TestUrlQueryEscape(t *testing.T) {
	cases := []struct{ in, out string }{
		{"simple", "simple"},
		{"a b", "a+b"},
		{"a&b", "a%26b"},
		{"a=b", "a%3Db"},
		{"a?b#c", "a%3Fb%23c"},
		{"100%", "100%25"},
		{"line\nbreak", "line%0Abreak"},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, urlQueryEscape(c.in), "input %q", c.in)
	}
}

// TestProjectArtifacts_ListSurfacesListerError — when the
// project's artifacts/ path is actually a file (e.g. someone
// renamed a directory by accident) the handler returns 500
// rather than panicking. Covers the listArtifactFiles error
// branch end-to-end.
func TestProjectArtifacts_ListSurfacesListerError(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "demo")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "artifacts"), []byte("bogus"), 0o644))

	s := NewServer(WithProjectWorkspaceRoot(root))
	req := httptest.NewRequest(http.MethodGet, "/projects/demo/artifacts", nil)
	w := httptest.NewRecorder()
	s.ProjectArtifacts(w, req, "demo")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// TestProjectArtifacts_DeleteRefusesSymlinkOutOfRoot — a symlink
// at the leaf pointing OUTSIDE the workspace root must be rejected
// by safepath's symlink-escape check before any unlink happens.
// The target file must survive untouched.
func TestProjectArtifacts_DeleteRefusesSymlinkOutOfRoot(t *testing.T) {
	root := stageWorkspace(t, "demo")
	external := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(external, []byte("untouchable"), 0o600))
	link := filepath.Join(root, "demo", "artifacts", "out", "escape.md")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	s := NewServer(WithProjectWorkspaceRoot(root))

	form := url.Values{}
	form.Set("path", "out/escape.md")
	req := httptest.NewRequest(http.MethodPost,
		"/projects/demo/artifacts/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ProjectArtifactDelete(w, req, "demo")

	// The handler must NOT have followed the symlink into the
	// external file. Concretely: the external file still exists.
	_, err := os.Stat(external)
	require.NoError(t, err, "external file must survive a symlink-escape attempt")
	assert.NotEqual(t, http.StatusSeeOther, w.Code,
		"should not have succeeded for an out-of-root symlink")
}

// TestProjectArtifactsRouterRejectsNonPostDelete — the router
// must serve a 405 for GET/PUT/etc. on /artifacts/delete, not
// fall through to ProjectDetail. Pins the routing-table guard.
func TestProjectArtifactsRouterRejectsNonPostDelete(t *testing.T) {
	root := stageWorkspace(t, "demo")
	s := NewServer(WithProjectWorkspaceRoot(root))

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/projects/demo/artifacts/delete?path=x", nil)
		w := httptest.NewRecorder()
		s.projectRouter(w, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "method=%s", method)
	}
}
