package erasure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDocs struct {
	docs    map[string][]Document // artifactID -> docs
	deleted []string
	delErr  error
}

func (f *fakeDocs) ListBySourceArtifact(_ context.Context, artifactID string) ([]Document, error) {
	return f.docs[artifactID], nil
}

func (f *fakeDocs) DeleteExtractedDocument(_ context.Context, id string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeChunks struct {
	byDoc      map[string]int // extractedDocID -> chunk count
	byArtifact map[string]int
	deletedDoc []string
	deletedArt []string
	delErr     error
}

func (f *fakeChunks) CountByExtractedDocument(_ context.Context, id string) (int, error) {
	return f.byDoc[id], nil
}

func (f *fakeChunks) CountByArtifact(_ context.Context, id string) (int, error) {
	return f.byArtifact[id], nil
}

func (f *fakeChunks) DeleteByExtractedDocument(_ context.Context, id string) (int, error) {
	if f.delErr != nil {
		return 0, f.delErr
	}
	f.deletedDoc = append(f.deletedDoc, id)
	return f.byDoc[id], nil
}

func (f *fakeChunks) DeleteByArtifact(_ context.Context, id string) (int, error) {
	if f.delErr != nil {
		return 0, f.delErr
	}
	f.deletedArt = append(f.deletedArt, id)
	return f.byArtifact[id], nil
}

// newService wires a service over a temp artifact root with one extraction.
func newService(t *testing.T) (*Service, *fakeDocs, *fakeChunks, string) {
	t.Helper()
	root := t.TempDir()
	storage := filepath.Join(root, "assistant", "extracted", "extdoc_1")
	if err := os.MkdirAll(filepath.Join(storage, "sections"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storage, "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, "sections", "001-image.md"), []byte("OCR text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, "files", "frame-001.jpg"), []byte("JPEG"), 0o600); err != nil {
		t.Fatal(err)
	}
	docs := &fakeDocs{docs: map[string][]Document{
		"artifact_1": {{ID: "extdoc_1", StoragePath: storage, SectionCount: 1}},
	}}
	chunks := &fakeChunks{
		byDoc:      map[string]int{"extdoc_1": 12},
		byArtifact: map[string]int{"artifact_1": 3},
	}
	return &Service{Docs: docs, Chunks: chunks, ArtifactRoot: root}, docs, chunks, storage
}

// Plan is read-only: an operator must be able to see the blast radius before
// anything is destroyed. Erasure is irreversible, so "run it and find out" is
// not an acceptable interface.
func TestPlan_IsReadOnlyAndComplete(t *testing.T) {
	s, docs, chunks, storage := newService(t)

	plan, err := s.Plan(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Documents) != 1 || plan.Documents[0].ID != "extdoc_1" {
		t.Fatalf("plan documents wrong: %+v", plan.Documents)
	}
	if plan.Documents[0].ChunkCount != 12 {
		t.Errorf("chunk count = %d, want 12", plan.Documents[0].ChunkCount)
	}
	if plan.DirectChunkCount != 3 {
		t.Errorf("direct chunk count = %d, want 3", plan.DirectChunkCount)
	}
	if plan.TotalChunks() != 15 {
		t.Errorf("total chunks = %d, want 15", plan.TotalChunks())
	}
	// Nothing may have been touched.
	if len(docs.deleted) != 0 || len(chunks.deletedDoc) != 0 || len(chunks.deletedArt) != 0 {
		t.Error("Plan must not delete anything")
	}
	if _, err := os.Stat(storage); err != nil {
		t.Error("Plan must not remove the storage directory")
	}
}

func TestErase_RemovesChunksDocsAndFiles(t *testing.T) {
	s, docs, chunks, storage := newService(t)

	res, err := s.Erase(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Error("the extraction's storage directory must be gone (it holds OCR text and keyframes)")
	}
	if len(docs.deleted) != 1 || docs.deleted[0] != "extdoc_1" {
		t.Errorf("extracted_documents row not deleted: %v", docs.deleted)
	}
	if len(chunks.deletedDoc) != 1 {
		t.Errorf("derived chunks not deleted: %v", chunks.deletedDoc)
	}
	if len(chunks.deletedArt) != 1 {
		t.Errorf("chunks linked directly to the artifact not deleted: %v", chunks.deletedArt)
	}
	if res.ChunksDeleted != 15 {
		t.Errorf("ChunksDeleted = %d, want 15", res.ChunksDeleted)
	}
	if res.DocumentsDeleted != 1 || res.DirectoriesRemoved != 1 {
		t.Errorf("unexpected result: %+v", res)
	}
}

// THE guard. StoragePath comes out of the database and is handed to a
// recursive delete, so a wrong or malicious value is a "wipe the wrong
// directory" bug. Anything not contained in ArtifactRoot must be refused
// before RemoveAll is called, and the refusal must abort the whole erasure
// rather than continuing — a partial erasure reported as success is worse
// than a failure.
func TestErase_RefusesPathOutsideArtifactRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "important")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	docs := &fakeDocs{docs: map[string][]Document{
		"artifact_1": {{ID: "extdoc_1", StoragePath: victim}},
	}}
	chunks := &fakeChunks{byDoc: map[string]int{}, byArtifact: map[string]int{}}
	s := &Service{Docs: docs, Chunks: chunks, ArtifactRoot: root}

	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a storage path outside the artifact root must be refused")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("the out-of-root directory must NOT have been removed")
	}
	if len(docs.deleted) != 0 || len(chunks.deletedDoc) != 0 {
		t.Error("nothing may be deleted once the plan is refused")
	}
}

