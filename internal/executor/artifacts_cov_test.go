package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArtifactsCov_HashFile_OpenError covers hashFile's os.Open error branch.
func TestArtifactsCov_HashFile_OpenError(t *testing.T) {
	_, err := hashFile(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.Error(t, err)
}

// TestArtifactsCov_HashFile_HappyPath pins the digest of a known input so the
// streaming-copy path is exercised end to end.
func TestArtifactsCov_HashFile_HappyPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("abc"), 0o600))
	h, err := hashFile(p)
	require.NoError(t, err)
	// SHA-256("abc")
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", h)
}

// TestArtifactsCov_SnapshotArtifactDir_SizeCapAndSubdirs covers:
//   - the >4 MiB size-cap branch (records "size:<n>" instead of hashing),
//   - the IsDir skip branch,
//   - normal hash entries.
func TestArtifactsCov_SnapshotArtifactDir_SizeCapAndSubdirs(t *testing.T) {
	dir := t.TempDir()

	// A subdirectory — must be skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))

	// A normal small file — hashed.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.md"), []byte("hi"), 0o600))

	// A file just over the 4 MiB cap — recorded by size, not hashed.
	big := make([]byte, 4*1024*1024+1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o600))

	e := &Executor{logger: zerolog.Nop()}
	snap := e.SnapshotArtifactDir(dir)

	assert.NotContains(t, snap, "sub", "directories must be skipped")
	assert.Contains(t, snap, "small.md")
	if got := snap["big.bin"]; assert.Contains(t, snap, "big.bin") {
		assert.Equal(t, "size:4194305", got, "oversized file recorded by size, not hash")
	}
}

// TestArtifactsCov_SnapshotArtifactDir_GuardBranches covers the empty-dir and
// unreadable-dir early returns.
func TestArtifactsCov_SnapshotArtifactDir_GuardBranches(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}

	// Empty dir argument → empty snapshot.
	assert.Empty(t, e.SnapshotArtifactDir(""))

	// Non-existent dir → os.ReadDir error → empty snapshot.
	assert.Empty(t, e.SnapshotArtifactDir(filepath.Join(t.TempDir(), "nope")))

	// nil-Executor receiver path: hashFile failure logs only when e != nil;
	// here we just confirm a valid dir still snapshots with a nil logger-less
	// executor via the e!=nil guard (constructed executor has a logger).
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.md"), []byte("y"), 0o600))
	assert.Contains(t, e.SnapshotArtifactDir(dir), "x.md")
}
