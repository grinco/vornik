// Regression tests for the create_task input_files confinement fix.
//
// create_task accepts model-controlled `input_files`. A literal host
// path there is a read primitive: createTask snapshots the file into
// the project artifact store via StoreInput, and
// read_artifact/send_artifact then hand the bytes straight back to the
// model — without ever crossing the worker's container-staging guard.
// The dispatcher must therefore confine the SOURCE path itself,
// rejecting any literal path outside the allow-list of legitimate
// input roots so a prompt can't exfiltrate /etc/passwd or the daemon's
// secrets dir.
//
// Pre-fix these tests fail: the unconfined path reaches StoreInput.
package dispatcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// confineStubStore records every StoreInput call so the test can
// assert that a rejected path NEVER reaches the artifact store.
type confineStubStore struct {
	sources []string
}

func (s *confineStubStore) StoreInput(_ context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error) {
	s.sources = append(s.sources, sourcePath)
	return &persistence.Artifact{
		ID:          "art-" + name,
		ProjectID:   projectID,
		Name:        name,
		StoragePath: "/store/" + name,
	}, nil
}

func (s *confineStubStore) Retrieve(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}

func confineRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	return mustRegistry(t,
		[]registry.Project{{ID: "snake", SwarmID: "s", DefaultWorkflowID: "w"}},
		oneSwarm("s"), oneWorkflow("w"))
}

