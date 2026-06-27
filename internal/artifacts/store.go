// Package artifacts provides artifact storage for vornik.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/secrets"
)

// Store manages artifact persistence backed by a FileBackend. The
// backend handles the actual blob I/O (filesystem, S3, future
// drivers); the Store layer owns layout decisions, secret-scan,
// hashing, and the DB row.
//
// Before phase 4 (2026-05-20) the Store wrote artifacts via direct
// os.Create / read via os.ReadFile, and `basePath` doubled as both
// the filesystem root AND the implicit storage-key prefix. Phase 4
// introduced FileBackend so the same Store can drive S3-shaped
// backends without conditional logic in upstream callers. New()
// constructs a default LocalBackend at basePath when WithBackend
// isn't supplied — keeps the historical zero-config behaviour
// intact for tests + boot-without-backend scenarios.
type Store struct {
	basePath string
	backend  FileBackend
	repo     persistence.ArtifactRepository
	logger   zerolog.Logger

	// secretsDetector (optional) scans text-class artifact content
	// before storage; downstream consumers (memory ingest, UI
	// display, agent file_read) see redacted bytes. Binary classes
	// (images, PDFs, archives) are skipped — regex matches on
	// arbitrary bytes can produce false positives that corrupt the
	// payload, and operators don't normally smuggle secrets into
	// images.
	secretsDetector secrets.Detector
	secretsActions  map[string]secrets.Action
}

// StoreOption is a functional option for configuring the Store.
type StoreOption func(*Store)

// WithBasePath sets the base path for artifact storage.
func WithBasePath(path string) StoreOption {
	return func(s *Store) {
		s.basePath = path
	}
}

// WithRepository sets the artifact repository.
func WithRepository(repo persistence.ArtifactRepository) StoreOption {
	return func(s *Store) {
		s.repo = repo
	}
}

// WithLogger sets the logger used for secret-leak findings.
func WithLogger(l zerolog.Logger) StoreOption {
	return func(s *Store) {
		s.logger = l
	}
}

// WithSecrets wires the secret-leak detector and per-checkpoint
// action map. A nil detector disables scanning.
func WithSecrets(d secrets.Detector, actions map[string]secrets.Action) StoreOption {
	return func(s *Store) {
		s.secretsDetector = d
		s.secretsActions = actions
	}
}

// WithBackend wires an explicit FileBackend. When omitted, New()
// constructs a LocalBackend at basePath so the Store always has a
// working backend (preserves the historical zero-config behaviour
// for tests + boot-without-config paths). Operators wire the S3
// backend via storage.OpenArtifactBackend → WithBackend in the
// service container.
func WithBackend(b FileBackend) StoreOption {
	return func(s *Store) {
		s.backend = b
	}
}

// SetSecrets wires the detector after construction. The artifact
// store is initialized before the secrets detector in the service
// container's boot sequence, so the detector lands via this setter
// once it's available.
func (s *Store) SetSecrets(d secrets.Detector, actions map[string]secrets.Action) {
	s.secretsDetector = d
	s.secretsActions = actions
}

// SetLogger replaces the store's logger. Like SetSecrets, used to
// inject the daemon logger after construction.
func (s *Store) SetLogger(l zerolog.Logger) {
	s.logger = l
}

