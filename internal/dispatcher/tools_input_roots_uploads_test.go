// Regression tests for incident-telegram-upload-input-roots-20260712.
//
// Since the 327e8f8a security gate (2026-06-03), create_task's
// input_files confinement allowed only os.TempDir() + the artifact
// store base path — but Telegram has saved uploads into
// <projectWorkspacePath>/<projectID>/uploads/ since af0a6036
// (2026-04-14) whenever an active project is set. Every such upload
// was rejected ("outside allowed roots"), the dispatcher LLM
// confabulated an "email/webchat only" story to the operator, and
// file→RAG ingestion via Telegram silently broke. A second latent
// bug made even the artifact-store root vanish: inputFileSourceRoots
// type-asserts basePathProvider, which *artifacts.Store did not
// implement (only artifacts.LocalBackend did), so prod logs showed
// allowed_roots=["/tmp"].
//
// These tests pin the fix: the per-project uploads dir is an allowed
// literal-path root, OTHER projects' uploads dirs stay rejected, and
// the real *artifacts.Store surfaces its base path into the roots.
package dispatcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence/mocks"
)

// isolateTempDir points os.TempDir() at a dedicated directory so a
// t.TempDir()-hosted workspace root is provably OUTSIDE the TempDir
// allow-list entry — otherwise every test path would pass via the
// TempDir root and the uploads-root behaviour under test would never
// be exercised.
func isolateTempDir(t *testing.T) {
	t.Helper()
	tmpDir := filepath.Join(t.TempDir(), "faketmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("mkdir faketmp: %v", err)
	}
	t.Setenv("TMPDIR", tmpDir)
	if got := os.TempDir(); got != tmpDir {
		t.Skipf("os.TempDir() = %q ignores TMPDIR on this platform", got)
	}
}

// TestCreateTask_AcceptsProjectUploadsPath — the exact incident shape:
// a Telegram upload saved under <projectWorkspacePath>/<project>/uploads/
// passed as a literal path in input_files must be snapshotted, not
// rejected.
func TestCreateTask_AcceptsProjectUploadsPath(t *testing.T) {
	isolateTempDir(t)
	workspaceRoot := t.TempDir()
	uploadsDir := filepath.Join(workspaceRoot, "snake", "uploads")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	// Filename with spaces, matching the real incident upload.
	upload := filepath.Join(uploadsDir, "AI-first Operating Model.pdf")
	if err := os.WriteFile(upload, []byte("pdf-bytes"), 0o644); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	store := &confineStubStore{}
	te := &ToolExecutor{
		registry:             confineRegistry(t),
		taskRepo:             &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactStore:        store,
		projectWorkspacePath: workspaceRoot,
		logger:               zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "snake",
		"type":        "feature",
		"prompt":      "ingest this",
		"input_files": []string{upload},
	}
	argsJSON, _ := json.Marshal(args)
	res := te.createTask(context.Background(), string(argsJSON), "snake", []string{"snake"}, 0)

	if len(store.sources) != 1 {
		t.Fatalf("project-uploads path should be snapshotted, got %d StoreInput calls: %+v (result: %s)",
			len(store.sources), store.sources, res.Content)
	}
}

// TestCreateTask_RejectsPathsOutsideProjectUploads — the uploads root
// is scoped: a sibling file in the project workspace but OUTSIDE
// uploads/, and another project's uploads dir, both stay rejected.
func TestCreateTask_RejectsPathsOutsideProjectUploads(t *testing.T) {
	isolateTempDir(t)
	workspaceRoot := t.TempDir()
	for _, p := range []string{
		filepath.Join(workspaceRoot, "snake", "uploads"),
		filepath.Join(workspaceRoot, "snake", "repo"),
		filepath.Join(workspaceRoot, "otherproj", "uploads"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	workspaceFile := filepath.Join(workspaceRoot, "snake", "repo", "secrets.env")
	otherProjectFile := filepath.Join(workspaceRoot, "otherproj", "uploads", "doc.pdf")
	for _, f := range []string{workspaceFile, otherProjectFile} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", f, err)
		}
	}

	for name, path := range map[string]string{
		"workspace file outside uploads": workspaceFile,
		"other project's uploads":        otherProjectFile,
	} {
		store := &confineStubStore{}
		te := &ToolExecutor{
			registry:             confineRegistry(t),
			taskRepo:             &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
			artifactStore:        store,
			projectWorkspacePath: workspaceRoot,
			logger:               zerolog.Nop(),
		}
		args := map[string]any{
			"project_id":  "snake",
			"type":        "feature",
			"prompt":      "read",
			"input_files": []string{path},
		}
		argsJSON, _ := json.Marshal(args)
		res := te.createTask(context.Background(), string(argsJSON), "snake", []string{"snake"}, 0)
		if len(store.sources) != 0 {
			t.Errorf("%s: path %q reached StoreInput, want rejection", name, path)
		}
		if res.Content == "" {
			t.Errorf("%s: expected a rejection message", name)
		}
	}
}

// TestInputFileSourceRoots_IncludesRealArtifactStoreBasePath — the
// latent half of the incident: the wired *artifacts.Store must satisfy
// basePathProvider so the store base survives as an allowed root
// (prod logs showed allowed_roots=["/tmp"] because it didn't).
func TestInputFileSourceRoots_IncludesRealArtifactStoreBasePath(t *testing.T) {
	base := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(base))
	if err != nil {
		t.Fatalf("artifacts.New: %v", err)
	}
	te := &ToolExecutor{artifactStore: store, logger: zerolog.Nop()}

	roots := te.inputFileSourceRoots("snake")
	want := resolveRootForContainment(base)
	for _, r := range roots {
		if r == want {
			return
		}
	}
	t.Fatalf("roots %v missing artifact store base %q", roots, want)
}

// TestInputFileSourceRoots_UploadsRootRequiresProject — no active
// project (or no workspace path) must not widen the roots.
func TestInputFileSourceRoots_UploadsRootRequiresProject(t *testing.T) {
	workspaceRoot := t.TempDir()
	te := &ToolExecutor{projectWorkspacePath: workspaceRoot, logger: zerolog.Nop()}

	for _, projectID := range []string{"", "../escape"} {
		roots := te.inputFileSourceRoots(projectID)
		for _, r := range roots {
			if r == workspaceRoot || strings.HasPrefix(r, workspaceRoot+string(filepath.Separator)) {
				t.Errorf("projectID=%q: unexpected workspace-derived root %q", projectID, r)
			}
		}
	}
}