// TestCreateTask_RejectsLiteralPathOutsideAllowedRoots — a literal
// absolute path that escapes every allowed root (here, a secrets dir)
// is rejected with an operator-readable error and NEVER reaches
// StoreInput. This is the core exfiltration guard.
func TestCreateTask_RejectsLiteralPathOutsideAllowedRoots(t *testing.T) {
	secretsDir := t.TempDir()
	secretFile := filepath.Join(secretsDir, "admin-key.txt")
	if err := os.WriteFile(secretFile, []byte("super-secret"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	// Allowed roots are an unrelated upload dir — NOT the secrets dir.
	uploadRoot := t.TempDir()

	store := &confineStubStore{}
	te := &ToolExecutor{
		registry:          confineRegistry(t),
		taskRepo:          &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactStore:     store,
		allowedInputRoots: []string{uploadRoot},
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "snake",
		"type":        "feature",
		"prompt":      "read this",
		"input_files": []string{secretFile},
	}
	argsJSON, _ := json.Marshal(args)
	res := te.createTask(context.Background(), string(argsJSON), "snake", []string{"snake"}, 0)

	if len(store.sources) != 0 {
		t.Fatalf("secret path reached StoreInput (exfiltration!): %+v", store.sources)
	}
	if res.Content == "" {
		t.Fatal("expected an operator-readable rejection, got empty result")
	}
	// The task must not be created from a rejected input.
	last := te.taskRepo.(*capturingTaskRepo).last
	if last != nil {
		t.Errorf("task should not be created when an input is rejected, got %+v", last)
	}
}

// TestCreateTask_AcceptsLiteralPathUnderAllowedRoot — positive
// control: a literal path that DOES live under an allowed root is
// snapshotted normally, so the fix doesn't regress valid uploads.
func TestCreateTask_AcceptsLiteralPathUnderAllowedRoot(t *testing.T) {
	uploadRoot := t.TempDir()
	upload := filepath.Join(uploadRoot, "doc.pdf")
	if err := os.WriteFile(upload, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	store := &confineStubStore{}
	te := &ToolExecutor{
		registry:          confineRegistry(t),
		taskRepo:          &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactStore:     store,
		allowedInputRoots: []string{uploadRoot},
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "snake",
		"type":        "feature",
		"prompt":      "process this",
		"input_files": []string{upload},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "snake", []string{"snake"}, 0)

	if len(store.sources) != 1 {
		t.Fatalf("expected the allowed upload to be snapshotted, got %d calls: %+v",
			len(store.sources), store.sources)
	}
}

// TestCreateTask_ArtifactIDBypassesConfinement — an artifact-ID entry
// (the email-attachment path) resolves to a real StoragePath via
// artifactRepo and is trusted by construction, so it passes even
// though that StoragePath sits outside the literal-path allow-list.
// Guards the 2026-05-21 attachment-plumbing behaviour.
func TestCreateTask_ArtifactIDBypassesConfinement(t *testing.T) {
	// StoragePath deliberately outside any allowed root.
	repo := &stubArtifactRepoForResolve{
		byID: map[string]*persistence.Artifact{
			"email-att-deadbeef": {
				ID:          "email-att-deadbeef",
				Name:        "book.epub",
				StoragePath: "/var/lib/vornik/artifacts/snake/inputs/x/book.epub",
			},
		},
	}
	store := &confineStubStore{}
	te := &ToolExecutor{
		registry:          confineRegistry(t),
		taskRepo:          &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactRepo:      repo,
		artifactStore:     store,
		allowedInputRoots: []string{t.TempDir()}, // does NOT include the StoragePath
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "snake",
		"type":        "feature",
		"prompt":      "summarise",
		"input_files": []string{"email-att-deadbeef"},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "snake", []string{"snake"}, 0)

	if len(store.sources) != 1 {
		t.Fatalf("artifact-ID entry should resolve + snapshot, got %d calls: %+v",
			len(store.sources), store.sources)
	}
	want := "/var/lib/vornik/artifacts/snake/inputs/x/book.epub"
	if store.sources[0] != want {
		t.Errorf("StoreInput source = %q; want resolved StoragePath %q", store.sources[0], want)
	}
}

// TestCreateTask_DefaultRootsAllowTempDir — when allowedInputRoots is
// unset, the dispatcher derives os.TempDir() as a baseline root (where
// channel uploads land). A path under it passes; a sibling path
// outside it is still rejected.
func TestCreateTask_DefaultRootsAllowTempDir(t *testing.T) {
	upload := filepath.Join(os.TempDir(), "confine-default-upload.bin")
	if err := os.WriteFile(upload, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	defer func() { _ = os.Remove(upload) }()

	store := &confineStubStore{}
	te := &ToolExecutor{
		registry:      confineRegistry(t),
		taskRepo:      &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}},
		artifactStore: store,
		// allowedInputRoots intentionally unset → default derivation.
		logger: zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "snake",
		"type":        "feature",
		"prompt":      "ok",
		"input_files": []string{upload},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "snake", []string{"snake"}, 0)
	if len(store.sources) != 1 {
		t.Fatalf("TempDir upload should pass under default roots, got %d: %+v",
			len(store.sources), store.sources)
	}

	// Now an outside-TempDir literal path with default roots is
	// rejected. Build the path under the OS root so it is provably
	// outside os.TempDir() regardless of how TMPDIR is configured on
	// the build host (t.TempDir() itself lives under os.TempDir()).
	tmpResolved := resolveRootForContainment(os.TempDir())
	outside := filepath.Join(string(filepath.Separator), "etc", "vornik-confine-probe-secret.txt")
	if _, under := confineInputFileSource(outside, []string{tmpResolved}); under {
		t.Skipf("probe path %q unexpectedly under os.TempDir() %q", outside, tmpResolved)
	}
	// No file is written: the confinement gate runs before any read,
	// so the reject must fire on the path alone.
	store2 := &confineStubStore{}
	te.artifactStore = store2
	args2 := map[string]any{
		"project_id":  "snake",
		"type":        "feature",
		"prompt":      "no",
		"input_files": []string{outside},
	}
	argsJSON2, _ := json.Marshal(args2)
	res := te.createTask(context.Background(), string(argsJSON2), "snake", []string{"snake"}, 0)
	if len(store2.sources) != 0 {
		t.Fatalf("outside-TempDir path reached StoreInput under default roots: %+v", store2.sources)
	}
	if res.Content == "" {
		t.Error("expected rejection message for outside-TempDir path")
	}
}