// DeleteProjectArtifacts removes every artifact blob stored under
// the project's key prefix. Used by the archive sweeper to hard-
// delete an archived project's blobs alongside its DB rows.
//
// Works against both the filesystem and S3 backends — the function
// walks the backend's List(prefix) results and Deletes each key.
// Idempotent: nonexistent keys + nonexistent prefixes return nil.
//
// Returns the count of keys deleted. errReturn is the first
// Delete error encountered, if any; the walker still iterates the
// full list so a partial failure doesn't leak the remaining keys.
func (s *Store) DeleteProjectArtifacts(ctx context.Context, projectID string) (int, error) {
	if s == nil || s.backend == nil {
		return 0, fmt.Errorf("artifacts: store backend not wired")
	}
	if projectID == "" {
		return 0, fmt.Errorf("artifacts: empty projectID would wipe every project's blobs")
	}
	// LocalBackend resolves keys under basePath; storage paths in
	// the DB row record absolute paths under basePath/<projectID>/.
	// Both backends use the same prefix convention — the prefix
	// argument is just the project ID.
	prefix := projectID + "/"

	var (
		deleted  int
		firstErr error
		toDelete []string
	)
	if err := s.backend.List(ctx, prefix, func(info ObjectInfo) error {
		toDelete = append(toDelete, info.Key)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("artifacts: list project prefix %q: %w", prefix, err)
	}
	for _, key := range toDelete {
		if err := s.backend.Delete(ctx, key); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleted++
	}
	return deleted, firstErr
}

// New creates a new artifact Store. If no FileBackend was supplied
// via WithBackend, a LocalBackend at basePath is wired automatically
// — that keeps every Store fully functional even in the absence of
// the (newer) backend-aware boot path.
func New(opts ...StoreOption) (*Store, error) {
	s := &Store{
		basePath: "./artifacts",
	}

	for _, opt := range opts {
		opt(s)
	}

	// Ensure base path exists. The S3 backend doesn't need it, but
	// the Store layer still records absolute paths in `StoragePath`
	// for back-compat with callers that read the column directly,
	// and a few code paths still buffer through a temp file under
	// basePath. So basePath stays a real on-disk root regardless of
	// which backend handles the actual blob.
	if err := os.MkdirAll(s.basePath, 0755); err != nil {
		if errors.Is(err, os.ErrPermission) && s.basePath != "./artifacts" {
			if fallbackErr := os.MkdirAll("./artifacts", 0755); fallbackErr == nil {
				s.basePath = "./artifacts"
				// Fall through to backend defaulting below.
			} else {
				return nil, fmt.Errorf("failed to create artifacts directory: %w", err)
			}
		} else {
			return nil, fmt.Errorf("failed to create artifacts directory: %w", err)
		}
	}

	// Default backend wiring. Tests and pre-phase-4 boot paths
	// construct Store without WithBackend; a LocalBackend at the
	// same basePath gives the new code path something to call so
	// every Store.* operation routes through ONE I/O abstraction
	// instead of branching on `backend == nil` at every site.
	if s.backend == nil {
		lb, err := NewLocalBackend(s.basePath)
		if err != nil {
			return nil, fmt.Errorf("failed to construct default local backend: %w", err)
		}
		s.backend = lb
	}

	return s, nil
}

// deriveKey turns an artifact's `StoragePath` (the value persisted
// in the DB) into the relative key the FileBackend expects.
//
// Historical filesystem rows have absolute paths under basePath;
// stripping the basePath prefix yields `{project}/{exec}/{name}`
// which LocalBackend resolves back to the same absolute path and
// which S3 Backend uses verbatim as the object key.
//
// S3-native rows (artifacts created while the daemon was running
// against the S3 backend) record the key directly — no prefix to
// strip — so the value is returned unchanged.
//
// Empty input returns empty output; Retrieve guards against that
// before calling.
func (s *Store) deriveKey(storagePath string) string {
	if storagePath == "" || s.basePath == "" {
		return storagePath
	}
	prefix := strings.TrimRight(s.basePath, string(filepath.Separator)) + string(filepath.Separator)
	if strings.HasPrefix(storagePath, prefix) {
		return strings.TrimPrefix(storagePath, prefix)
	}
	return storagePath
}

// Store copies a file to the artifact store and creates a database record.
// Returns the created artifact.
// Store persists an artifact file + DB record. taskID is optional — pass
// empty string when the artifact isn't associated with a specific task
// (rare; most agent outputs come from a task-scoped execution). Without
// taskID, downstream consumers that filter by task (e.g. Telegram
// `sendArtifactsToWatchers`) won't find the artifact, so callers should
// always pass it when available.
func (s *Store) Store(ctx context.Context, projectID, executionID, taskID, name, sourcePath string) (*persistence.Artifact, error) {
	// Generate artifact ID
	artifactID := persistence.GenerateID("artifact")

	// Create storage path: {basePath}/{projectID}/{executionID}/{name}
	safeProjectID, err := safepath.CleanPathComponent(projectID)
	if err != nil {
		return nil, fmt.Errorf("invalid project ID: %w", err)
	}
	safeExecutionID, err := safepath.CleanPathComponent(executionID)
	if err != nil {
		return nil, fmt.Errorf("invalid execution ID: %w", err)
	}
	safeName, err := safepath.CleanFileName(name)
	if err != nil {
		return nil, fmt.Errorf("invalid artifact name: %w", err)
	}
	// Compute the storage path (absolute, under basePath) for the DB
	// row's StoragePath column. The actual write goes through the
	// backend at the derived key; basePath is retained here so the
	// recorded path remains back-compat with the (still un-migrated)
	// upstream callers that read it directly.
	storageDir, err := safepath.JoinUnder(s.basePath, safeProjectID, safeExecutionID)
	if err != nil {
		return nil, fmt.Errorf("invalid storage directory: %w", err)
	}
	storagePath, err := safepath.JoinUnder(storageDir, safeName)
	if err != nil {
		return nil, fmt.Errorf("invalid storage path: %w", err)
	}
	storageKey := filepath.ToSlash(filepath.Join(safeProjectID, safeExecutionID, safeName))

	// Determine MIME type up front so the secret-scan path knows
	// whether the file is text-shaped (worth scanning) or a binary
	// blob (skipped to avoid false positives on random byte runs).
	mimeType := detectMimeType(name)

	// Read + scan + write through the backend. The legacy
	// persistAndScan helper still owns the read/scan/redact logic;
	// it returns the bytes to write, then we hand them to the
	// backend in a single Put.
	hash, size, body, err := s.scanForBackend(sourcePath, mimeType, projectID, taskID, safeName)
	if err != nil {
		return nil, fmt.Errorf("failed to scan artifact: %w", err)
	}
	if _, err := s.backend.Put(ctx, storageKey, bytes.NewReader(body)); err != nil {
		return nil, fmt.Errorf("failed to write artifact via backend: %w", err)
	}

	// Create artifact record. task_id is a *string pointer — only set
	// when the caller provided a non-empty taskID, otherwise left nil
	// (NULL in the DB). Telegram's sendArtifactsToWatchers filters by
	// task_id, so missing this value is exactly why attachments went
	// silent — callers should always pass it when they can.
	artifact := &persistence.Artifact{
		ID:                artifactID,
		ProjectID:         projectID,
		ExecutionID:       &executionID,
		Name:              safeName,
		ArtifactClass:     persistence.ArtifactClassOutput,
		StoragePath:       storagePath,
		SizeBytes:         &size,
		ContentHashSHA256: &hash,
		MimeType:          &mimeType,
		CreatedAt:         time.Now(),
		Origin:            persistence.ArtifactOriginTaskOutput,
	}
	if taskID != "" {
		artifact.TaskID = &taskID
	}

	// Persist to database
	if s.repo != nil {
		if err := s.repo.Create(ctx, artifact); err != nil {
			// Clean up via the same backend used for the write so
			// S3 cleanups don't leave orphans.
			_ = s.backend.Delete(ctx, storageKey)
			return nil, fmt.Errorf("failed to create artifact record: %w", err)
		}
	}

	return artifact, nil
}

// StoreInput persists a user-supplied input file (Telegram upload, API
// task attachment) at a stable location with class=INPUT. Layout is
// {basePath}/{projectID}/inputs/{artifactID}/{name} — keyed by the
// artifact ID rather than by execution or task because inputs typically
// arrive before any task exists. The caller links the artifact to a
// task afterwards via repo.UpdateTaskID once the task ID is allocated.
//
// The original file at sourcePath is left in place; this is a copy, not
// a move. That preserves the operator-visible upload trail in the
// project workspace and keeps the artifact store as the durable
// source-of-truth for retries — the two paths can diverge if the
// workspace upload is later cleaned up.
func (s *Store) StoreInput(ctx context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error) {
	artifactID := persistence.GenerateID("artifact")

	safeProjectID, err := safepath.CleanPathComponent(projectID)
	if err != nil {
		return nil, fmt.Errorf("invalid project ID: %w", err)
	}
	safeArtifactID, err := safepath.CleanPathComponent(artifactID)
	if err != nil {
		return nil, fmt.Errorf("invalid artifact ID: %w", err)
	}
	safeName, err := safepath.CleanFileName(name)
	if err != nil {
		return nil, fmt.Errorf("invalid artifact name: %w", err)
	}
	storageDir, err := safepath.JoinUnder(s.basePath, safeProjectID, "inputs", safeArtifactID)
	if err != nil {
		return nil, fmt.Errorf("invalid storage directory: %w", err)
	}
	storagePath, err := safepath.JoinUnder(storageDir, safeName)
	if err != nil {
		return nil, fmt.Errorf("invalid storage path: %w", err)
	}
	storageKey := filepath.ToSlash(filepath.Join(safeProjectID, "inputs", safeArtifactID, safeName))

	mimeType := detectMimeType(name)
	hash, size, body, err := s.scanForBackend(sourcePath, mimeType, projectID, "", safeName)
	if err != nil {
		return nil, fmt.Errorf("failed to scan input artifact: %w", err)
	}
	if _, err := s.backend.Put(ctx, storageKey, bytes.NewReader(body)); err != nil {
		return nil, fmt.Errorf("failed to write input artifact via backend: %w", err)
	}

	artifact := &persistence.Artifact{
		ID:                artifactID,
		ProjectID:         projectID,
		Name:              safeName,
		ArtifactClass:     persistence.ArtifactClassInput,
		StoragePath:       storagePath,
		SizeBytes:         &size,
		ContentHashSHA256: &hash,
		MimeType:          &mimeType,
		CreatedAt:         time.Now(),
		Origin:            persistence.ArtifactOriginUpload,
	}

	if s.repo != nil {
		if err := s.repo.Create(ctx, artifact); err != nil {
			_ = s.backend.Delete(ctx, storageKey)
			return nil, fmt.Errorf("failed to create input artifact record: %w", err)
		}
	}
	return artifact, nil
}

// Retrieve reads an artifact from storage via the configured
// FileBackend. Existing filesystem rows record an absolute path in
// `StoragePath`; deriveKey strips the basePath prefix so the
// LocalBackend / S3 Backend sees a relative key.
func (s *Store) Retrieve(ctx context.Context, artifactID string) ([]byte, error) {
	if s.repo == nil {
		return nil, ErrNoRepository
	}

	// Get artifact metadata
	artifact, err := s.repo.Get(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("failed to get artifact: %w", err)
	}
	if artifact == nil {
		// Repository contract is that Get returns (nil, ErrNotFound)
		// on miss, but the mock + a couple legacy adapters return
		// (nil, nil). Treat that as not-found rather than panic on
		// StoragePath dereference below.
		return nil, fmt.Errorf("artifact %s not found", artifactID)
	}

	// Re-validate the stored path at read time. Store() guards against
	// traversal at write time via safepath.JoinUnder, but the DB column is
	// plain text — a corrupted or manually-edited row could point outside
	// the artifact root. Skip the basePath-traversal check for S3-shaped
	// keys (which won't have the basePath prefix).
	if s.usesLocalBackend() {
		if err := assertUnderBase(s.basePath, artifact.StoragePath); err != nil {
			return nil, fmt.Errorf("artifact storage path escapes root: %w", err)
		}
	}

	key := s.deriveKey(artifact.StoragePath)
	rc, err := s.backend.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to read artifact via backend: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read artifact bytes: %w", err)
	}

	// Verify hash
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])
	if artifact.ContentHashSHA256 != nil && *artifact.ContentHashSHA256 != hashStr {
		return nil, ErrHashMismatch
	}

	return data, nil
}

