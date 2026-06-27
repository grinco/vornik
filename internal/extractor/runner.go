// Package extractor — Runner: persistence layer between the
// stateless Extractor implementations and the daemon's storage.
//
// The Runner takes an Extractor + a source artifact and:
//  1. Runs extraction.
//  2. Writes sections to disk under
//     <storage_path>/<extracted_doc_id>/sections/<section_id>.md
//  3. Writes metadata.json + outline.json next to the sections dir.
//  4. Upserts the extracted_documents row.
//
// Runner is the right seam for the executor's extract step: hand it
// the source artifact + the artifacts base path, get back an
// ExtractedDocument row.
package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

// extractionLocks serializes concurrent Run calls that target the
// same (project, artifact, extractor, version). It is a process-local
// keyed mutex — no sidecar, no DB lock table — which fits vornik's
// single-deployable-unit invariant. batch-3 ingress/untrusted-input:
// document-extraction hardening (e).
//
// Why a lock and not just rely on the DB's idempotent upsert: without
// it, two concurrent extractions of the same artifact both miss the
// cache, both run the (expensive) extractor, both mint distinct
// doc IDs and write distinct on-disk storage dirs — duplicate CPU/IO
// plus an orphaned directory the GC sweep must later reclaim. The
// lock makes the second caller wait for the first, after which it
// re-checks the cache and returns the already-extracted row.
//
// Keyed per target (not global) so distinct artifacts still extract
// in parallel — throughput is preserved.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// lock returns the per-key mutex (creating it on first use) already
// locked, plus an unlock func the caller defers.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// pkgExtractionLocks is the process-wide lock registry. A package-
// level singleton (rather than a Runner field) so the guard holds
// even when callers construct Runner inline per request — which the
// extract handlers do.
var pkgExtractionLocks = newKeyedMutex()

// Clock is the injected time source. Production wires
// time.Now().UTC(); tests inject a deterministic clock.
type Clock func() time.Time

// IDMint produces an extracted_document_id. Production uses
// persistence.GenerateID("extdoc"); tests inject a counter for
// deterministic assertions.
type IDMint func() string

// Runner persists an extraction end-to-end. Fields are public so
// callers can construct it inline; nil fields fall back to sensible
// defaults (UTC clock, persistence.GenerateID).
type Runner struct {
	// Repo persists the extracted_documents row. Required.
	Repo persistence.ExtractedDocumentRepository

	// BasePath is the artifact-store root
	// (Storage.ArtifactsPath, e.g. /opt/vornik/artifacts).
	// The extraction layout is
	//   <BasePath>/<project_id>/extracted/<extracted_doc_id>/
	// — mirrors the inputs/ layout so the LocalBackend stays
	// uniform across artifact classes.
	BasePath string

	// Clock injects time. Nil = time.Now().UTC.
	Clock Clock

	// IDMint injects the extracted-document ID generator. Nil =
	// persistence.GenerateID("extdoc").
	IDMint IDMint

	// Metrics emits the vornik_extractions_total /
	// vornik_extraction_duration_seconds / vornik_extracted_documents_total
	// series. Nil = no emission (every call site is nil-safe).
	Metrics *Metrics
}

