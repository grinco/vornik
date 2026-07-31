package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryscope"
	"vornik.io/vornik/internal/persistence"
)

// repoScopeFromPayload (B-18) MOVED to internal/memoryscope, which this package
// now imports. It had been a byte-identical copy of
// executor.extractRepoScopeFromPayload — its own comment said so, justifying the
// duplication as keeping this package's dependencies narrow. When the
// project-default fallback needed the same rule, three copies of "where does this
// chunk land" was one too many, and memoryscope is a stdlib-only leaf, so the
// import costs exactly nothing in dependency surface.

// extractedDocGetter is the narrow lookup surface the index
// handler needs. Defined here (not pulled from the full
// persistence.ExtractedDocumentRepository) so tests can stub
// with one method.
type extractedDocGetter interface {
	Get(ctx context.Context, id string) (*persistence.ExtractedDocument, error)
}

// extractedSectionsIngester mirrors memory.Indexer's
// IngestExtractedSections + PatchScopeByArtifact so the handler
// is testable without constructing a full Indexer + Repository
// chain. PatchScopeByArtifact is the B-18 follow-on to B-7:
// chunks land scoped per the task payload's repo_scope so
// strict-scope recalls can filter them.
type extractedSectionsIngester interface {
	IngestExtractedSections(
		ctx context.Context,
		projectID, taskID, sourceArtifactID, extractedDocumentID string,
		sections []memory.ExtractedSection,
	) (int, error)

	// PatchScopeByArtifact stamps repo_scope onto every chunk
	// tied to artifactID. Called by rag.index after the section
	// content has been chunked + persisted. Empty repoScope is a
	// no-op contract the implementation enforces.
	PatchScopeByArtifact(ctx context.Context, projectID, artifactID, repoScope string) error
}

// IndexHandler consumes the rag.extract step's output and writes
// every extracted section's content into project memory via the
// Indexer. Pure-Go, no LLM call.
type IndexHandler struct {
	docRepo extractedDocGetter
	indexer extractedSectionsIngester
	// projectScope resolves a project's default repo_scope. Optional: nil means
	// payload-only resolution, which is the pre-2026-07-30 behaviour.
	projectScope func(projectID string) string
}

// NewIndexHandler constructs the "rag.index" handler. Nil deps
// are tolerated; Execute returns a clear "dependencies missing"
// error in that case so the workflow's on_fail branch can route
// it operator-visible.
func NewIndexHandler(docRepo extractedDocGetter, indexer extractedSectionsIngester) *IndexHandler {
	return &IndexHandler{docRepo: docRepo, indexer: indexer}
}

// WithProjectScope supplies the per-project default repo_scope lookup, used only
// when the task payload carries no scope of its own.
//
// An OPTION rather than a constructor parameter, deliberately: the default is
// enrichment, not a dependency — without it the handler behaves exactly as it did
// before the default existed (payload-only, else NULL). Threading a fifth
// argument through twenty existing call sites to express "optional" would have
// been churn that says the opposite.
func (h *IndexHandler) WithProjectScope(fn func(projectID string) string) *IndexHandler {
	if h != nil {
		h.projectScope = fn
	}
	return h
}

// projectScopeFor is the nil-safe read of the optional resolver.
func (h *IndexHandler) projectScopeFor(projectID string) string {
	if h == nil || h.projectScope == nil || projectID == "" {
		return ""
	}
	return h.projectScope(projectID)
}

// Name implements executor.SystemHandler.
func (h *IndexHandler) Name() string { return "rag.index" }

// indexResult is the handler's emitted summary. Useful for the
// operator's task-detail UI + the optional `summarize` step in
// the document-ingest workflow.
type indexResult struct {
	Chunks    int `json:"chunks"`
	Documents int `json:"documents"`
}