// Open returns a streaming reader for an artifact's bytes. The
// caller must Close the returned ReadCloser. Hash verification is
// NOT performed — buffering for verification is what Retrieve does.
// Open is for HTTP file servers, Telegram document uploads, and
// other streaming consumers where the latency of an in-memory copy
// would be wasteful.
//
// Routes through the configured FileBackend, so it works against
// LocalBackend (filesystem) and S3 Backend identically.
func (s *Store) Open(ctx context.Context, artifactID string) (io.ReadCloser, error) {
	if s.repo == nil {
		return nil, ErrNoRepository
	}
	artifact, err := s.repo.Get(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("failed to get artifact: %w", err)
	}
	if artifact == nil {
		return nil, fmt.Errorf("artifact %s not found", artifactID)
	}
	if s.usesLocalBackend() {
		if err := assertUnderBase(s.basePath, artifact.StoragePath); err != nil {
			return nil, fmt.Errorf("artifact storage path escapes root: %w", err)
		}
	}
	return s.backend.Get(ctx, s.deriveKey(artifact.StoragePath))
}

// Delete removes an artifact from storage and database via the
// configured FileBackend.
func (s *Store) Delete(ctx context.Context, artifactID string) error {
	if s.repo == nil {
		return ErrNoRepository
	}

	// Get artifact metadata
	artifact, err := s.repo.Get(ctx, artifactID)
	if err != nil {
		return fmt.Errorf("failed to get artifact: %w", err)
	}
	if artifact == nil {
		return fmt.Errorf("artifact %s not found", artifactID)
	}

	// Defend against a corrupted/manually-edited row pointing outside the
	// artifact root — only meaningful for filesystem-shaped paths.
	if s.usesLocalBackend() {
		if err := assertUnderBase(s.basePath, artifact.StoragePath); err != nil {
			return fmt.Errorf("artifact storage path escapes root: %w", err)
		}
	}

	key := s.deriveKey(artifact.StoragePath)
	if err := s.backend.Delete(ctx, key); err != nil {
		return fmt.Errorf("failed to delete artifact via backend: %w", err)
	}

	// Delete database record
	if err := s.repo.Delete(ctx, artifactID); err != nil {
		return fmt.Errorf("failed to delete artifact record: %w", err)
	}

	return nil
}

