package artifacts

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// rtNewStore builds a Store backed by a real LocalBackend under
// t.TempDir and the in-package MockArtifactRepo. Both halves are
// real-ish: blobs land on disk, metadata lives in the mock map. This
// exercises the Store↔LocalBackend↔filesystem path end-to-end rather
// than the fake-backend delegation path in store_backend_test.go.
func rtNewStore(t *testing.T) (*Store, *MockArtifactRepo, string) {
	t.Helper()
	base := t.TempDir()
	repo := NewMockArtifactRepo()
	store, err := New(WithBasePath(base), WithRepository(repo))
	require.NoError(t, err)
	return store, repo, base
}

func rtWriteSource(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, body, 0o644))
	return p
}

// TestStore_RetrieveRoundTrip — Store then Retrieve returns the exact
// bytes written, and the recorded SHA-256 verifies on read. This is
// the core content-fidelity + content-addressing contract against a
// real on-disk backend.
func TestStore_RetrieveRoundTrip(t *testing.T) {
	store, _, _ := rtNewStore(t)
	want := []byte("output line one\noutput line two\n")
	src := rtWriteSource(t, "out.txt", want)

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "out.txt", src)
	require.NoError(t, err)
	require.NotNil(t, art.ContentHashSHA256)
	require.NotNil(t, art.SizeBytes)
	assert.Equal(t, int64(len(want)), *art.SizeBytes)

	got, err := store.Retrieve(context.Background(), art.ID)
	require.NoError(t, err)
	assert.Equal(t, want, got, "retrieved bytes must match stored bytes exactly")
}

// TestStore_RetrieveHashMismatch — if the blob on disk is tampered
// with after storage, Retrieve must surface ErrHashMismatch rather
// than silently serving corrupted bytes. Drives the content-address
// verification branch in Retrieve.
func TestStore_RetrieveHashMismatch(t *testing.T) {
	store, _, _ := rtNewStore(t)
	src := rtWriteSource(t, "data.txt", []byte("trustworthy"))

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "data.txt", src)
	require.NoError(t, err)

	// Tamper with the on-disk blob behind the store's back.
	require.NoError(t, os.WriteFile(art.StoragePath, []byte("TAMPERED"), 0o644))

	_, err = store.Retrieve(context.Background(), art.ID)
	assert.ErrorIs(t, err, ErrHashMismatch)
}

// TestStore_RetrieveRepoGetError — a repository Get failure propagates
// out of Retrieve wrapped, not as a nil-deref panic.
func TestStore_RetrieveRepoGetError(t *testing.T) {
	store, repo, _ := rtNewStore(t)
	repo.getErr = errors.New("db down")

	_, err := store.Retrieve(context.Background(), "whatever")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get artifact")
}

