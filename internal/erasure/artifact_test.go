package erasure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// EraseIncludingArtifact — the complete Art 17 erasure of an uploaded artifact
// (GDPR increment 5, slice 5b).
//
// Erase() removes everything DERIVED from an artifact and deliberately stops
// there; `vornikctl erase artifact` prints "the artifact row itself is untouched"
// to say so. For a retention-driven cleanup that is right. For an Art 17 erasure
// it is not: the original upload is the most direct copy of the subject's data,
// and its filename ("mri-scan-jane-doe.pdf") is often personal data by itself. An
// erasure that removed the OCR text but left the scan on disk would report
// success while the file it was asked to destroy is still there.

type fakeArtifactRows struct {
	paths   map[string]string
	deleted []string
	getErr  error
	delErr  error
}

func (f *fakeArtifactRows) ArtifactStoragePath(_ context.Context, id string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.paths[id], nil
}

func (f *fakeArtifactRows) DeleteArtifactRow(_ context.Context, id string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

// newCompleteService extends the derived-cascade fixture with the artifact's own
// stored file.
func newCompleteService(t *testing.T) (*Service, *fakeArtifactRows, string) {
	t.Helper()
	svc, _, _, _ := newService(t)
	root := svc.ArtifactRoot
	blob := filepath.Join(root, "assistant", "uploads", "mri-scan.pdf")
	if err := os.MkdirAll(filepath.Dir(blob), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("%PDF the actual upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := &fakeArtifactRows{paths: map[string]string{"artifact_1": blob}}
	svc.Artifacts = rows
	return svc, rows, blob
}

// The finding this method exists for: the upload itself must go.
func TestEraseIncludingArtifact_RemovesTheUploadedFileAndTheRow(t *testing.T) {
	svc, rows, blob := newCompleteService(t)

	res, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("EraseIncludingArtifact: %v", err)
	}
	if _, statErr := os.Stat(blob); !os.IsNotExist(statErr) {
		t.Error("the uploaded file itself must be deleted — leaving it is the failure this method exists to prevent")
	}
	if len(rows.deleted) != 1 || rows.deleted[0] != "artifact_1" {
		t.Errorf("the artifact row must be deleted, got %v", rows.deleted)
	}
	if !res.ArtifactRowDeleted {
		t.Error("the result must record that the row went")
	}
	if res.BlobsRemoved != 1 {
		t.Errorf("BlobsRemoved = %d, want 1", res.BlobsRemoved)
	}
	// And the derived cascade still ran.
	if res.ChunksDeleted != 15 { // 12 by extraction + 3 direct
		t.Errorf("ChunksDeleted = %d, want the derived cascade's 15", res.ChunksDeleted)
	}
	if res.DocumentsDeleted != 1 {
		t.Errorf("DocumentsDeleted = %d, want 1", res.DocumentsDeleted)
	}
}

// Containment applies to the artifact blob exactly as it does to extraction
// directories: the path comes out of the database and is handed to a delete.
func TestEraseIncludingArtifact_RefusesABlobOutsideTheRoot(t *testing.T) {
	svc, rows, _ := newCompleteService(t)
	outside := filepath.Join(t.TempDir(), "elsewhere", "passwd")
	if err := os.MkdirAll(filepath.Dir(outside), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows.paths["artifact_1"] = outside

	if _, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a blob path outside the containment root must be refused")
	}
	if _, statErr := os.Stat(outside); os.IsNotExist(statErr) {
		t.Fatal("the out-of-root file must NOT have been deleted")
	}
	if len(rows.deleted) != 0 {
		t.Error("the row must not be deleted when the blob could not be safely removed")
	}
}

// An artifact whose stored file is already gone is not an error — an operator
// must be able to retry a partially-failed erasure, which is the property Erase()
// already has.
func TestEraseIncludingArtifact_IsIdempotentOverAMissingBlob(t *testing.T) {
	svc, rows, blob := newCompleteService(t)
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	res, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("a missing blob must not fail the erasure: %v", err)
	}
	if res.BlobsRemoved != 0 {
		t.Errorf("BlobsRemoved = %d, want 0 for an already-absent file", res.BlobsRemoved)
	}
	if len(rows.deleted) != 1 {
		t.Error("the row should still be deleted")
	}
}

// A row with no recorded storage path still gets its row deleted; there is simply
// no blob to remove. Refusing here would strand the row forever.
func TestEraseIncludingArtifact_HandlesAnEmptyStoragePath(t *testing.T) {
	svc, rows, _ := newCompleteService(t)
	rows.paths["artifact_1"] = ""

	res, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1")
	if err != nil {
		t.Fatalf("EraseIncludingArtifact: %v", err)
	}
	if res.BlobsRemoved != 0 {
		t.Errorf("BlobsRemoved = %d, want 0", res.BlobsRemoved)
	}
	if len(rows.deleted) != 1 {
		t.Error("the row must still be deleted")
	}
}

// Bytes before rows, the same ordering discipline Erase() uses: if the row went
// first and the file delete then failed, the file would be orphaned with no
// database pointer left to find it by.
func TestEraseIncludingArtifact_DoesNotDeleteTheRowWhenTheBlobDeleteFails(t *testing.T) {
	svc, rows, blob := newCompleteService(t)
	svc.removeAll = func(string) error { return errors.New("permission denied") }

	if _, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a failed blob delete must fail the erasure")
	}
	if len(rows.deleted) != 0 {
		t.Error("the row must survive so the orphaned file remains findable")
	}
	if _, statErr := os.Stat(blob); os.IsNotExist(statErr) {
		t.Error("fixture sanity: the blob should still be present")
	}
}

func TestEraseIncludingArtifact_RefusesWithoutTheArtifactStore(t *testing.T) {
	svc, _, _, _ := newService(t)
	svc.Artifacts = nil
	if _, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1"); err == nil {
		t.Fatal("without an artifact store this method cannot delete the row and must refuse")
	}
}

// A lookup failure must not proceed to delete anything: without the storage path
// the blob cannot be removed, and deleting the row would orphan it.
func TestEraseIncludingArtifact_LookupFailureDeletesNothing(t *testing.T) {
	svc, rows, blob := newCompleteService(t)
	rows.getErr = errors.New("db down")

	if _, err := svc.EraseIncludingArtifact(context.Background(), "artifact_1"); err == nil {
		t.Fatal("a storage-path lookup failure must fail the erasure")
	}
	if len(rows.deleted) != 0 {
		t.Error("nothing may be deleted when the blob location is unknown")
	}
	if _, statErr := os.Stat(blob); os.IsNotExist(statErr) {
		t.Error("the blob must be untouched")
	}
}

// Erase() must keep its existing contract — retention pruning relies on it NOT
// touching the artifact row.
func TestErase_StillLeavesTheArtifactRowAlone(t *testing.T) {
	svc, rows, blob := newCompleteService(t)

	if _, err := svc.Erase(context.Background(), "artifact_1"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if len(rows.deleted) != 0 {
		t.Error("Erase must not delete the artifact row — retention pruning depends on that")
	}
	if _, statErr := os.Stat(blob); os.IsNotExist(statErr) {
		t.Error("Erase must not delete the artifact's own file either")
	}
}