// List returns artifacts for an execution.
func (s *Store) List(ctx context.Context, projectID, executionID string) ([]*persistence.Artifact, error) {
	if s.repo == nil {
		return nil, ErrNoRepository
	}

	filter := persistence.ArtifactFilter{
		ProjectID:   &projectID,
		ExecutionID: &executionID,
		PageSize:    100,
	}

	return s.repo.List(ctx, filter)
}

// GetPath returns the filesystem path for an artifact.
func (s *Store) GetPath(artifactID string) (string, error) {
	if s.repo == nil {
		return "", ErrNoRepository
	}

	ctx := context.Background()
	artifact, err := s.repo.Get(ctx, artifactID)
	if err != nil {
		return "", fmt.Errorf("failed to get artifact: %w", err)
	}

	return artifact.StoragePath, nil
}

// usesLocalBackend reports whether artifacts live on the local filesystem
// (a *LocalBackend). Only then is StoragePath a filesystem path that must be
// containment-checked against basePath. Previously the call sites gated on
// strings.HasPrefix(StoragePath, basePath), which a corrupted row could evade:
// a path that filepath.Clean resolves outside the root (e.g. "<base>/../x"
// pre-cleaned to a sibling) no longer textually prefixes basePath, so the
// check was skipped entirely. Gating on the backend type — not the string —
// means every local-backend read/delete is containment-checked; S3-shaped
// keys (object store) are validated by the store, not path traversal.
func (s *Store) usesLocalBackend() bool {
	_, ok := s.backend.(*LocalBackend)
	return ok
}

