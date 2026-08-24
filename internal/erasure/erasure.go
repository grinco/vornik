// Package erasure performs the GDPR Article 17 cascade for a deleted source
// artifact: the extraction rows derived from it, the memory chunks derived
// from those, and the extraction's on-disk storage directory.
//
// WHY THIS EXISTS. Verified against the live schema on 2026-07-29, the cascade
// was absent at every link:
//
//   - `extracted_documents` has no foreign key on `source_artifact_id`, so
//     deleting a source artifact orphans its extraction row.
//   - The extraction's storage directory (`sections/`, and since the video
//     extractor `files/` with sampled keyframes) is filesystem state that no
//     database constraint can reach, and nothing garbage-collects it.
//   - `project_memory_chunks.artifact_id` is ON DELETE SET NULL, so the
//     derived embedding and text SURVIVE a source deletion and merely lose the
//     pointer back to where they came from. That is the sharpest edge: an
//     erasure request satisfied by deleting the artifact leaves the data
//     subject's derived data in the vector store AND destroys the provenance
//     that would let anyone find it later.
//
// WHY NOT A DATABASE CASCADE. Adding ON DELETE CASCADE would have been a
// smaller diff and a worse answer, for two reasons:
//
//  1. It cannot delete the storage directory. A row-only cascade would leave
//     OCR text and video keyframes on disk while reporting erasure — the exact
//     shape of failure being fixed.
//  2. It would fire on EVERY artifact deletion, including retention pruning of
//     ordinary task artifacts, silently destroying memory chunks an operator
//     relies on. Turning SET NULL into CASCADE is a data-loss change to a live
//     store, not a bug fix.
//
// So erasure is an explicit, previewable operation instead: existing deletion
// semantics are untouched, and this service is the one path that performs a
// complete cascade. Plan() is read-only and shows the blast radius before
// anything is destroyed, because erasure is irreversible and "run it and find
// out" is not an acceptable interface for it.
//
// see LLD § https://docs.vornik.io §4.4
package erasure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Document is one extraction derived from a source artifact.
type Document struct {
	ID           string
	StoragePath  string
	SectionCount int
}

// DocumentStore is the narrow slice of the extracted-document repository this
// service needs.
type DocumentStore interface {
	ListBySourceArtifact(ctx context.Context, artifactID string) ([]Document, error)
	DeleteExtractedDocument(ctx context.Context, id string) error
}

// ChunkStore is the narrow slice of the memory-chunk repository this service
// needs. Counts drive the preview; deletes return how many rows went.
type ChunkStore interface {
	CountByExtractedDocument(ctx context.Context, extractedDocumentID string) (int, error)
	CountByArtifact(ctx context.Context, artifactID string) (int, error)
	DeleteByExtractedDocument(ctx context.Context, extractedDocumentID string) (int, error)
	DeleteByArtifact(ctx context.Context, artifactID string) (int, error)
}

// Service performs the cascade. ArtifactRoot is mandatory: every storage path
// must resolve beneath it before any recursive delete runs.
type Service struct {
	Docs   DocumentStore
	Chunks ChunkStore

	// Artifacts is required only by EraseIncludingArtifact, which removes the
	// artifact's own stored file and row on top of the derived cascade. Erase()
	// does not use it, so retention callers may leave it nil.
	Artifacts ArtifactStore

	// ArtifactRoot bounds every filesystem deletion this service performs.
	// StoragePath values come out of the database and are handed to a
	// recursive remove, so an unbounded service is one bad row away from
	// deleting the wrong tree.
	ArtifactRoot string

	// Derived removes what the erased chunks DERIVED: knowledge-graph entities
	// and edges, and the pre-ingest copies in project_memory_quarantine. Nil for
	// retention callers, which prune rather than erase and must not hard-delete
	// graph rows — see RequestID.
	Derived DerivedStore

	// RequestID is the Art 17 request authorising the hard deletes. Required
	// whenever Derived is set: these are the only legitimate graph deletions in
	// the system, so the authorisation travels as a value rather than as a
	// convention.
	RequestID string

	// removeAll is os.RemoveAll unless a test injects a failure.
	removeAll func(string) error
}

// DocumentPlan is what would be erased for one extraction.
type DocumentPlan struct {
	Document
	ChunkCount int
}

// Plan is the read-only preview of an erasure.
type Plan struct {
	ArtifactID string
	Documents  []DocumentPlan
	// DirectChunkCount is chunks linked straight to the artifact
	// (project_memory_chunks.artifact_id), independent of any extraction.
	DirectChunkCount int
}

