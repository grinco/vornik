// Regression test for incident-telegram-upload-input-roots-20260712
// (latent half): the dispatcher's create_task input-confinement gate
// type-asserts its InputArtifactStore against a BasePath() string
// capability to derive the artifact-store root of its allow-list.
// *Store never implemented it (only LocalBackend did), so the root
// silently vanished and prod allowed_roots collapsed to ["/tmp"].
package artifacts

import (
	"context"
	"io"
	"testing"
)

// nonLocalBackend is a minimal FileBackend that is NOT a LocalBackend,
// standing in for the S3 driver.
type nonLocalBackend struct{}

func (nonLocalBackend) Put(context.Context, string, io.Reader) (int64, error) { return 0, nil }
func (nonLocalBackend) Get(context.Context, string) (io.ReadCloser, error)    { return nil, ErrNotFound }
func (nonLocalBackend) Delete(context.Context, string) error                  { return nil }
func (nonLocalBackend) Exists(context.Context, string) (bool, error)          { return false, nil }
func (nonLocalBackend) Stat(context.Context, string) (ObjectInfo, error) {
	return ObjectInfo{}, ErrNotFound
}
func (nonLocalBackend) List(context.Context, string, WalkFunc) error { return nil }
func (nonLocalBackend) Close() error                                 { return nil }

func TestStoreBasePath_LocalBackend(t *testing.T) {
	base := t.TempDir()
	s, err := New(WithBasePath(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.BasePath(); got != base {
		t.Fatalf("BasePath() = %q, want %q", got, base)
	}
}

func TestStoreBasePath_NonLocalBackend(t *testing.T) {
	s, err := New(WithBasePath(t.TempDir()), WithBackend(nonLocalBackend{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A non-filesystem backend has no local root literal paths could
	// legitimately live under — exposing one would widen the
	// dispatcher's read-primitive allow-list for no reason.
	if got := s.BasePath(); got != "" {
		t.Fatalf("BasePath() = %q, want empty for non-local backend", got)
	}
}