// assertUnderBase reports whether path, after cleaning and symlink
// resolution when possible, stays under base. Empty base disables the
// check for test scenarios that leave basePath unset. Used by Retrieve
// and Delete to defend against corrupted DB rows pointing outside the
// artifact root.
func assertUnderBase(base, path string) error {
	if base == "" {
		return nil
	}
	cleanBase := filepath.Clean(base)
	if resolved, err := filepath.EvalSymlinks(cleanBase); err == nil {
		cleanBase = resolved
	}
	cleanPath := filepath.Clean(path)
	// Resolve symlinks only if the target exists; Retrieve's ReadFile will
	// surface a clean "file not found" if not.
	if resolved, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = resolved
	}
	rel, err := filepath.Rel(cleanBase, cleanPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || rel == "." || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q outside artifact root %q", cleanPath, cleanBase)
	}
	return nil
}

// scanForBackend reads sourcePath into memory, optionally runs the
// secret-leak scan + redaction, and returns the bytes plus their
// SHA-256 + size. The caller writes the bytes through whatever
// FileBackend it has configured.
//
// Hash + size reflect the bytes actually returned — so a redacted
// artifact's recorded hash matches what Retrieve will serve, not
// the unredacted source's hash.
//
// Replaces the pre-phase-4 persistAndScan helper, which wrote
// directly to a destination filesystem path. The shape change is
// what lets S3 (and any future blob driver) ride the same scan
// pipeline without the helper knowing what storage it's writing to.
func (s *Store) scanForBackend(srcPath, mimeType, projectID, taskID, name string) (string, int64, []byte, error) {
	body, err := os.ReadFile(srcPath)
	if err != nil {
		return "", 0, nil, fmt.Errorf("read source: %w", err)
	}
	if s.secretsDetector != nil && shouldScanBody(mimeType, body) {
		findings := s.secretsDetector.Scan(body)
		if len(findings) > 0 {
			action := secrets.ResolveAction(secrets.CheckpointArtifacts, s.secretsActions)
			counts := secrets.CountByType(findings)
			logEvent := s.logger.Warn().
				Str("project_id", projectID).
				Str("task_id", taskID).
				Str("artifact_name", name).
				Str("mime_type", mimeType).
				Str("checkpoint", secrets.CheckpointArtifacts).
				Str("action", string(action)).
				Int("findings", len(findings)).
				Interface("by_type", counts)
			switch action {
			case secrets.ActionRedact:
				logEvent.Msg("artifacts: scanned — redacting findings before storage")
				body = secrets.Redact(body, findings)
			case secrets.ActionBlock:
				// Block-on-artifacts degrades to Redact. The artifact
				// pipeline doesn't have a clean failure-class wire-up
				// yet (Phase 2 task #5 lands SECRET_LEAK at result.json
				// + tool_audit + container_logs first); refusing the
				// upload here would surface as a generic "store
				// failed" without the operator-friendly class. Once
				// SECRET_LEAK ships we can promote this to a real
				// refusal.
				logEvent.Msg("artifacts: BLOCK ACTION NOT YET ENFORCED, degraded to redact")
				body = secrets.Redact(body, findings)
			default: // ActionDetect
				logEvent.Msg("artifacts: scanned — detect-only, content stored unchanged")
			}
		}
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), int64(len(body)), body, nil
}

