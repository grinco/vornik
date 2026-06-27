package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingBackend captures every backend operation so tests can
// assert the Store layer actually delegates Put / Get / Delete to
// the backend (rather than the pre-phase-4 direct os.* paths).
type recordingBackend struct {
	puts    map[string][]byte
	gets    []string
	deletes []string
	putErr  error
	getErr  error
	delErr  error
}

func newRecordingBackend() *recordingBackend {
	return &recordingBackend{puts: make(map[string][]byte)}
}

func (r *recordingBackend) Put(_ context.Context, key string, src io.Reader) (int64, error) {
	if r.putErr != nil {
		return 0, r.putErr
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return 0, err
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	r.puts[key] = cp
	return int64(len(b)), nil
}

func (r *recordingBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	r.gets = append(r.gets, key)
	body, ok := r.puts[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (r *recordingBackend) Delete(_ context.Context, key string) error {
	if r.delErr != nil {
		return r.delErr
	}
	r.deletes = append(r.deletes, key)
	delete(r.puts, key)
	return nil
}

func (r *recordingBackend) Exists(_ context.Context, key string) (bool, error) {
	_, ok := r.puts[key]
	return ok, nil
}

func (r *recordingBackend) Stat(_ context.Context, key string) (ObjectInfo, error) {
	b, ok := r.puts[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (r *recordingBackend) List(_ context.Context, prefix string, fn WalkFunc) error {
	for k, b := range r.puts {
		if strings.HasPrefix(k, prefix) {
			if err := fn(ObjectInfo{Key: k, Size: int64(len(b))}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *recordingBackend) Close() error { return nil }

// TestWithBackend_OverridesDefault — when WithBackend is supplied,
// New() must NOT silently overwrite it with a LocalBackend.
func TestWithBackend_OverridesDefault(t *testing.T) {
	rb := newRecordingBackend()
	s, err := New(
		WithBasePath(t.TempDir()),
		WithBackend(rb),
	)
	require.NoError(t, err)
	assert.Same(t, rb, s.backend,
		"WithBackend must take precedence over the default LocalBackend")
}

// TestNew_DefaultsToLocalBackend — pre-phase-4 callers that
// construct Store without WithBackend still get a working backend
// (the LocalBackend at basePath). This is what keeps every
// existing test green through the refactor.
func TestNew_DefaultsToLocalBackend(t *testing.T) {
	tmp := t.TempDir()
	s, err := New(WithBasePath(tmp))
	require.NoError(t, err)
	require.NotNil(t, s.backend, "default backend must be wired")
	lb, ok := s.backend.(*LocalBackend)
	require.True(t, ok, "default backend must be *LocalBackend, got %T", s.backend)
	assert.Equal(t, tmp, lb.BasePath())
}

// TestStore_PutGoesThroughBackend — Store.Store calls backend.Put
// with the relative key (not the absolute filesystem path the DB
// records). That's the abstraction needed for S3 — the DB still
// records an absolute path for back-compat, but the actual write
// uses a relative key the bucket understands.
func TestStore_PutGoesThroughBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "source.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello backend"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)

	art, err := s.Store(context.Background(), "proj1", "exec1", "task1", "out.txt", src)
	require.NoError(t, err)

	// Key the backend received must NOT be the absolute path
	// (S3 keys can't start with /).
	expectedKey := "proj1/exec1/out.txt"
	require.Contains(t, rb.puts, expectedKey,
		"backend.Put was not called with the relative storage key")
	assert.Equal(t, []byte("hello backend"), rb.puts[expectedKey])

	// DB row still records absolute path for back-compat with
	// callers that read StoragePath directly.
	assert.True(t, strings.HasPrefix(art.StoragePath, tmp),
		"recorded StoragePath %q should still be under basePath", art.StoragePath)
}

// TestStoreInput_PutGoesThroughBackend mirrors the same assertion
// for the input artifact path (Telegram upload, API attachment).
func TestStoreInput_PutGoesThroughBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "input.txt")
	require.NoError(t, os.WriteFile(src, []byte("input body"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)

	art, err := s.StoreInput(context.Background(), "proj1", "attached.txt", src)
	require.NoError(t, err)
	require.NotNil(t, art)

	// Expected key shape: proj1/inputs/{artifactID}/attached.txt
	found := false
	for k := range rb.puts {
		if strings.HasPrefix(k, "proj1/inputs/") && strings.HasSuffix(k, "/attached.txt") {
			found = true
			assert.Equal(t, []byte("input body"), rb.puts[k])
		}
	}
	assert.True(t, found, "backend.Put was not called for the input artifact (puts=%v)", rb.puts)
}

// TestRetrieve_GoesThroughBackend covers the read path.
func TestRetrieve_GoesThroughBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "source.txt")
	require.NoError(t, os.WriteFile(src, []byte("retrieve me"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)
	art, err := s.Store(context.Background(), "p", "e", "t", "r.txt", src)
	require.NoError(t, err)

	data, err := s.Retrieve(context.Background(), art.ID)
	require.NoError(t, err)
	assert.Equal(t, []byte("retrieve me"), data)
	assert.NotEmpty(t, rb.gets, "backend.Get was not called by Retrieve")
}

// TestDelete_GoesThroughBackend covers the delete path.
func TestDelete_GoesThroughBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "source.txt")
	require.NoError(t, os.WriteFile(src, []byte("delete me"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)
	art, err := s.Store(context.Background(), "p", "e", "t", "d.txt", src)
	require.NoError(t, err)

	require.NoError(t, s.Delete(context.Background(), art.ID))
	require.NotEmpty(t, rb.deletes, "backend.Delete was not called")
	assert.Equal(t, "p/e/d.txt", rb.deletes[0])

	_, err = s.Retrieve(context.Background(), art.ID)
	require.Error(t, err, "Retrieve must fail after Delete")
}

// TestDeriveKey_StripsBasePath covers the back-compat path: legacy
// DB rows store an absolute path under basePath; the backend needs
// a relative key. Uses a real tempdir so New() doesn't fall back
// to ./artifacts on a non-creatable path.
func TestDeriveKey_StripsBasePath(t *testing.T) {
	tmp := t.TempDir()
	s, err := New(WithBasePath(tmp))
	require.NoError(t, err)

	cases := []struct {
		name, in, want string
	}{
		{"absolute under base", filepath.Join(tmp, "p/e/file.txt"), "p/e/file.txt"},
		{"s3 key (no prefix)", "p/e/file.txt", "p/e/file.txt"},
		{"empty", "", ""},
		{"single-segment under base", filepath.Join(tmp, "p"), "p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, s.deriveKey(tc.in))
		})
	}
}

// TestStore_DBFailureCleansUpBackend — when the DB Create fails,
// the artifact bytes already written via backend.Put must be
// cleaned up. Otherwise S3 fills up with orphans on transient DB
// errors.
func TestStore_DBFailureCleansUpBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "source.txt")
	require.NoError(t, os.WriteFile(src, []byte("orphan check"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	repo.createErr = errors.New("simulated DB outage")
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)

	_, err = s.Store(context.Background(), "p", "e", "t", "x.txt", src)
	require.Error(t, err, "Store must propagate DB Create errors")
	require.NotEmpty(t, rb.deletes, "DB-failure cleanup did not call backend.Delete (orphan risk)")
}

// TestOpen_StreamsViaBackend covers the streaming read seam Open
// adds for callers that want an io.ReadCloser (HTTP ServeContent,
// Telegram document upload) instead of buffering with Retrieve.
func TestOpen_StreamsViaBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "source.txt")
	require.NoError(t, os.WriteFile(src, []byte("stream me"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)
	art, err := s.Store(context.Background(), "p", "e", "t", "o.txt", src)
	require.NoError(t, err)

	rc, err := s.Open(context.Background(), art.ID)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, rc)
	require.NoError(t, err)
	assert.Equal(t, []byte("stream me"), buf.Bytes())
	assert.NotEmpty(t, rb.gets, "backend.Get must be called by Open")
}

// TestOpen_RepoMissingReturnsNotFound — Open must fail closed when
// the artifact ID isn't in the repo, not panic on a nil dereference.
func TestOpen_RepoMissingReturnsNotFound(t *testing.T) {
	tmp := t.TempDir()
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)

	_, err = s.Open(context.Background(), "does-not-exist")
	require.Error(t, err)
}

// TestStoreInput_DBFailureCleansUpBackend mirrors the same cleanup
// contract for the input-artifact path.
func TestStoreInput_DBFailureCleansUpBackend(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "in.txt")
	require.NoError(t, os.WriteFile(src, []byte("orphan input"), 0o644))
	rb := newRecordingBackend()
	repo := NewMockArtifactRepo()
	repo.createErr = errors.New("simulated DB outage")
	s, err := New(
		WithBasePath(tmp),
		WithBackend(rb),
		WithRepository(repo),
	)
	require.NoError(t, err)

	_, err = s.StoreInput(context.Background(), "p", "i.txt", src)
	require.Error(t, err)
	require.NotEmpty(t, rb.deletes,
		"StoreInput DB-failure cleanup did not call backend.Delete")
}
