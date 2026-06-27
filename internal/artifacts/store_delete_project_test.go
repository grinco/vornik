package artifacts

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestStore_DeleteProjectArtifacts_OnlyWipesNamedProject covers
// the happy path: two projects' blobs share the backend's key
// space; deleting one leaves the other intact. Pins the
// backend-agnostic shape — same code runs against S3 or
// filesystem.
func TestStore_DeleteProjectArtifacts_OnlyWipesNamedProject(t *testing.T) {
	base := t.TempDir()
	store, err := New(WithBasePath(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	// Put 3 blobs under projectA + 1 under projectB. List+Delete
	// is keyed by the leading path segment so projectA's wipe
	// must touch all 3 of its keys and none of B's.
	put := func(key, body string) {
		if _, err := store.backend.Put(ctx, key, strings.NewReader(body)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put("projectA/exec-1/result.json", `{"x":1}`)
	put("projectA/exec-2/report.md", "report 2")
	put("projectA/exec-2/audit.json", `{}`)
	put("projectB/exec-1/result.json", `{"x":2}`)

	n, err := store.DeleteProjectArtifacts(ctx, "projectA")
	if err != nil {
		t.Fatalf("DeleteProjectArtifacts: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}

	// projectA gone — every Get returns ErrNotFound.
	for _, key := range []string{
		"projectA/exec-1/result.json",
		"projectA/exec-2/report.md",
		"projectA/exec-2/audit.json",
	} {
		exists, err := store.backend.Exists(ctx, key)
		if err != nil {
			t.Fatalf("Exists(%s): %v", key, err)
		}
		if exists {
			t.Errorf("projectA key %s should be deleted", key)
		}
	}
	// projectB still intact.
	exists, err := store.backend.Exists(ctx, "projectB/exec-1/result.json")
	if err != nil {
		t.Fatalf("Exists(projectB): %v", err)
	}
	if !exists {
		t.Errorf("projectB blob should still exist after projectA wipe")
	}
}

// TestStore_DeleteProjectArtifacts_EmptyProjectID refuses the
// blank ID — would otherwise List the entire backend and wipe
// everything.
func TestStore_DeleteProjectArtifacts_EmptyProjectID(t *testing.T) {
	store, _ := New(WithBasePath(t.TempDir()))
	if _, err := store.DeleteProjectArtifacts(context.Background(), ""); err == nil {
		t.Errorf("empty projectID should be refused")
	}
}

// TestStore_DeleteProjectArtifacts_NoBlobs returns 0 + nil for a
// project that never wrote anything. Idempotent contract — the
// sweeper retries are safe.
func TestStore_DeleteProjectArtifacts_NoBlobs(t *testing.T) {
	store, _ := New(WithBasePath(t.TempDir()))
	n, err := store.DeleteProjectArtifacts(context.Background(), "missing")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}
}

// Ensure bytes is referenced even if a future trimming pass
// removes the import. (Defensive — most test setups use the
// bytes package for fixture buffers.)
var _ = bytes.NewReader