// shouldScanBody decides whether an artifact's bytes should run
// through the secret-leak scanner. The decision is text-vs-binary
// based on the ACTUAL bytes, not just an enumerated extension subset
// — extensions are agent-chosen, so a narrow allowlist let common
// credential-bearing text formats (.log, .csv, .env, .conf, .pem,
// .sh, no-extension) bypass redaction and be served verbatim on
// download.
//
// Returns true when the MIME type is a known text type OR the body
// sniffs as text. Returns false only for content that is confirmed
// binary (a recognised binary MIME type, or bytes that fail the text
// sniff). That preserves the existing false-positive protection for
// compressed/binary blobs (random byte runs spuriously matching jwt /
// generic_kv patterns) while closing the extension-allowlist gap.
func shouldScanBody(mimeType string, body []byte) bool {
	if mimeIsScanCandidate(mimeType) {
		return true
	}
	if mimeIsConfirmedBinary(mimeType) {
		return false
	}
	// Unknown / agent-chosen type (notably application/octet-stream
	// from an unrecognised extension): fall back to sniffing the
	// bytes. Empty bodies have nothing to scan.
	if len(body) == 0 {
		return false
	}
	return bodyLooksTextual(body)
}

// bodyLooksTextual reports whether body is plausibly UTF-8 text. It
// rejects content with NUL bytes (a reliable binary marker) and
// content that http.DetectContentType classifies as binary, then
// requires the bytes to be valid UTF-8. This is the load-bearing
// backstop: it catches textual artifacts the extension allowlist
// misses without re-introducing false positives on binary blobs.
func bodyLooksTextual(body []byte) bool {
	// A NUL byte is a strong binary signal; UTF-8 text never contains
	// one.
	if bytes.IndexByte(body, 0x00) >= 0 {
		return false
	}
	// http.DetectContentType sniffs the leading bytes; "text/*" (and
	// some structured text types) are safe to scan. It returns a full
	// "type/subtype; charset=..." string, so prefix-match the type.
	sniffed := http.DetectContentType(body)
	if strings.HasPrefix(sniffed, "text/") {
		return true
	}
	// Fall back to a UTF-8 validity check for textual payloads that
	// the sniffer labels application/octet-stream (e.g. .pem, .env,
	// .conf, .log without an HTTP-recognised signature).
	return utf8.Valid(body)
}

