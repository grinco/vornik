package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// TestAllowedStagingRoots_TempDir — the OS temp dir is always
// allowed; previous-step workspaces and /tmp Telegram fallbacks land
// here, and dropping it would break every existing artifact pipeline.
func TestAllowedStagingRoots_TempDir(t *testing.T) {
	roots := allowedStagingRoots("", "", "")
	if len(roots) != 1 {
		t.Fatalf("expected 1 root (just tmp), got %d: %v", len(roots), roots)
	}
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	if roots[0] != tmp && roots[0] != filepath.Clean(os.TempDir()) {
		t.Fatalf("first root should be tmpdir, got %q", roots[0])
	}
}

// TestAllowedStagingRoots_ProjectUploads — when a project workspace
// path and project ID are supplied, the per-project uploads/ dir
// joins the allowlist. Legacy path; modern flows route through the
// artifacts store, but in-flight retries from before the migration
// may still reference workspace uploads.
func TestAllowedStagingRoots_ProjectUploads(t *testing.T) {
	dir := t.TempDir()
	uploads := filepath.Join(dir, "myproj", "uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := allowedStagingRoots(dir, "myproj", "")
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots (tmp + uploads), got %d: %v", len(roots), roots)
	}
	resolved, _ := filepath.EvalSymlinks(uploads)
	found := false
	for _, r := range roots {
		if r == resolved || r == uploads {
			found = true
		}
	}
	if !found {
		t.Fatalf("uploads dir missing from roots: %v (wanted %q)", roots, resolved)
	}
}

// TestAllowedStagingRoots_ArtifactsPath — durable INPUT artifacts
// land here. Without this entry the dispatcher's snapshot-and-rewrite
// flow produces task payloads pointing into the artifacts store, and
// every staging attempt would be rejected at the security check.
func TestAllowedStagingRoots_ArtifactsPath(t *testing.T) {
	artDir := t.TempDir()
	roots := allowedStagingRoots("", "", artDir)
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots (tmp + artifacts), got %d: %v", len(roots), roots)
	}
	resolved, _ := filepath.EvalSymlinks(artDir)
	found := false
	for _, r := range roots {
		if r == resolved || r == artDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifacts root missing from roots: %v (wanted %q)", roots, resolved)
	}
}

// TestResolveStagingSrc_RejectsRelativeEscape pins the audit fix:
// a relative path like "./../../etc/passwd" used to bypass the
// allowed-roots gate (filepath.IsAbs returned false → check skipped
// → os.ReadFile resolved against the daemon's CWD). resolveStagingSrc
// always normalises to absolute first so the gate runs unconditionally.
func TestResolveStagingSrc_RejectsRelativeEscape(t *testing.T) {
	tmp := t.TempDir()
	roots := []string{tmp}
	for _, candidate := range []string{
		"../../etc/passwd",
		"./../../etc/passwd",
		"etc/passwd",
		"sneaky.txt", // CWD-relative; almost certainly outside tmp
	} {
		_, ok := resolveStagingSrc(candidate, roots)
		if ok {
			t.Errorf("resolveStagingSrc(%q) accepted; relative paths must be rejected outside roots", candidate)
		}
	}
}

