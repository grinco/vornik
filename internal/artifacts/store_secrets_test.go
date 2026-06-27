package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

// newTestStoreWithSecrets builds an artifact Store with a real
// MultiDetector and the given action override. Backed by t.TempDir
// so each test gets isolated storage.
func newTestStoreWithSecrets(t *testing.T, actions map[string]secrets.Action) *Store {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	repo := NewMockArtifactRepo()
	store, err := New(
		WithBasePath(t.TempDir()),
		WithRepository(repo),
		WithLogger(zerolog.Nop()),
		WithSecrets(det, actions),
	)
	require.NoError(t, err)
	return store
}

func writeTempSource(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0644))
	return p
}

// TestStore_RedactsTextArtifactByDefault — the headline contract:
// a markdown artifact carrying an OpenAI key gets redacted before
// the bytes hit storage. The recorded hash is over the redacted
// content so Retrieve's hash check stays consistent.
func TestStore_RedactsTextArtifactByDefault(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)

	src := writeTempSource(t, "result.md", "# notes\nkey=sk-proj1234567890abcdefghijklmnopqrstuv\nsafe text here")
	art, err := store.Store(context.Background(), "p1", "exec1", "task1", "result.md", src)
	require.NoError(t, err)
	require.NotNil(t, art)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "sk-proj1234567890",
		"redact-mode default must scrub the OpenAI key from stored artifact bytes")
	assert.Contains(t, string(stored), "[REDACTED:openai_key]")
	assert.Contains(t, string(stored), "safe text here")

	// Retrieve must succeed — the recorded hash matches the
	// redacted bytes, not the source bytes.
	body, err := store.Retrieve(context.Background(), art.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "sk-proj1234567890")
}

// TestStore_DetectModeLeavesContentIntact — operator override to
// detect-only logs the finding but writes the source bytes
// unchanged. Useful for staging the detector against a noisy
// project corpus before promoting.
func TestStore_DetectModeLeavesContentIntact(t *testing.T) {
	store := newTestStoreWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointArtifacts: secrets.ActionDetect,
	})
	src := writeTempSource(t, "result.md", "key=sk-proj1234567890abcdefghijklmnopqrstuv")
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Equal(t, "key=sk-proj1234567890abcdefghijklmnopqrstuv", string(stored),
		"detect-mode must write the source bytes verbatim")
}

// TestStore_BlockDegradesToRedact — Phase 2 leaves artifacts in the
// "block degrades to redact" state until the SECRET_LEAK failure
// class lands at the result.json/tool_audit/container_logs
// checkpoints first. Verifies the degradation path doesn't write
// the secret to disk even when block is configured.
func TestStore_BlockDegradesToRedact(t *testing.T) {
	store := newTestStoreWithSecrets(t, map[string]secrets.Action{
		secrets.CheckpointArtifacts: secrets.ActionBlock,
	})
	src := writeTempSource(t, "leaked.md", "ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234")
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "leaked.md", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "sk-ant-api03",
		"block degradation must still scrub the secret from durable storage")
	assert.Contains(t, string(stored), "[REDACTED:anthropic_key]")
}

// TestStore_BinaryMimeTypeSkipsScan — random bytes in compressed
// payloads can spuriously hit detector regexes (jwt, generic_kv).
// Binary types must pass through the backend write unmodified.
func TestStore_BinaryMimeTypeSkipsScan(t *testing.T) {
	store := newTestStoreWithSecrets(t, nil)
	// .png triggers detectMimeType -> "image/png" -> skipped.
	// Use bytes that would otherwise trip the entropy detector.
	bin := make([]byte, 256)
	for i := range bin {
		bin[i] = byte(i)
	}
	src := filepath.Join(t.TempDir(), "blob.png")
	require.NoError(t, os.WriteFile(src, bin, 0644))

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "blob.png", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Equal(t, bin, stored, "binary types must be stored byte-for-byte")
}

// TestStore_NilDetectorIsNoop — secrets layer disabled by config.
// The store must use the streaming copy path and not crash on the
// nil detector.
func TestStore_NilDetectorIsNoop(t *testing.T) {
	repo := NewMockArtifactRepo()
	store, err := New(
		WithBasePath(t.TempDir()),
		WithRepository(repo),
		WithLogger(zerolog.Nop()),
	)
	require.NoError(t, err)

	src := writeTempSource(t, "result.md", "key=sk-proj1234567890abcdefghijklmnopqrstuv")
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Contains(t, string(stored), "sk-proj1234567890",
		"nil detector means no scan; source bytes pass through")
}

// TestSetSecrets_PostConstruction — the service container builds
// the store before the secrets detector. SetSecrets is the late-
// binding setter that wires them up. Verify it doesn't panic when
// called on a store that previously had no detector.
func TestSetSecrets_PostConstruction(t *testing.T) {
	repo := NewMockArtifactRepo()
	store, err := New(WithBasePath(t.TempDir()), WithRepository(repo), WithLogger(zerolog.Nop()))
	require.NoError(t, err)

	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	store.SetSecrets(det, nil)

	src := writeTempSource(t, "r.md", "key=sk-proj1234567890abcdefghijklmnopqrstuv")
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "r.md", src)
	require.NoError(t, err)
	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Contains(t, string(stored), "[REDACTED:openai_key]")
}