// TotalChunks is every chunk the erasure would remove.
func (p *Plan) TotalChunks() int {
	total := p.DirectChunkCount
	for _, d := range p.Documents {
		total += d.ChunkCount
	}
	return total
}

// Summary renders the blast radius for an operator to read before confirming.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Erasing artifact %s would remove:\n", p.ArtifactID)
	fmt.Fprintf(&b, "  - %d extracted document(s)\n", len(p.Documents))
	for _, d := range p.Documents {
		fmt.Fprintf(&b, "      %s  (%d section(s), %d chunk(s))\n        storage: %s\n",
			d.ID, d.SectionCount, d.ChunkCount, d.StoragePath)
	}
	fmt.Fprintf(&b, "  - %d memory chunk(s) total (%d derived from extractions, %d linked directly)\n",
		p.TotalChunks(), p.TotalChunks()-p.DirectChunkCount, p.DirectChunkCount)
	b.WriteString("\nThis is irreversible. The storage directories hold extracted text and, for\nvideo, sampled keyframes.\n")
	return b.String()
}

// Result reports what an erasure actually did.
type Result struct {
	ArtifactID         string
	DocumentsDeleted   int
	ChunksDeleted      int
	DirectoriesRemoved int

	// BlobsRemoved counts the artifact's OWN stored files removed. Only
	// EraseIncludingArtifact sets it; Erase leaves the upload in place.
	BlobsRemoved int
	// ArtifactRowDeleted records whether the artifacts row itself went. False
	// after a plain Erase, which deliberately retains it.
	ArtifactRowDeleted bool

	// Derived counts rows removed BEYOND the chunks — graph entities, graph
	// edges, and quarantined pre-ingest copies. Reported because once they are
	// gone the report is the only evidence the erasure covered them, and an
	// erasure that silently omitted derived rows is how 3,795 entities
	// accumulated in production unnoticed.
	Derived DerivedCounts
}

// Plan collects everything an erasure would touch, without touching it.
func (s *Service) Plan(ctx context.Context, artifactID string) (*Plan, error) {
	if err := s.check(artifactID); err != nil {
		return nil, err
	}
	docs, err := s.Docs.ListBySourceArtifact(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("erasure: list extractions for %s: %w", artifactID, err)
	}
	plan := &Plan{ArtifactID: artifactID}
	for _, d := range docs {
		n, err := s.Chunks.CountByExtractedDocument(ctx, d.ID)
		if err != nil {
			return nil, fmt.Errorf("erasure: count chunks for %s: %w", d.ID, err)
		}
		plan.Documents = append(plan.Documents, DocumentPlan{Document: d, ChunkCount: n})
	}
	direct, err := s.Chunks.CountByArtifact(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("erasure: count chunks for artifact %s: %w", artifactID, err)
	}
	plan.DirectChunkCount = direct
	return plan, nil
}

// Erase performs the cascade.
//
// Order is deliberate and load-bearing: **filesystem first, then rows.** The
// bytes on disk are the personal data; the rows are the index that lets anyone
// find them. Deleting the index first and then failing on the files would
// leave the data present and unfindable, which is strictly worse than failing
// with everything still in place. So a path that cannot be validated or
// removed aborts the whole operation before a single row is touched.
//
// Idempotent: a directory that is already gone is not an error, so an operator
// can safely retry after a partial failure.
func (s *Service) Erase(ctx context.Context, artifactID string) (*Result, error) {
	plan, err := s.Plan(ctx, artifactID)
	if err != nil {
		return nil, err
	}

	// Validate every path BEFORE removing anything, so a bad row later in the
	// list cannot leave us half-erased.
	targets := make([]string, 0, len(plan.Documents))
	for _, d := range plan.Documents {
		resolved, err := s.safeTarget(d.StoragePath)
		if err != nil {
			return nil, fmt.Errorf("erasure: refusing to erase %s: %w", d.ID, err)
		}
		targets = append(targets, resolved)
	}

	remove := s.removeAll
	if remove == nil {
		remove = os.RemoveAll
	}
	res := &Result{ArtifactID: artifactID}
	for i, target := range targets {
		if _, statErr := os.Stat(target); os.IsNotExist(statErr) {
			continue // already gone — idempotent
		}
		if err := remove(target); err != nil {
			return nil, fmt.Errorf("erasure: remove storage for %s (%s): %w — NOTHING has been deleted from the database, so the erasure can be retried safely", plan.Documents[i].ID, target, err)
		}
		res.DirectoriesRemoved++
	}

	// Capture what the chunks derived BEFORE any of them goes. Deleting a chunk
	// cascades entity_mentions, so afterwards there is nothing left to compute
	// this from — the same collect-first shape DeleteByExtractedDocument
	// already uses for the embedding cache.
	docIDs := make([]string, 0, len(plan.Documents))
	for _, d := range plan.Documents {
		docIDs = append(docIDs, d.ID)
	}
	var captured Derivation
	if s.Derived != nil {
		captured, err = s.Derived.CaptureDerivation(ctx, artifactID, docIDs)
		if err != nil {
			return nil, fmt.Errorf("erasure: capture derived data for %s: %w — NOTHING has been deleted from the database, so the erasure can be retried safely", artifactID, err)
		}
	}

	// Rows only after the bytes are gone.
	for _, d := range plan.Documents {
		n, err := s.Chunks.DeleteByExtractedDocument(ctx, d.ID)
		if err != nil {
			return nil, fmt.Errorf("erasure: delete chunks for %s: %w — storage directories were removed; re-run to finish", d.ID, err)
		}
		res.ChunksDeleted += n
		if err := s.Docs.DeleteExtractedDocument(ctx, d.ID); err != nil {
			return nil, fmt.Errorf("erasure: delete extraction row %s: %w — storage and chunks were removed; re-run to finish", d.ID, err)
		}
		res.DocumentsDeleted++
	}

	n, err := s.Chunks.DeleteByArtifact(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("erasure: delete chunks linked to artifact %s: %w", artifactID, err)
	}
	res.ChunksDeleted += n

	// Derived data last: the entity sweep's keep-or-delete decision is made
	// against the state AFTER the chunks are gone, re-checked inside the store's
	// own transaction so a concurrently-ingested mention is seen.
	derived, err := eraseDerived(ctx, s.Derived, s.RequestID, artifactID, captured, res.ChunksDeleted)
	if err != nil {
		return nil, err
	}
	res.Derived = derived
	return res, nil
}