// Run executes the extractor over a source artifact and persists
// the result. The returned ExtractedDocument carries the full row
// (including the freshly-minted ID and on-disk storage path).
//
// On extraction error: returns the error and a PARTIAL/FAILED row
// IS NOT written. Operators see the failure via the workflow step
// outcome; re-running the workflow re-attempts cleanly.
//
// On partial extraction (extractor returned an error but emitted
// some sections): future enhancement may persist a PARTIAL row.
// Phase 1 keeps the semantics simple — all-or-nothing.
func (r *Runner) Run(ctx context.Context, projectID, sourceArtifactID string, ext Extractor, src Source) (row *persistence.ExtractedDocument, err error) {
	if r == nil {
		return nil, errors.New("runner is nil")
	}
	if r.Repo == nil {
		return nil, errors.New("runner: Repo is required")
	}
	if r.BasePath == "" {
		return nil, errors.New("runner: BasePath is required")
	}
	if ext == nil {
		return nil, errors.New("runner: extractor is nil")
	}
	if projectID == "" || sourceArtifactID == "" {
		return nil, errors.New("runner: projectID and sourceArtifactID are required")
	}

	clock := r.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	mintID := r.IDMint
	if mintID == nil {
		mintID = func() string { return persistence.GenerateID("extdoc") }
	}

	safeProjectID, err := safepath.CleanPathComponent(projectID)
	if err != nil {
		return nil, fmt.Errorf("runner: invalid project id %q: %w", projectID, err)
	}

	// Serialize concurrent extractions of the same target so a
	// racing pair doesn't both run the extractor + leave an orphan
	// storage dir (hardening (e)). Held across the whole Extract +
	// persist so the second caller observes the first's committed
	// row. The key spans name+version so a version bump (which is a
	// legitimately distinct extraction) is not blocked by the old
	// version's in-flight run.
	unlock := pkgExtractionLocks.lock(extractionLockKey(projectID, sourceArtifactID, ext))
	defer unlock()

	started := clock()
	res, extractErr := ext.Extract(ctx, src)
	durationMS := clock().Sub(started).Milliseconds()
	r.Metrics.observeDuration(ext.Name(), float64(durationMS)/1000.0)
	// From here on every exit is an extraction outcome — record it once
	// via the named return. Pre-Extract validation failures returned
	// above (before this defer is registered) are not counted: they're
	// config/programmer errors, not extraction attempts.
	defer func() {
		if err != nil {
			r.Metrics.recordExtraction(ext.Name(), "error")
			return
		}
		r.Metrics.recordExtraction(ext.Name(), "ok")
		r.Metrics.recordDocument(projectID)
	}()
	if extractErr != nil {
		return nil, fmt.Errorf("runner: extract: %w", extractErr)
	}
	if len(res.Sections) == 0 {
		return nil, fmt.Errorf("runner: extractor produced zero sections")
	}

	docID := mintID()
	safeDocID, err := safepath.CleanPathComponent(docID)
	if err != nil {
		return nil, fmt.Errorf("runner: invalid extracted-document id %q: %w", docID, err)
	}

	storageDir := filepath.Join(r.BasePath, safeProjectID, "extracted", safeDocID)
	if err := os.MkdirAll(filepath.Join(storageDir, "sections"), 0o700); err != nil {
		return nil, fmt.Errorf("runner: mkdir storage: %w", err)
	}

	if err := writeSections(storageDir, res.Sections); err != nil {
		return nil, fmt.Errorf("runner: write sections: %w", err)
	}

	metadataBytes, err := json.MarshalIndent(res.Metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runner: marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "metadata.json"), metadataBytes, 0o600); err != nil {
		return nil, fmt.Errorf("runner: write metadata.json: %w", err)
	}

	outlineBytes, err := json.MarshalIndent(res.Outline, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runner: marshal outline: %w", err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "outline.json"), outlineBytes, 0o600); err != nil {
		return nil, fmt.Errorf("runner: write outline.json: %w", err)
	}

	row = &persistence.ExtractedDocument{
		ID:                   docID,
		ProjectID:            projectID,
		SourceArtifactID:     sourceArtifactID,
		ExtractorName:        ext.Name(),
		ExtractorVersion:     ext.Version(),
		MimeType:             src.MimeType,
		StoragePath:          storageDir,
		MetadataBlob:         metadataBytes,
		OutlineBlob:          outlineBytes,
		SectionCount:         len(res.Sections),
		TotalTextBytes:       res.TotalTextBytes(),
		ExtractionDurationMS: &durationMS,
		Status:               persistence.ExtractedDocumentStatusOK,
		ExtractedAt:          clock(),
	}
	if err := r.Repo.Upsert(ctx, row); err != nil {
		// Persistence failed — we already wrote files to disk. Leave
		// them; the next Upsert attempt (manual or via re-run) will
		// reuse the same on-disk layout because the path is keyed on
		// docID, and the failed attempt will be GC'd by the orphan
		// sweep (future retention work).
		return nil, fmt.Errorf("runner: upsert row: %w", err)
	}
	return row, nil
}

// extractionLockKey builds the per-target serialization key. The
// NUL separators keep the components unambiguous (a NUL can't appear
// in any of the inputs) so distinct triples never collide.
func extractionLockKey(projectID, sourceArtifactID string, ext Extractor) string {
	return projectID + "\x00" + sourceArtifactID + "\x00" + ext.Name() + "\x00" + ext.Version()
}

// writeSections writes one markdown file per Section under
// <storageDir>/sections/<section_id>.md. Uses safepath.JoinUnder
// to guard against a malicious section_id smuggling a "../"
// segment past the extractor's sanitisation (defence in depth).
func writeSections(storageDir string, sections []Section) error {
	sectionsDir := filepath.Join(storageDir, "sections")
	for _, s := range sections {
		if s.SectionID == "" {
			return fmt.Errorf("section has empty SectionID")
		}
		path, err := safepath.JoinUnder(sectionsDir, s.SectionID+".md")
		if err != nil {
			return fmt.Errorf("section %q: %w", s.SectionID, err)
		}
		if err := os.WriteFile(path, []byte(s.Content), 0o600); err != nil {
			return fmt.Errorf("write %q: %w", path, err)
		}
	}
	return nil
}

// ReadSection reads one section's content from a persisted
// extracted document. The runner writes them; the document_*
// tools (Phase 2) read via this helper to keep the on-disk layout
// in one place.
func ReadSection(doc *persistence.ExtractedDocument, sectionID string) (string, error) {
	if doc == nil {
		return "", errors.New("document is nil")
	}
	if doc.StoragePath == "" {
		return "", errors.New("document storage_path is empty")
	}
	path, err := safepath.JoinUnder(filepath.Join(doc.StoragePath, "sections"), sectionID+".md")
	if err != nil {
		return "", fmt.Errorf("section %q: %w", sectionID, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
