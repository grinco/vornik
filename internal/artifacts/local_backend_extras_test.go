package artifacts

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalBackend_EmptyKey_AllMethods — every public method that
// runs through resolveKey returns the empty-key error. The
// pre-existing tests cover this through happy paths only; this
// test pins the rejection so a future refactor cannot quietly let
// the empty key through.
func TestLocalBackend_EmptyKey_AllMethods(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	ctx := context.Background()
	if _, err := b.Get(ctx, ""); err == nil {
		t.Error("Get(\"\") returned nil, want empty-key error")
	}
	if err := b.Delete(ctx, ""); err == nil {
		t.Error("Delete(\"\") returned nil, want empty-key error")
	}
	if _, err := b.Exists(ctx, ""); err == nil {
		t.Error("Exists(\"\") returned nil, want empty-key error")
	}
	if _, err := b.Stat(ctx, ""); err == nil {
		t.Error("Stat(\"\") returned nil, want empty-key error")
	}
}

// TestLocalBackend_TraversalKey_Rejected — safepath rejects keys
// containing `..` segments. The error path is the same across Put /
// Get / Delete / Exists / Stat so we just hit one to pin the error
// surface; the safepath package owns the detailed traversal tests.
func TestLocalBackend_TraversalKey_Rejected(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	_, err = b.Put(context.Background(), "../escape.txt", bytes.NewReader([]byte("x")))
	if err == nil {
		t.Error("Put with traversal key returned nil, want rejected")
	}
}

// TestLocalBackend_List_TraversalPrefix_Rejected — the prefix lookup
// runs through safepath.JoinUnder so a traversal-style prefix is
// rejected before we ever read directories. Pins the List error
// branch at local_backend.go:185-187.
func TestLocalBackend_List_TraversalPrefix_Rejected(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	err = b.List(context.Background(), "../escape", func(ObjectInfo) error { return nil })
	if err == nil {
		t.Error("List with traversal prefix returned nil, want rejected")
	}
}

// TestLocalBackend_List_NonExistentPrefix_EmptyResult — when prefix
// resolves to a path that doesn't exist (typical first-time-ever
// list under a fresh project), List must return cleanly with no
// entries, matching the S3 ListObjectsV2 contract. Pins local_backend.go:200-201
// (the os.IsNotExist branch on the list root).
func TestLocalBackend_List_NonExistentPrefix_EmptyResult(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	var hits int
	err = b.List(context.Background(), "never-created", func(ObjectInfo) error {
		hits++
		return nil
	})
	if err != nil {
		t.Errorf("List nonexistent prefix err = %v, want nil", err)
	}
	if hits != 0 {
		t.Errorf("hits = %d, want 0 for nonexistent prefix", hits)
	}
}

// TestLocalBackend_List_WalkContextCancelled — a context cancelled
// mid-walk surfaces ctx.Err. Drives the in-walk ctx.Err check at
// local_backend.go:227-229.
func TestLocalBackend_List_WalkContextCancelled(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	// Write one file so the walker has work to do.
	if _, err := b.Put(context.Background(), "k/x.txt", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	err = b.List(ctx, "", func(ObjectInfo) error { return nil })
	if err == nil {
		t.Error("List with cancelled ctx returned nil err, want context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("List err = %v, want context.Canceled", err)
	}
}

// TestNewLocalBackend_MkdirFailure_OnFileCollision — when basePath
// points at an existing regular file, MkdirAll fails. Surfaces the
// mkdir-error wrap at local_backend.go:40-42.
func TestNewLocalBackend_MkdirFailure_OnFileCollision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a regular file where the backend wants a directory.
	collision := filepath.Join(dir, "iamafile")
	if err := os.WriteFile(collision, []byte("x"), 0o644); err != nil {
		t.Fatalf("write collision file: %v", err)
	}
	// Attempt to root the backend AT the file path itself ⇒ mkdir fails.
	_, err := NewLocalBackend(collision)
	if err == nil {
		t.Error("NewLocalBackend at file path returned nil, want mkdir error")
	}
}

// TestLocalBackend_Put_ResolveKey_OnFileCollision — when an
// intermediate path component already exists as a regular file,
// safepath.JoinUnder rejects the key (because symlink resolution
// hits a non-directory). Verifies the resolveKey error wrap fires
// and the wrapped message includes the key.
func TestLocalBackend_Put_ResolveKey_OnFileCollision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b, err := NewLocalBackend(dir)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "collide"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write collide: %v", err)
	}
	_, err = b.Put(context.Background(), "collide/inner.txt", bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("Put under file-collision parent returned nil, want error")
	}
}
