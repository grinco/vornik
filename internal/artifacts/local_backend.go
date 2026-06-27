// Package artifacts: local_backend.go implements FileBackend on
// top of the local filesystem. It is the default backend and is
// kept byte-for-byte compatible with the pre-phase-4 Store.* code
// paths so an upgrade from any earlier release is a no-op for
// filesystem-backed deployments.
//
// Key handling: keys are resolved relative to BasePath via the
// safepath package, so traversal attempts (`..`, absolute paths)
// are rejected at write time and re-validated at read time. The
// S3 backend uses the same logical key but with bucket+prefix
// rather than basePath; tests in repotest exercise both layouts.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/safepath"
)

// LocalBackend is the default FileBackend, writing under BasePath
// on the local filesystem. Concurrent writes to the same key are
// last-writer-wins (same as the previous os.Create-based behaviour).
type LocalBackend struct {
	basePath string
}

// NewLocalBackend constructs a LocalBackend rooted at basePath. The
// directory is created (recursively) if it does not exist.
// basePath="./artifacts" is the historical default.
func NewLocalBackend(basePath string) (*LocalBackend, error) {
	if basePath == "" {
		basePath = "./artifacts"
	}
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		return nil, fmt.Errorf("artifacts/local: mkdir %q: %w", basePath, err)
	}
	return &LocalBackend{basePath: basePath}, nil
}

// BasePath returns the root directory the backend writes under.
// Exposed so the artifact Store can keep its assertUnderBase guard
// path-consistent with the backend's view.
func (b *LocalBackend) BasePath() string { return b.basePath }

// resolveKey turns an opaque key into an absolute filesystem path
// under basePath, rejecting traversal attempts. Empty keys produce
// an error rather than returning basePath itself — callers should
// always pass a non-empty key.
func (b *LocalBackend) resolveKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("artifacts/local: empty key")
	}
	// Trim leading slashes — S3-style keys often start with "/", but
	// safepath.JoinUnder treats those as absolute and rejects them.
	trimmed := strings.TrimLeft(key, "/")
	// safepath rejects ".." segments and absolute components.
	joined, err := safepath.JoinUnder(b.basePath, trimmed)
	if err != nil {
		return "", fmt.Errorf("artifacts/local: resolve key %q: %w", key, err)
	}
	return joined, nil
}

// Put writes r's bytes to the given key, creating intermediate
// directories as needed. Returns the number of bytes written.
func (b *LocalBackend) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	abs, err := b.resolveKey(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, fmt.Errorf("artifacts/local: mkdir parent: %w", err)
	}
	f, err := os.Create(abs)
	if err != nil {
		return 0, fmt.Errorf("artifacts/local: create %q: %w", key, err)
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, r)
	if err != nil {
		_ = os.Remove(abs)
		return 0, fmt.Errorf("artifacts/local: write %q: %w", key, err)
	}
	return n, nil
}

// Get opens the file at key for reading. Returns ErrNotFound if the
// key does not exist.
func (b *LocalBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := b.resolveKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("artifacts/local: open %q: %w", key, err)
	}
	return f, nil
}

// Delete removes the file at key. Non-existent keys are a no-op
// (matches S3 DeleteObject + the legacy Store.Delete contract).
func (b *LocalBackend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := b.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("artifacts/local: remove %q: %w", key, err)
	}
	return nil
}

// Exists reports whether a file exists at key.
func (b *LocalBackend) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	abs, err := b.resolveKey(key)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("artifacts/local: stat %q: %w", key, err)
	}
	return true, nil
}

// Stat returns size + key for the file at key.
func (b *LocalBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	abs, err := b.resolveKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("artifacts/local: stat %q: %w", key, err)
	}
	return ObjectInfo{Key: key, Size: info.Size()}, nil
}

// List walks every file under prefix and invokes fn with its
// metadata. Directories are skipped; only regular files surface to
// the walker. Order follows filepath.Walk's lexical traversal —
// callers that need a stable order across backends should not rely
// on it (the S3 backend's order is also unspecified).
func (b *LocalBackend) List(ctx context.Context, prefix string, fn WalkFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rootDir := b.basePath
	if prefix != "" {
		// Strip leading slashes for parity with resolveKey.
		trimmed := strings.TrimLeft(prefix, "/")
		// Find the longest path component that is a directory; the
		// remainder is treated as a filename prefix.
		joined, err := safepath.JoinUnder(b.basePath, trimmed)
		if err != nil {
			return fmt.Errorf("artifacts/local: resolve list prefix %q: %w", prefix, err)
		}
		// If the resolved path is a directory, walk it; otherwise
		// walk the parent and filter by the file-name prefix.
		if st, statErr := os.Stat(joined); statErr == nil && st.IsDir() {
			rootDir = joined
		} else {
			// Walk parent dir; filter by basename prefix below.
			rootDir = filepath.Dir(joined)
		}
	}
	// If the root does not exist (e.g. prefix points at nothing),
	// treat as an empty result. S3 ListObjectsV2 returns an empty
	// page in the analogous case.
	if _, err := os.Stat(rootDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("artifacts/local: stat list root %q: %w", rootDir, err)
	}

	absPrefix, _ := b.resolveKey(strings.TrimLeft(prefix, "/"))
	walkErr := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// Filter by the original prefix (so "foo/bar" matches both
		// "foo/bar" the dir and "foo/barbaz" the file at the same
		// depth).
		if prefix != "" && absPrefix != "" && !strings.HasPrefix(path, absPrefix) {
			return nil
		}
		rel, relErr := filepath.Rel(b.basePath, path)
		if relErr != nil {
			return relErr
		}
		// Normalize to forward slashes for backend-portable keys.
		key := filepath.ToSlash(rel)
		if walkCtxErr := ctx.Err(); walkCtxErr != nil {
			return walkCtxErr
		}
		if cbErr := fn(ObjectInfo{Key: key, Size: info.Size()}); cbErr != nil {
			if errors.Is(cbErr, io.EOF) {
				return filepath.SkipAll
			}
			return cbErr
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		return walkErr
	}
	return nil
}

// Close is a no-op for the local backend; included so callers can
// `defer backend.Close()` without branching on the type.
func (b *LocalBackend) Close() error { return nil }

// Compile-time check.
var _ FileBackend = (*LocalBackend)(nil)