// mimeIsConfirmedBinary reports whether the MIME type is a known
// binary class that should never be scanned regardless of byte
// content. application/octet-stream is deliberately NOT listed — it
// is the catch-all for unknown extensions, so it falls through to the
// body sniff in shouldScanBody.
func mimeIsConfirmedBinary(mimeType string) bool {
	if strings.HasPrefix(mimeType, "image/") {
		return true
	}
	switch mimeType {
	case "application/pdf", "application/zip", "application/gzip",
		"application/x-gzip", "application/x-tar", "application/wasm":
		return true
	}
	return false
}

// mimeIsScanCandidate reports whether the MIME type is text-shaped
// enough that secret-pattern regexes are meaningful. Binary types
// (images, PDFs, archives, application/octet-stream) skip the scan
// — random byte runs in compressed payloads can spuriously match
// patterns like jwt or generic_kv and corrupt the file.
func mimeIsScanCandidate(mimeType string) bool {
	switch mimeType {
	case "text/plain", "text/markdown", "text/html", "text/css",
		"text/csv", "text/xml", "application/json", "application/x-yaml",
		"application/javascript", "application/toml", "application/x-sh":
		return true
	}
	return false
}

// detectMimeType returns a MIME type based on file extension.
func detectMimeType(name string) string {
	ext := filepath.Ext(name)
	switch ext {
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/x-yaml"
	case ".md":
		return "text/markdown"
	case ".html":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".csv":
		return "text/csv"
	case ".xml":
		return "text/xml"
	case ".toml":
		return "application/toml"
	case ".sh", ".bash":
		return "application/x-sh"
	case ".log", ".ini", ".conf", ".cfg", ".env", ".pem", ".key", ".crt", ".text":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

// Errors
var (
	ErrNoRepository = &StoreError{Message: "no artifact repository configured"}
	ErrHashMismatch = &StoreError{Message: "artifact content hash mismatch"}
)

// StoreError represents a store error.
type StoreError struct {
	Message string
}

func (e *StoreError) Error() string {
	return e.Message
}
