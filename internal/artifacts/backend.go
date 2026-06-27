// Package artifacts: backend.go defines the FileBackend interface
// that artifact storage drivers implement, plus the LocalBackend
// (filesystem) reference impl extracted from the original
// os.*-based code paths.
//
// Phase 4 of the storage abstraction (see
// https://docs.vornik.io) introduced
// FileBackend so the daemon can swap local-filesystem storage for an
// S3-compatible object store without touching upstream callers.
// Backend pickers live in internal/storage/artifactbackend.go.
//
// FileBackend semantics:
//
//   - Put/Get/Delete are addressed by an opaque storage *key*. Local
//     callers see this key as a filesystem path under basePath; S3
//     callers see it as an object key under their bucket+prefix. The
//     Store layer normalises both shapes via PathKey.
//
//   - All methods are context-cancelable. Local fs ops honour ctx by
//     pre-checking ctx.Err(); S3 ops thread ctx through the SDK.
//
//   - Get returns an io.ReadCloser; the caller closes it. For text
//     artifacts the Store will buffer + verify hash on top.
//
//   - List uses a callback walker rather than returning a slice so
//     paginated S3 results stream without buffering everything in
//     memory. Stop iteration by returning an error from the walker;
//     io.EOF is treated as a normal stop signal.
package artifacts

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get/Stat/Delete when the requested key
// does not exist. Both LocalBackend and the S3 backend wrap their
// driver-native not-found errors in this sentinel so callers can
// branch on it uniformly.
var ErrNotFound = errors.New("artifacts: object not found")

// ObjectInfo carries the metadata Stat returns. Size is in bytes;
// ETag is the storage-driver-specific content tag (S3 object ETag,
// or "" for the local backend — the artifact Store re-computes its
// own SHA-256 hash on write so the content-hash truth lives there).
type ObjectInfo struct {
	Key  string
	Size int64
	ETag string
}

// WalkFunc is invoked for each object emitted by List. Return a
// non-nil error to stop iteration; that error propagates back to
// List's caller. Returning io.EOF is treated as a normal stop and
// converted to nil.
type WalkFunc func(ObjectInfo) error

// FileBackend is the artifact-blob storage interface. All methods
// take a context and must honor cancellation. The local-filesystem
// reference impl (LocalBackend) preserves the pre-phase-4 behaviour;
// the S3 impl (s3Backend, in internal/artifacts/s3) is wire-
// compatible with AWS S3, MinIO, and Ceph RGW.
//
// The interface deliberately stays minimal — Read/Write/Delete/Stat/
// Exists/List — so an alternative backend (GCS, Azure Blob, IPFS)
// can land as a single ~200 LOC file plus a factory case.
type FileBackend interface {
	// Put writes the bytes from r to the storage at key. Returns the
	// number of bytes written. Overwrites if the key already exists
	// (matches both POSIX semantics and S3 default behaviour).
	Put(ctx context.Context, key string, r io.Reader) (int64, error)

	// Get returns a reader streaming the object's bytes. The caller
	// must close the returned ReadCloser. Returns ErrNotFound if the
	// key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the object at key. Returns nil for non-existent
	// keys (idempotent — matches S3 DeleteObject's behaviour and the
	// existing Store.Delete's "ignore not-exists" branch).
	Delete(ctx context.Context, key string) error

	// Exists reports whether an object exists at key.
	Exists(ctx context.Context, key string) (bool, error)

	// Stat returns metadata for the object at key. Returns ErrNotFound
	// if the key does not exist.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// List walks every object whose key starts with prefix, invoking
	// fn for each one. Iteration is paginated transparently by the
	// driver (S3 continuation tokens, filesystem walk). Order is not
	// guaranteed.
	List(ctx context.Context, prefix string, fn WalkFunc) error

	// Close releases any backend-held resources (HTTP clients,
	// connection pools). Idempotent.
	Close() error
}