func (s *Service) check(artifactID string) error {
	if strings.TrimSpace(artifactID) == "" {
		return fmt.Errorf("erasure: artifact id is required")
	}
	if strings.TrimSpace(s.ArtifactRoot) == "" {
		// Refuse rather than default: an empty root would make "" resolve to
		// the process working directory and turn a containment check into a
		// rubber stamp.
		return fmt.Errorf("erasure: ArtifactRoot is not configured; refusing to delete anything")
	}
	// Refuse the filesystem root explicitly.
	//
	// This is defence in depth rather than a live hole: with root "/" the
	// containment test in safeTarget compares against "//" and therefore
	// rejects every path, so the behaviour today already fails CLOSED
	// (verified by TestService_RefusesFilesystemRoot). But that safety is an
	// accident of string concatenation, not an expressed intention — a future
	// refactor to a cleaner containment helper could normalise "//" to "/" and
	// silently turn a fail-closed into a fail-open on the one input where the
	// blast radius is the entire disk. Saying no here means the property
	// survives that refactor.
	if filepath.Clean(strings.TrimSpace(s.ArtifactRoot)) == string(os.PathSeparator) {
		return fmt.Errorf("erasure: ArtifactRoot is the filesystem root; refusing — a containment boundary of / bounds nothing")
	}
	return nil
}

// safeTarget resolves a database-supplied storage path and refuses anything
// that is not a proper descendant of ArtifactRoot.
//
// Symlinks are resolved before the containment test so a link inside the root
// cannot point a recursive delete outside it. The root itself is refused: a row
// whose storage_path had been truncated to the root would otherwise erase every
// project's extractions in one call.
func (s *Service) safeTarget(storagePath string) (string, error) {
	if strings.TrimSpace(storagePath) == "" {
		return "", fmt.Errorf("storage path is empty")
	}
	root, err := filepath.Abs(s.ArtifactRoot)
	if err != nil {
		return "", fmt.Errorf("resolve artifact root: %w", err)
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	abs, err := filepath.Abs(storagePath)
	if err != nil {
		return "", fmt.Errorf("resolve storage path: %w", err)
	}
	// EvalSymlinks fails on a non-existent path, which is fine here: an
	// already-erased directory still has to pass containment, so fall back to
	// the lexically-cleaned absolute path in that case.
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	if abs == root {
		return "", fmt.Errorf("storage path is the artifact root itself (%s)", root)
	}
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("storage path %s is outside the artifact root %s", abs, root)
	}
	return abs, nil
}

// Compile-time guard: the postgres adapters must keep satisfying these
// contracts. Declared here (rather than in the postgres package) so a change
// to either interface breaks at the definition site.
//
// Wired in internal/service; see the erase CLI command for the operator entry
// point.
var (
	_ = func(d DocumentStore, c ChunkStore) *Service {
		return &Service{Docs: d, Chunks: c}
	}
)
