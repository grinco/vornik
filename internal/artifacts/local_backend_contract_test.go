package artifacts_test

import (
	"testing"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/artifacts/backendtest"
)

// TestLocalBackend_Contract proves the LocalBackend satisfies the
// FileBackend contract used by every consumer of artifact storage,
// independent of the filesystem-specific paths exercised in
// local_backend_test.go.
func TestLocalBackend_Contract(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) (artifacts.FileBackend, func()) {
		b, err := artifacts.NewLocalBackend(t.TempDir())
		if err != nil {
			t.Fatalf("NewLocalBackend: %v", err)
		}
		return b, func() { _ = b.Close() }
	})
}