func TestErase_RefusesTraversalPath(t *testing.T) {
	root := t.TempDir()
	docs := &fakeDocs{docs: map[string][]Document{
		"artifact_1": {{ID: "extdoc_1", StoragePath: filepath.Join(root, "..", "escaped")}},
	}}
	s := &Service{Docs: docs, Chunks: &fakeChunks{}, ArtifactRoot: root}
	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a traversal path must be refused")
	}
}

// The artifact root itself must never be the target — that would erase every
// project's extractions in one call.
func TestErase_RefusesArtifactRootItself(t *testing.T) {
	root := t.TempDir()
	docs := &fakeDocs{docs: map[string][]Document{
		"artifact_1": {{ID: "extdoc_1", StoragePath: root}},
	}}
	s := &Service{Docs: docs, Chunks: &fakeChunks{}, ArtifactRoot: root}
	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("the artifact root itself must never be a deletion target")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("the artifact root must still exist")
	}
}

// An empty StoragePath must not degrade into deleting the root or the CWD.
func TestErase_RefusesEmptyStoragePath(t *testing.T) {
	root := t.TempDir()
	docs := &fakeDocs{docs: map[string][]Document{
		"artifact_1": {{ID: "extdoc_1", StoragePath: ""}},
	}}
	s := &Service{Docs: docs, Chunks: &fakeChunks{}, ArtifactRoot: root}
	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("an empty storage path must be refused")
	}
}

// Filesystem before database, deliberately. The bytes on disk ARE the personal
// data; the rows are the index that lets anyone find them. Deleting the index
// first and then failing on the files would leave the data present and
// unfindable — the worst outcome for an erasure request.
func TestErase_FilesBeforeRowsSoAFailureNeverStrandsData(t *testing.T) {
	s, docs, chunks, storage := newService(t)
	s.removeAll = func(string) error { return errors.New("disk busy") }

	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a filesystem failure must fail the erasure")
	}
	if len(docs.deleted) != 0 {
		t.Error("no row may be deleted after the filesystem step failed")
	}
	if len(chunks.deletedDoc) != 0 || len(chunks.deletedArt) != 0 {
		t.Error("no chunk may be deleted after the filesystem step failed")
	}
	if _, err := os.Stat(storage); err != nil {
		t.Error("the directory should still be present (our fake failed, it did not delete)")
	}
}

// Erasing twice must succeed: an already-erased artifact is the desired state,
// and an operator retrying after a partial failure must not be blocked.
func TestErase_IsIdempotent(t *testing.T) {
	s, _, _, _ := newService(t)
	if _, err := s.Erase(context.Background(), "artifact_1"); err != nil {
		t.Fatalf("first erase: %v", err)
	}
	res, err := s.Erase(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("second erase must not fail: %v", err)
	}
	// The directory is already gone, so nothing was removed the second time.
	if res.DirectoriesRemoved != 0 {
		t.Errorf("second erase removed %d directories, want 0", res.DirectoriesRemoved)
	}
}

