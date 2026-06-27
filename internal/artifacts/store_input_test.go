package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestStoreInput_HappyPath — file is copied into the input layout,
// DB record is created with class=INPUT, no execution_id or task_id,
// and StoragePath sits under basePath.
func TestStoreInput_HappyPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(src, []byte("imagebytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	repo := &mocks.MockArtifactRepository{}
	store, err := New(WithBasePath(base), WithRepository(repo))
	if err != nil {
		t.Fatal(err)
	}

	art, err := store.StoreInput(context.Background(), "myproj", "photo.jpg", src)
	if err != nil {
		t.Fatalf("StoreInput: %v", err)
	}
	if art.ProjectID != "myproj" {
		t.Errorf("project: got %q want myproj", art.ProjectID)
	}
	if art.ArtifactClass != persistence.ArtifactClassInput {
		t.Errorf("class: got %q want INPUT", art.ArtifactClass)
	}
	if art.TaskID != nil {
		t.Errorf("task_id should be nil at snapshot time, got %v", art.TaskID)
	}
	if art.ExecutionID != nil {
		t.Errorf("execution_id should be nil at snapshot time, got %v", art.ExecutionID)
	}
	// Layout: {base}/{project}/inputs/{artifactID}/{name}
	if !strings.HasPrefix(art.StoragePath, base+"/myproj/inputs/") {
		t.Errorf("storage path off: %q", art.StoragePath)
	}
	if !strings.HasSuffix(art.StoragePath, "/photo.jpg") {
		t.Errorf("storage filename off: %q", art.StoragePath)
	}
	// File actually written.
	data, err := os.ReadFile(art.StoragePath)
	if err != nil {
		t.Fatalf("read storage: %v", err)
	}
	if string(data) != "imagebytes" {
		t.Errorf("contents lost: %q", data)
	}
	// DB record created.
	if repo.CallCount.Create != 1 {
		t.Errorf("expected 1 Create call, got %d", repo.CallCount.Create)
	}
}

// TestStoreInput_DBFailureRemovesFile — if the DB record fails (FK
// constraint, dropped connection), the file we copied must be cleaned
// up so we don't leave orphaned bytes on disk.
func TestStoreInput_DBFailureRemovesFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	repo := &mocks.MockArtifactRepository{
		CreateFunc: func(ctx context.Context, _ *persistence.Artifact) error {
			return os.ErrPermission
		},
	}
	store, err := New(WithBasePath(base), WithRepository(repo))
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.StoreInput(context.Background(), "myproj", "x.jpg", src)
	if err == nil {
		t.Fatal("expected error from DB failure")
	}
	// The file under the artifact ID dir should NOT exist.
	matches, _ := filepath.Glob(filepath.Join(base, "myproj", "inputs", "*", "x.jpg"))
	for _, m := range matches {
		if _, statErr := os.Stat(m); statErr == nil {
			t.Errorf("orphaned file left behind: %s", m)
		}
	}
}

// TestStoreInput_PathTraversalRejected — a path component containing
// "../" must be rejected so an upload can't escape the artifact root.
func TestStoreInput_PathTraversalRejected(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(WithBasePath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ project, name string }{
		{"../etc", "x.jpg"},
		{"valid", "../../etc/passwd"},
		{"valid", "x/y.jpg"}, // CleanFileName disallows separators
	}
	for _, tc := range cases {
		if _, err := store.StoreInput(context.Background(), tc.project, tc.name, src); err == nil {
			t.Errorf("project=%q name=%q: expected error, got nil", tc.project, tc.name)
		}
	}
}

// TestStoreInput_HashAndMimeRecorded — the artifact record should
// carry the SHA256 hash (for future dedup) and inferred MIME type.
func TestStoreInput_HashAndMimeRecorded(t *testing.T) {
	src := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(src, []byte("%PDF-1.7"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(WithBasePath(t.TempDir()), WithRepository(&mocks.MockArtifactRepository{}))
	if err != nil {
		t.Fatal(err)
	}
	art, err := store.StoreInput(context.Background(), "p", "doc.pdf", src)
	if err != nil {
		t.Fatal(err)
	}
	if art.ContentHashSHA256 == nil || *art.ContentHashSHA256 == "" {
		t.Error("content hash not recorded")
	}
	if art.MimeType == nil || *art.MimeType != "application/pdf" {
		t.Errorf("mime: got %v want application/pdf", art.MimeType)
	}
	if art.SizeBytes == nil || *art.SizeBytes != int64(len("%PDF-1.7")) {
		t.Errorf("size: got %v want %d", art.SizeBytes, len("%PDF-1.7"))
	}
}
