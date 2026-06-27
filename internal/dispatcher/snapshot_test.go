package dispatcher

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// fakeInputStore is a minimal InputArtifactStore for the dispatcher
// snapshot tests. Records every snapshot call so the test can assert
// on what the dispatcher passed in, and emits a deterministic storage
// path so the rewritten payload is easy to verify.
type fakeInputStore struct {
	calls  []fakeStoreCall
	failOn string // when non-empty, return an error if name == failOn
	prefix string // storage path prefix to attach in the returned artifact
	nextID int
}

type fakeStoreCall struct {
	ProjectID  string
	Name       string
	SourcePath string
}

func (f *fakeInputStore) StoreInput(_ context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error) {
	f.calls = append(f.calls, fakeStoreCall{ProjectID: projectID, Name: name, SourcePath: sourcePath})
	if f.failOn != "" && name == f.failOn {
		return nil, errInjectedSnapshotFailure
	}
	f.nextID++
	id := persistence.GenerateID("artifact")
	return &persistence.Artifact{
		ID:          id,
		ProjectID:   projectID,
		Name:        name,
		StoragePath: f.prefix + "/" + projectID + "/inputs/" + id + "/" + name,
	}, nil
}

func (f *fakeInputStore) Retrieve(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}

var errInjectedSnapshotFailure = &snapshotError{}

type snapshotError struct{}

func (e *snapshotError) Error() string { return "injected snapshot failure" }

// fakeTaskRepo records the Create call so the dispatcher's outbound
// payload can be inspected without a real DB. Returns the task back
// via the parser's ActionResult so the dispatcher's downstream
// linking step has something to work with.
type capturingTaskRepo struct {
	*mocks.MockTaskRepository
	last *persistence.Task
}

func (c *capturingTaskRepo) Create(ctx context.Context, t *persistence.Task) error {
	c.last = t
	return c.MockTaskRepository.Create(ctx, t)
}