// TestResolveStagingSrc_AcceptsAbsoluteUnderRoot confirms the happy
// path: an absolute path inside an allowed root is admitted and the
// returned canonical path matches the input.
func TestResolveStagingSrc_AcceptsAbsoluteUnderRoot(t *testing.T) {
	tmp := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(tmp)
	roots := []string{resolved}
	src := filepath.Join(tmp, "ok.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := resolveStagingSrc(src, roots)
	if !ok {
		t.Fatalf("resolveStagingSrc(%q, %v) rejected an in-root path", src, roots)
	}
	if got != src {
		t.Fatalf("got %q, want %q", got, src)
	}
}

// TestResolveStagingSrc_RejectsAbsoluteOutsideRoot confirms an
// absolute path that doesn't live under an allowed root is rejected
// — the legacy behaviour for IsAbs paths, preserved.
func TestResolveStagingSrc_RejectsAbsoluteOutsideRoot(t *testing.T) {
	tmp := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(tmp)
	if _, ok := resolveStagingSrc("/etc/passwd", []string{resolved}); ok {
		t.Fatalf("/etc/passwd must not pass the gate when only %q is allowed", resolved)
	}
}

// TestResolveStagingSrc_EmptyPathRejected — defensive: an empty
// srcPath is a config or agent bug and must not slip through to
// os.ReadFile, which would error opaquely.
func TestResolveStagingSrc_EmptyPathRejected(t *testing.T) {
	if _, ok := resolveStagingSrc("", []string{t.TempDir()}); ok {
		t.Fatal("empty srcPath must be rejected")
	}
}

// TestAllowedStagingRoots_NonExistentUploads — the staging code is
// allowed to run before the uploads/ dir has been created (the
// Telegram handler creates it on first download). The allowlist
// must still include the path so that subsequent access succeeds —
// EvalSymlinks fails on a missing dir but we fall back to Clean.
func TestAllowedStagingRoots_NonExistentUploads(t *testing.T) {
	dir := t.TempDir()
	roots := allowedStagingRoots(dir, "newproj", "")
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots even when uploads/ missing, got %d: %v", len(roots), roots)
	}
}

// TestAllowedStagingRoots_AllThree — the production wiring: project
// workspace + artifacts path. The full set must include all three
// roots, and pathUnderAny must accept paths under each.
func TestAllowedStagingRoots_AllThree(t *testing.T) {
	wsDir := t.TempDir()
	artDir := t.TempDir()
	uploads := filepath.Join(wsDir, "p", "uploads")
	_ = os.MkdirAll(uploads, 0o755)
	roots := allowedStagingRoots(wsDir, "p", artDir)
	if len(roots) != 3 {
		t.Fatalf("expected 3 roots, got %d: %v", len(roots), roots)
	}

	tmpFile := filepath.Join(os.TempDir(), "x.dat")
	uploadFile := filepath.Join(uploads, "y.dat")
	artFile := filepath.Join(artDir, "p", "inputs", "abc", "z.dat")
	for _, p := range []string{tmpFile, uploadFile, artFile} {
		// Resolve as the staging code does so symlink farms don't
		// flake the assertion.
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			p = filepath.Join(resolved, filepath.Base(p))
		}
		if !pathUnderAny(p, roots) {
			t.Errorf("path %q should be allowed, roots=%v", p, roots)
		}
	}
}

// TestPathUnderAny — exact match and prefixed paths both count;
// sibling paths (substring matches that aren't proper subdirs) must
// NOT be accepted, or "/tmpfoo/x" would slip past a "/tmp" root.
func TestPathUnderAny(t *testing.T) {
	roots := []string{"/tmp", "/var/lib/vornik/uploads"}
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp", true},
		{"/tmp/photo.jpg", true},
		{"/tmp/sub/nested.txt", true},
		{"/var/lib/vornik/uploads", true},
		{"/var/lib/vornik/uploads/x.jpg", true},
		{"/tmpfoo/x", false},                 // sibling — not a subpath
		{"/var/lib/vornik/uploads-x", false}, // sibling
		{"/etc/passwd", false},               // outside
		{"/var/lib/vornik", false},           // parent of an allowed root
		{"/opt/vornik/uploads/x.jpg", false}, // wrong prefix
	}
	for _, tc := range cases {
		got := pathUnderAny(tc.path, roots)
		if got != tc.want {
			t.Errorf("pathUnderAny(%q): got %v want %v", tc.path, got, tc.want)
		}
	}
}