// Execute implements executor.SystemHandler. Walks every
// extracted_document_id from the previous step's result, reads
// the on-disk outline + sections, and hands the section content
// to the indexer.
func (h *IndexHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	if h == nil || h.docRepo == nil || h.indexer == nil {
		return executor.SystemStepResult{}, errors.New("rag.index: handler is missing required dependencies (doc repo / indexer)")
	}
	if in.Task == nil {
		return executor.SystemStepResult{}, errors.New("rag.index: task is nil")
	}

	var prev extractResultEnvelope
	if len(in.PrevResult) > 0 {
		if err := json.Unmarshal(in.PrevResult, &prev); err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("rag.index: parse previous result: %w", err)
		}
	}
	if len(prev.Extracted) == 0 {
		return executor.SystemStepResult{}, errors.New("rag.index: previous step left no extracted documents (rag.extract must run before rag.index)")
	}

	totalChunks := 0
	for _, entry := range prev.Extracted {
		doc, err := h.docRepo.Get(ctx, entry.ExtractedDocumentID)
		if err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("rag.index: load extracted doc %s: %w", entry.ExtractedDocumentID, err)
		}
		if doc.StoragePath == "" {
			return executor.SystemStepResult{}, fmt.Errorf("rag.index: extracted doc %s has empty storage_path", doc.ID)
		}

		var outline []extractor.OutlineEntry
		if len(doc.OutlineBlob) > 0 {
			if err := json.Unmarshal(doc.OutlineBlob, &outline); err != nil {
				return executor.SystemStepResult{}, fmt.Errorf("rag.index: parse outline for %s: %w", doc.ID, err)
			}
		}
		if len(outline) == 0 {
			// An empty outline means the extractor produced no
			// sections. Skip silently — happens on truly empty
			// inputs (zero-byte text files, etc.) and shouldn't
			// abort a multi-document batch.
			continue
		}

		sections := make([]memory.ExtractedSection, 0, len(outline))
		for _, oe := range outline {
			path := filepath.Join(doc.StoragePath, "sections", oe.SectionID+".md")
			content, err := os.ReadFile(path) // #nosec G304 — path composed from extractor-controlled IDs
			if err != nil {
				return executor.SystemStepResult{}, fmt.Errorf("rag.index: read section %s/%s: %w", doc.ID, oe.SectionID, err)
			}
			sections = append(sections, memory.ExtractedSection{
				SectionID:  oe.SectionID,
				SourceName: oe.Title,
				Content:    string(content),
			})
		}

		n, err := h.indexer.IngestExtractedSections(
			ctx, in.Task.ProjectID, in.Task.ID,
			doc.SourceArtifactID, doc.ID, sections,
		)
		if err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("rag.index: ingest %s: %w", doc.ID, err)
		}
		totalChunks += n
	}

	// B-18: stamp repo_scope from the task payload onto every
	// chunk tied to the source artifacts. Without this, chunks
	// from document-ingest land NULL-scoped and strict-scope
	// recalls miss them. Empty scope is a no-op (preserves any
	// pre-existing tags on the artifact's chunks). Done once
	// per source-artifact-id so the PatchScopeByArtifact UPDATE
	// runs at most N times for N sources, not chunk-by-chunk.
	if scope := memoryscope.Resolve(in.Task.Payload, h.projectScopeFor(in.Task.ProjectID)); scope != "" {
		seen := make(map[string]struct{}, len(prev.Extracted))
		for _, entry := range prev.Extracted {
			doc, err := h.docRepo.Get(ctx, entry.ExtractedDocumentID)
			if err != nil {
				// Already loaded successfully above; a transient
				// failure here is best-effort — we've already
				// landed the chunks, the scope tag is the only
				// thing missing and a retry-from-step covers it.
				continue
			}
			if _, dup := seen[doc.SourceArtifactID]; dup {
				continue
			}
			seen[doc.SourceArtifactID] = struct{}{}
			if err := h.indexer.PatchScopeByArtifact(ctx, in.Task.ProjectID, doc.SourceArtifactID, scope); err != nil {
				return executor.SystemStepResult{}, fmt.Errorf("rag.index: patch scope for artifact %s: %w", doc.SourceArtifactID, err)
			}
		}
	}

	resultBytes, err := json.Marshal(indexResult{
		Chunks:    totalChunks,
		Documents: len(prev.Extracted),
	})
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("rag.index: marshal result: %w", err)
	}
	return executor.SystemStepResult{Result: resultBytes}, nil
}