// TestDispatcher_CreateTask_SnapshotsInputs — the dispatcher's
// happy path: with an artifact store wired, input_files are
// snapshotted into durable storage and the task payload references
// the storage path, not the original host path. Artifact IDs are
// also recorded in the payload (inputArtifactIDs) so consumers can
// look them up later.
func TestDispatcher_CreateTask_SnapshotsInputs(t *testing.T) {
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	artifactRepo := &mocks.MockArtifactRepository{}
	store := &fakeInputStore{prefix: "/var/lib/vornik/artifacts"}

	te := &ToolExecutor{
		taskRepo:          taskRepo,
		artifactRepo:      artifactRepo,
		artifactStore:     store,
		allowedInputRoots: []string{"/tmp", "/var/home/u/uploads"},
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "myproj",
		"type":        "feature",
		"prompt":      "scan this image",
		"input_files": []string{"/tmp/photo.jpg", "/var/home/u/uploads/scan.png"},
	}
	argsJSON, _ := json.Marshal(args)

	res := te.createTask(context.Background(), string(argsJSON), "myproj", []string{"myproj"}, 0)
	if res.Content == "" {
		t.Fatal("createTask returned empty result")
	}

	// Both inputs snapshotted.
	if len(store.calls) != 2 {
		t.Fatalf("expected 2 snapshot calls, got %d: %+v", len(store.calls), store.calls)
	}
	for _, call := range store.calls {
		if call.ProjectID != "myproj" {
			t.Errorf("project mismatch: %+v", call)
		}
	}
	// The dispatcher should have basenamed the source paths.
	gotNames := []string{store.calls[0].Name, store.calls[1].Name}
	wantNames := []string{"photo.jpg", "scan.png"}
	for i, got := range gotNames {
		if got != wantNames[i] {
			t.Errorf("name %d: got %q want %q", i, got, wantNames[i])
		}
	}

	// Task payload now references the storage paths, not /tmp/...
	if taskRepo.last == nil {
		t.Fatal("task not persisted")
	}
	var payload struct {
		Context struct {
			InputFiles       []string `json:"inputFiles"`
			InputArtifactIDs []string `json:"inputArtifactIDs"`
		} `json:"context"`
	}
	if err := json.Unmarshal(taskRepo.last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(payload.Context.InputFiles) != 2 {
		t.Fatalf("expected 2 input files in payload, got %d", len(payload.Context.InputFiles))
	}
	for _, p := range payload.Context.InputFiles {
		if p == "/tmp/photo.jpg" || p == "/var/home/u/uploads/scan.png" {
			t.Errorf("payload still references original host path %q — snapshot didn't rewrite", p)
		}
	}
	if len(payload.Context.InputArtifactIDs) != 2 {
		t.Errorf("expected 2 artifact IDs, got %d", len(payload.Context.InputArtifactIDs))
	}

	// Best-effort linking should have called UpdateTaskID for each.
	if artifactRepo.CallCount.UpdateTaskID != 2 {
		t.Errorf("expected UpdateTaskID called twice, got %d", artifactRepo.CallCount.UpdateTaskID)
	}
}

// fakeAutoExtractor captures AutoExtract calls so the dispatcher
// auto-extract hook can be asserted on without booting the
// extractor stack.
type fakeAutoExtractor struct {
	calls    []AutoExtractRequest
	response *AttachmentExtraction
	err      error
}

func (f *fakeAutoExtractor) AutoExtract(_ context.Context, in AutoExtractRequest) (*AttachmentExtraction, error) {
	f.calls = append(f.calls, in)
	return f.response, f.err
}

// TestDispatcher_CreateTask_AutoExtractFolded — every snapshotted
// input runs through the dispatcher's auto-extract hook. The
// returned summary lands in the task payload's
// context.inputExtractions so the executor's prompt builder can
// surface a "↳ ingested into project memory" trailer to the
// worker. Mirrors the email channel's parallel trigger; covers
// Telegram/webchat/API uploads that bypass the email path.
func TestDispatcher_CreateTask_AutoExtractFolded(t *testing.T) {
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &fakeInputStore{prefix: "/var/lib/vornik/artifacts"}
	extractor := &fakeAutoExtractor{
		response: &AttachmentExtraction{
			ExtractedDocumentID: "extdoc_xyz",
			Title:               "Schema Coaching",
			Author:              "Iain McCormick",
			SectionCount:        30,
			ChunksIngested:      283,
		},
	}

	te := &ToolExecutor{
		taskRepo:                taskRepo,
		artifactStore:           store,
		attachmentAutoExtractor: extractor,
		allowedInputRoots:       []string{"/tmp"},
		logger:                  zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "p",
		"type":        "feature",
		"prompt":      "process this book",
		"input_files": []string{"/tmp/book.epub"},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p", []string{"p"}, 0)

	// AutoExtractor fired once with the snapshotted artifact.
	if len(extractor.calls) != 1 {
		t.Fatalf("AutoExtract calls = %d; want 1", len(extractor.calls))
	}
	call := extractor.calls[0]
	if call.ProjectID != "p" {
		t.Errorf("call.ProjectID = %q", call.ProjectID)
	}
	if call.Name != "book.epub" {
		t.Errorf("call.Name = %q; want book.epub", call.Name)
	}
	if call.StoragePath == "" || call.ArtifactID == "" {
		t.Errorf("call missing storage/artifact id: %+v", call)
	}

	// Payload now carries the extraction summary so the worker's
	// prompt builder can surface the trailer.
	if taskRepo.last == nil {
		t.Fatal("task not persisted")
	}
	var payload struct {
		Context struct {
			InputExtractions []map[string]any `json:"inputExtractions"`
		} `json:"context"`
	}
	if err := json.Unmarshal(taskRepo.last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(payload.Context.InputExtractions) != 1 {
		t.Fatalf("InputExtractions = %d; want 1", len(payload.Context.InputExtractions))
	}
	e := payload.Context.InputExtractions[0]
	if e["extracted_document_id"] != "extdoc_xyz" {
		t.Errorf("extracted_document_id = %v", e["extracted_document_id"])
	}
	if e["title"] != "Schema Coaching" {
		t.Errorf("title = %v", e["title"])
	}
}

// TestDispatcher_CreateTask_AutoExtractFailureIsBestEffort — when
// the extractor errors, the task creation still succeeds; the
// payload just lacks the extraction summary. Mirrors the
// fall-through behaviour for snapshot failures above.
func TestDispatcher_CreateTask_AutoExtractFailureIsBestEffort(t *testing.T) {
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &fakeInputStore{prefix: "/var/lib/vornik/artifacts"}
	extractor := &fakeAutoExtractor{err: &snapshotError{}}

	te := &ToolExecutor{
		taskRepo:                taskRepo,
		artifactStore:           store,
		attachmentAutoExtractor: extractor,
		allowedInputRoots:       []string{"/tmp"},
		logger:                  zerolog.Nop(),
	}
	args := map[string]any{
		"project_id": "p", "type": "feature", "prompt": "x",
		"input_files": []string{"/tmp/x.epub"},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p", []string{"p"}, 0)

	if taskRepo.last == nil {
		t.Fatal("task creation must succeed even when auto-extract fails")
	}
	var payload struct {
		Context struct {
			InputExtractions []map[string]any `json:"inputExtractions"`
		} `json:"context"`
	}
	_ = json.Unmarshal(taskRepo.last.Payload, &payload)
	if len(payload.Context.InputExtractions) != 0 {
		t.Errorf("failed extractor must NOT populate inputExtractions; got %v",
			payload.Context.InputExtractions)
	}
}

// TestDispatcher_CreateTask_AutoExtractNilSummaryIsSilentSkip —
// when the extractor returns (nil, nil) (unsupported MIME), the
// payload is unchanged. No inputExtractions entry, no error.
func TestDispatcher_CreateTask_AutoExtractNilSummaryIsSilentSkip(t *testing.T) {
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &fakeInputStore{prefix: "/var/lib/vornik/artifacts"}
	extractor := &fakeAutoExtractor{} // nil response, nil error

	te := &ToolExecutor{
		taskRepo:                taskRepo,
		artifactStore:           store,
		attachmentAutoExtractor: extractor,
		allowedInputRoots:       []string{"/tmp"},
		logger:                  zerolog.Nop(),
	}
	args := map[string]any{
		"project_id": "p", "type": "feature", "prompt": "x",
		"input_files": []string{"/tmp/random.bin"},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p", []string{"p"}, 0)
	if taskRepo.last == nil {
		t.Fatal("task not persisted")
	}
	var payload struct {
		Context struct {
			InputExtractions []map[string]any `json:"inputExtractions"`
		} `json:"context"`
	}
	_ = json.Unmarshal(taskRepo.last.Payload, &payload)
	if len(payload.Context.InputExtractions) != 0 {
		t.Errorf("nil summary must NOT populate inputExtractions; got %v",
			payload.Context.InputExtractions)
	}
}

// TestDispatcher_CreateTask_SnapshotFailureFallsBack — when an
// individual snapshot fails, the dispatcher must keep the original
// host path for that file rather than dropping it. The user's input
// shouldn't disappear because the artifacts subsystem hiccupped.
func TestDispatcher_CreateTask_SnapshotFailureFallsBack(t *testing.T) {
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	store := &fakeInputStore{prefix: "/var/lib/vornik/artifacts", failOn: "broken.png"}

	te := &ToolExecutor{
		taskRepo:          taskRepo,
		artifactStore:     store,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "p",
		"type":        "feature",
		"prompt":      "x",
		"input_files": []string{"/tmp/ok.jpg", "/tmp/broken.png"},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p", []string{"p"}, 0)

	if taskRepo.last == nil {
		t.Fatal("task not persisted")
	}
	var payload struct {
		Context struct {
			InputFiles       []string `json:"inputFiles"`
			InputArtifactIDs []string `json:"inputArtifactIDs"`
		} `json:"context"`
	}
	_ = json.Unmarshal(taskRepo.last.Payload, &payload)
	if len(payload.Context.InputFiles) != 2 {
		t.Fatalf("expected 2 input files preserved, got %d", len(payload.Context.InputFiles))
	}
	// First file snapshotted (storage path), second fell back to /tmp path.
	if payload.Context.InputFiles[0] == "/tmp/ok.jpg" {
		t.Error("first file should have been snapshotted to storage path")
	}
	if payload.Context.InputFiles[1] != "/tmp/broken.png" {
		t.Errorf("second file should have fallen back to host path, got %q", payload.Context.InputFiles[1])
	}
	// Only one artifact ID recorded (the successful snapshot).
	if len(payload.Context.InputArtifactIDs) != 1 {
		t.Errorf("expected 1 artifact ID (successful snapshot only), got %d", len(payload.Context.InputArtifactIDs))
	}
}

// TestDispatcher_CreateTask_NoStoreFallsThrough — when no artifact
// store is wired (tests, minimal deployments), the dispatcher must
// preserve the original behaviour: input_files in the payload point
// at the host path. This guards backwards compatibility.
func TestDispatcher_CreateTask_NoStoreFallsThrough(t *testing.T) {
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		taskRepo:          taskRepo,
		allowedInputRoots: []string{"/tmp"},
		logger:            zerolog.Nop(),
		// artifactStore intentionally nil
	}

	args := map[string]any{
		"project_id":  "p",
		"type":        "feature",
		"prompt":      "x",
		"input_files": []string{"/tmp/photo.jpg"},
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p", []string{"p"}, 0)

	var payload struct {
		Context struct {
			InputFiles       []string `json:"inputFiles"`
			InputArtifactIDs []string `json:"inputArtifactIDs"`
		} `json:"context"`
	}
	_ = json.Unmarshal(taskRepo.last.Payload, &payload)
	if len(payload.Context.InputFiles) != 1 || payload.Context.InputFiles[0] != "/tmp/photo.jpg" {
		t.Fatalf("expected host path preserved, got %v", payload.Context.InputFiles)
	}
	if len(payload.Context.InputArtifactIDs) != 0 {
		t.Errorf("expected no artifact IDs without store, got %d", len(payload.Context.InputArtifactIDs))
	}
}

// Force chat.ExecuteAction to be importable by referencing it.
var _ = chat.ActionCreateTask