func TestErase_NoExtractionsStillClearsDirectChunks(t *testing.T) {
	root := t.TempDir()
	docs := &fakeDocs{docs: map[string][]Document{}}
	chunks := &fakeChunks{byArtifact: map[string]int{"artifact_1": 5}, byDoc: map[string]int{}}
	s := &Service{Docs: docs, Chunks: chunks, ArtifactRoot: root}

	res, err := s.Erase(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.ChunksDeleted != 5 || len(chunks.deletedArt) != 1 {
		t.Errorf("chunks linked directly to the artifact must still be erased: %+v", res)
	}
}

// A chunk-delete failure must surface. Erasure that half-succeeds and reports
// success is the failure mode this whole exercise exists to remove.
func TestErase_ChunkFailureSurfaces(t *testing.T) {
	s, _, chunks, _ := newService(t)
	chunks.delErr = errors.New("db down")
	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a chunk-deletion failure must be reported")
	}
}

func TestErase_DocFailureSurfaces(t *testing.T) {
	s, docs, _, _ := newService(t)
	docs.delErr = errors.New("db down")
	if _, err := s.Erase(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a document-deletion failure must be reported")
	}
}

func TestService_RequiresArtifactRoot(t *testing.T) {
	s := &Service{Docs: &fakeDocs{}, Chunks: &fakeChunks{}}
	if _, err := s.Erase(context.Background(), "a"); err == nil {
		t.Fatal("an unset ArtifactRoot must be refused rather than defaulting to /")
	}
	if _, err := s.Plan(context.Background(), "a"); err == nil {
		t.Fatal("Plan must also refuse an unset ArtifactRoot")
	}
}

func TestErase_RequiresArtifactID(t *testing.T) {
	s, _, _, _ := newService(t)
	if _, err := s.Erase(context.Background(), ""); err == nil {
		t.Fatal("an empty artifact id must be refused")
	}
}

// The plan's human summary is what an operator reads before confirming, so it
// must name the counts rather than merely listing ids.
func TestPlan_SummaryNamesTheBlastRadius(t *testing.T) {
	s, _, _, _ := newService(t)
	plan, err := s.Plan(context.Background(), "artifact_1")
	if err != nil {
		t.Fatal(err)
	}
	sum := plan.Summary()
	for _, want := range []string{"artifact_1", "1 extracted document", "15 memory chunk"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q:\n%s", want, sum)
		}
	}
}

// ArtifactRoot="/" must be refused outright.
//
// Raised in review (2026-07-29) as "any absolute path passes, defeating the
// boundary". That reading was wrong — with root "/" the containment test
// compares against "//" and rejects EVERY path, so the behaviour already
// failed closed. This test pins both halves: the refusal is now explicit AND
// nothing is deleted, so if someone later replaces the prefix comparison with
// a normalising helper (which would turn "//" into "/" and let everything
// through) the explicit guard still holds the line.
func TestService_RefusesFilesystemRoot(t *testing.T) {
	dir := t.TempDir()
	storage := filepath.Join(dir, "extdoc_1")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	docs := &fakeDocs{docs: map[string][]Document{
		"artifact_1": {{ID: "extdoc_1", StoragePath: storage}},
	}}
	for _, root := range []string{"/", "//", "/.", " / "} {
		s := &Service{Docs: docs, Chunks: &fakeChunks{}, ArtifactRoot: root}
		_, err := s.Erase(context.Background(), "artifact_1")
		if err == nil {
			t.Fatalf("ArtifactRoot=%q must be refused", root)
		}
		if !strings.Contains(err.Error(), "filesystem root") {
			t.Errorf("ArtifactRoot=%q: error should name the cause, got: %v", root, err)
		}
		if _, statErr := os.Stat(storage); statErr != nil {
			t.Fatalf("ArtifactRoot=%q DELETED the directory — fail-open", root)
		}
	}
	// Plan must refuse it too, so a preview cannot imply a root is usable.
	s := &Service{Docs: docs, Chunks: &fakeChunks{}, ArtifactRoot: "/"}
	if _, err := s.Plan(context.Background(), "artifact_1"); err == nil {
		t.Error("Plan must also refuse the filesystem root")
	}
}