// TestStageInputArtifacts_OutputClassReachesNextStep is the regression
// test for task e9a5 — researcher output must reach the next step.
//
// In a multi-step STATIC workflow (researcher → writer) the researcher
// writes artifacts/out/research.md inside its EPHEMERAL per-step
// container. persistArtifacts harvests it into the durable artifact
// store, then hands the store-backed {name, sourcePath} forward tagged
// class="output". The next step's staging must materialise that file at
// <workspaceDir>/artifacts/out/<name> (where the role reads from, NOT
// artifacts/in/) and rewrite art["path"] to the container view. Pre-fix
// the bridge carried the agent's container path with no host sourcePath,
// resolveStagingSrc rejected it, and the file was silently dropped — the
// writer reported research.md missing.
func TestStageInputArtifacts_OutputClassReachesNextStep(t *testing.T) {
	// Store root = an allowed staging root (artifactStoragePath).
	storeRoot := t.TempDir()
	resolvedStore, _ := filepath.EvalSymlinks(storeRoot)
	// The store-backed source file the previous step's output was
	// persisted to.
	storedSrc := filepath.Join(storeRoot, "proj", "outputs", "exec1", "research.md")
	if err := os.MkdirAll(filepath.Dir(storedSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "# research findings\n"
	if err := os.WriteFile(storedSrc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	workspaceDir := t.TempDir()
	allowedRoots := []string{resolvedStore}

	e := &Executor{logger: zerolog.Nop()}
	art := map[string]string{
		"name":       "research.md",
		"sourcePath": storedSrc,
		"class":      "output",
	}
	if err := e.stageInputArtifacts(workspaceDir, []map[string]string{art}, allowedRoots); err != nil {
		t.Fatalf("stageInputArtifacts errored: %v", err)
	}

	// File must land in artifacts/out/ (where the next role reads) and
	// be readable with the original bytes.
	staged := filepath.Join(workspaceDir, "artifacts", "out", "research.md")
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("output-class artifact not staged into artifacts/out/: %v", err)
	}
	if string(got) != body {
		t.Fatalf("staged content mismatch: got %q want %q", got, body)
	}
	// Container-view path must be rewritten to the out/ tree.
	if art["path"] != "/app/workspace/artifacts/out/research.md" {
		t.Fatalf("art[path] = %q, want /app/workspace/artifacts/out/research.md", art["path"])
	}
}

// TestStageInputArtifacts_TaskUploadLandsInIn pins the existing
// task-upload contract: an artifact with no class and a host sourcePath
// under an allowed root lands in artifacts/in/ (NOT out/) and rewrites to
// the in/ container path. Must not regress the upload pipeline.
func TestStageInputArtifacts_TaskUploadLandsInIn(t *testing.T) {
	uploadRoot := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(uploadRoot)
	src := filepath.Join(uploadRoot, "spec.pdf")
	if err := os.WriteFile(src, []byte("%PDF upload"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspaceDir := t.TempDir()
	e := &Executor{logger: zerolog.Nop()}
	art := map[string]string{
		"name":       "spec.pdf",
		"sourcePath": src,
	}
	if err := e.stageInputArtifacts(workspaceDir, []map[string]string{art}, []string{resolved}); err != nil {
		t.Fatalf("stageInputArtifacts errored: %v", err)
	}

	staged := filepath.Join(workspaceDir, "artifacts", "in", "spec.pdf")
	if _, err := os.ReadFile(staged); err != nil {
		t.Fatalf("task-upload artifact not staged into artifacts/in/: %v", err)
	}
	if art["path"] != "/app/workspace/artifacts/in/spec.pdf" {
		t.Fatalf("art[path] = %q, want /app/workspace/artifacts/in/spec.pdf", art["path"])
	}
	// And it must NOT have leaked into out/.
	if _, err := os.Stat(filepath.Join(workspaceDir, "artifacts", "out", "spec.pdf")); !os.IsNotExist(err) {
		t.Fatalf("task upload must not land in artifacts/out/")
	}
}

// TestStageInputArtifacts_UnresolvableOutputDropped proves the security
// guard survives: an output-class artifact whose sourcePath is outside
// every allowed root is dropped (never staged), preserving the
// resolveStagingSrc allowlist that quietly rejected the agent's raw
// container path pre-fix.
func TestStageInputArtifacts_UnresolvableOutputDropped(t *testing.T) {
	// sourcePath lives outside the only allowed root.
	outside := t.TempDir()
	evil := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(evil, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	allowedRoot := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(allowedRoot)

	workspaceDir := t.TempDir()
	e := &Executor{logger: zerolog.Nop()}
	art := map[string]string{
		"name":       "secret.md",
		"sourcePath": evil,
		"class":      "output",
	}
	if err := e.stageInputArtifacts(workspaceDir, []map[string]string{art}, []string{resolved}); err != nil {
		t.Fatalf("stageInputArtifacts errored: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspaceDir, "artifacts", "out", "secret.md")); !os.IsNotExist(err) {
		t.Fatalf("artifact outside allowed roots must be dropped, not staged")
	}
}