// TestStore_RetrieveMissingArtifact — Get returning (nil,nil) (the
// mock/legacy-adapter miss contract) is treated as not-found, not a
// panic on StoragePath deref.
func TestStore_RetrieveMissingArtifact(t *testing.T) {
	store, _, _ := rtNewStore(t)
	_, err := store.Retrieve(context.Background(), "artifact_doesnotexist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestStore_OpenStreamRoundTrip — Open returns a streaming reader
// whose bytes match what was stored, and the caller-closeable
// ReadCloser closes without error.
func TestStore_OpenStreamRoundTrip(t *testing.T) {
	store, _, _ := rtNewStore(t)
	want := []byte("streamed body for http file server")
	src := rtWriteSource(t, "stream.bin", want)

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "stream.bin", src)
	require.NoError(t, err)

	rc, err := store.Open(context.Background(), art.ID)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestStore_OpenMissingArtifact — Open of an unknown ID is a clean
// not-found error (repo returns nil artifact), not a panic.
func TestStore_OpenMissingArtifact(t *testing.T) {
	store, _, _ := rtNewStore(t)
	_, err := store.Open(context.Background(), "artifact_nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestStore_OpenRejectsEscapingStoragePath — like Retrieve, Open
// re-validates the stored path at read time and refuses a row that
// textually sits under basePath but resolves outside it.
func TestStore_OpenRejectsEscapingStoragePath(t *testing.T) {
	store, repo, base := rtNewStore(t)
	escaping := base + string(filepath.Separator) + "sub" +
		string(filepath.Separator) + ".." + string(filepath.Separator) +
		".." + string(filepath.Separator) + "outside-secret.txt"
	id := "artifact_open_escape"
	mustStore(t, repo, id, "p1", escaping)

	_, err := store.Open(context.Background(), id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")
}

// TestStore_DeleteRoundTrip — Delete removes both the on-disk blob and
// the DB record; a subsequent Retrieve fails. Exercises the full
// Store.Delete happy path against a real backend.
func TestStore_DeleteRoundTrip(t *testing.T) {
	store, repo, _ := rtNewStore(t)
	src := rtWriteSource(t, "gone.txt", []byte("delete me"))

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "gone.txt", src)
	require.NoError(t, err)
	// Blob present on disk before delete.
	_, statErr := os.Stat(art.StoragePath)
	require.NoError(t, statErr)

	require.NoError(t, store.Delete(context.Background(), art.ID))

	// Blob gone from disk.
	_, statErr = os.Stat(art.StoragePath)
	assert.True(t, os.IsNotExist(statErr), "blob should be removed from disk")
	// DB record gone.
	got, _ := repo.Get(context.Background(), art.ID)
	assert.Nil(t, got, "DB record should be removed")
}

// TestStore_DeleteRepoDeleteError — when the blob delete succeeds but
// the DB record delete fails, the error surfaces (so callers don't
// believe a half-deletion succeeded). Drives the repo.Delete error
// branch.
func TestStore_DeleteRepoDeleteError(t *testing.T) {
	store, repo, _ := rtNewStore(t)
	src := rtWriteSource(t, "x.txt", []byte("payload"))
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "x.txt", src)
	require.NoError(t, err)

	repo.deleteErr = errors.New("constraint violation")
	err = store.Delete(context.Background(), art.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete artifact record")
}

// TestStore_DeleteMissingArtifact — deleting an unknown ID is a clean
// not-found error (repo returns nil artifact), not a panic.
func TestStore_DeleteMissingArtifact(t *testing.T) {
	store, _, _ := rtNewStore(t)
	err := store.Delete(context.Background(), "artifact_unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestStore_ListByExecution — List returns the project's artifacts via
// the repository filter. The in-package mock filters on ProjectID, so
// a stored artifact surfaces for its project and not for another.
func TestStore_ListByExecution(t *testing.T) {
	store, _, _ := rtNewStore(t)
	src := rtWriteSource(t, "rep.md", []byte("# report"))
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "rep.md", src)
	require.NoError(t, err)

	got, err := store.List(context.Background(), "p1", "e1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, art.ID, got[0].ID)

	// A different project yields nothing.
	other, err := store.List(context.Background(), "p2", "e1")
	require.NoError(t, err)
	assert.Empty(t, other)
}

// TestStore_ListRepoError — a repository List failure propagates out
// of Store.List unchanged.
func TestStore_ListRepoError(t *testing.T) {
	store, repo, _ := rtNewStore(t)
	sentinel := errors.New("list failed")
	repo.listErr = sentinel

	_, err := store.List(context.Background(), "p1", "e1")
	assert.ErrorIs(t, err, sentinel)
}

// TestStore_GetPathReturnsStoragePath — GetPath returns the recorded
// absolute StoragePath under basePath for a stored artifact.
func TestStore_GetPathReturnsStoragePath(t *testing.T) {
	store, _, base := rtNewStore(t)
	src := rtWriteSource(t, "p.txt", []byte("path test"))
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "p.txt", src)
	require.NoError(t, err)

	path, err := store.GetPath(art.ID)
	require.NoError(t, err)
	assert.Equal(t, art.StoragePath, path)
	rel, relErr := filepath.Rel(base, path)
	require.NoError(t, relErr)
	assert.NotContains(t, rel, "..", "stored path must stay under basePath")
}

// TestStore_GetPathRepoError — a repository Get failure propagates out
// of GetPath wrapped.
func TestStore_GetPathRepoError(t *testing.T) {
	store, repo, _ := rtNewStore(t)
	repo.getErr = errors.New("db unreachable")

	_, err := store.GetPath("anything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get artifact")
}

// TestStore_RetrieveRejectsEscapingStoragePath — a corrupted/edited DB
// row whose StoragePath shares the basePath prefix but resolves
// outside the artifact root must be refused by the read-time
// containment guard (assertUnderBase), not served.
func TestStore_RetrieveRejectsEscapingStoragePath(t *testing.T) {
	store, repo, base := rtNewStore(t)

	// Craft a row whose path textually starts with basePath (so the
	// HasPrefix guard runs) but resolves outside the root via "..".
	// A plausible corrupted/hand-edited-row scenario. Build it by
	// string concatenation so the basePath prefix survives un-cleaned.
	escaping := base + string(filepath.Separator) + "sub" +
		string(filepath.Separator) + ".." + string(filepath.Separator) +
		".." + string(filepath.Separator) + "outside-secret.txt"
	id := "artifact_escape"
	mustStore(t, repo, id, "p1", escaping)

	_, err := store.Retrieve(context.Background(), id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")
}

// TestStore_RetrieveRejectsCleanedEscapingPath covers the case the old
// strings.HasPrefix gate MISSED: a StoragePath that resolves outside the root
// but does NOT textually prefix basePath (a sibling dir). Pre-fix this slipped
// the containment check entirely; the local-backend gate now catches it.
func TestStore_RetrieveRejectsCleanedEscapingPath(t *testing.T) {
	store, repo, base := rtNewStore(t)
	// Sibling of base — outside the root, and does not start with base.
	escaping := filepath.Dir(base) + string(filepath.Separator) + "outside-secret.txt"
	id := "artifact_cleaned_escape"
	mustStore(t, repo, id, "p1", escaping)

	_, err := store.Retrieve(context.Background(), id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")
}

// TestAssertUnderBase exercises the containment primitive directly:
// the in-root case passes; escape, equal-to-base, and sibling-prefix
// cases are rejected; and an empty base disables the check.
func TestAssertUnderBase(t *testing.T) {
	base := t.TempDir()

	// Inside base — allowed.
	require.NoError(t, assertUnderBase(base, filepath.Join(base, "proj", "f.txt")))

	// Escape via parent — rejected.
	assert.Error(t, assertUnderBase(base, filepath.Join(base, "..", "evil.txt")))

	// Equal to base (rel == ".") — rejected (not a file under root).
	assert.Error(t, assertUnderBase(base, base))

	// Sibling directory sharing the textual prefix — rejected.
	assert.Error(t, assertUnderBase(base, base+"-sibling/f.txt"))

	// Empty base disables the check.
	require.NoError(t, assertUnderBase("", "/anywhere/at/all"))
}

// TestStore_DeleteProjectArtifacts_NilBackendGuard — a nil store or a
// store with no backend wired returns the guard error rather than
// nil-deref panicking. Pins the defensive guard at the top of
// DeleteProjectArtifacts.
func TestStore_DeleteProjectArtifacts_NilBackendGuard(t *testing.T) {
	var nilStore *Store
	_, err := nilStore.DeleteProjectArtifacts(context.Background(), "p1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backend not wired")

	noBackend := &Store{} // constructed without New ⇒ backend == nil
	_, err = noBackend.DeleteProjectArtifacts(context.Background(), "p1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backend not wired")
}

// mustStore inserts a pre-built artifact row into the mock repo with a
// caller-chosen StoragePath, used to simulate corrupted/hand-edited
// rows for the read-time containment guards.
func mustStore(t *testing.T, repo *MockArtifactRepo, id, projectID, storagePath string) {
	t.Helper()
	require.NoError(t, repo.Create(context.Background(), &persistence.Artifact{
		ID:            id,
		ProjectID:     projectID,
		Name:          filepath.Base(storagePath),
		ArtifactClass: persistence.ArtifactClassOutput,
		StoragePath:   storagePath,
	}))
}
