// Package rag implements the deterministic system-step handlers
// powering the document-ingest workflow (B-7). Two handlers:
//
//   - "rag.extract" — resolves each task input artifact's MIME
//     type, dispatches to the matching extractor (epub / pdf /
//     html / textfile / audio / image), and persists the
//     extracted_documents row.
//   - "rag.index" — reads the extracted sections from disk and
//     chunks them into memory via Indexer.IngestExtractedSections.
//
// Together they turn "operator uploads a file" into "operator can
// recall() the content" with zero LLM tokens spent.
package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
)

// artifactGetter is the narrow Artifact-lookup surface the extract
// handler needs. Defined here so the handler doesn't pull the full
// persistence.ArtifactRepository interface — keeps test doubles
// trivial.
type artifactGetter interface {
	Get(ctx context.Context, id string) (*persistence.Artifact, error)
}

// ExtractHandler runs the registered extractor against every
// input artifact attached to a task. Pure-Go (no LLM call). The
// handler emits a {"extracted":[{artifact_id, extracted_document_id}, ...]}
// result so the downstream rag.index step can find the produced
// extracted_documents rows.
type ExtractHandler struct {
	registry     *extractor.Registry
	runner       *extractor.Runner
	artifactRepo artifactGetter
}

// NewExtractHandler returns the "rag.extract" handler. Nil-safe
// for missing dependencies — execution returns a clear error
// rather than panicking, so a daemon that wires the workflow but
// not the extractor stack still surfaces actionable failures.
func NewExtractHandler(reg *extractor.Registry, runner *extractor.Runner, artifactRepo artifactGetter) *ExtractHandler {
	return &ExtractHandler{registry: reg, runner: runner, artifactRepo: artifactRepo}
}

// Name implements executor.SystemHandler.
func (h *ExtractHandler) Name() string { return "rag.extract" }

// extractedEntry is one row of the handler's result envelope.
type extractedEntry struct {
	ArtifactID          string `json:"artifact_id"`
	ExtractedDocumentID string `json:"extracted_document_id"`
	SectionCount        int    `json:"section_count"`
}

// extractResultEnvelope is the JSON shape rag.index expects to
// receive on its PrevResult.
type extractResultEnvelope struct {
	Extracted []extractedEntry `json:"extracted"`
}

// taskPayloadInputs is the narrow shape we read out of the task
// payload. Matches what processInputArtifactsWithOpts writes when
// a companion delegate / REST POST stamps inputArtifactIDs.
type taskPayloadInputs struct {
	Context struct {
		InputArtifactIDs []string `json:"inputArtifactIDs"`
	} `json:"context"`
}

// Execute implements executor.SystemHandler.
func (h *ExtractHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	if h == nil || h.registry == nil || h.runner == nil || h.artifactRepo == nil {
		return executor.SystemStepResult{}, errors.New("rag.extract: handler is missing required dependencies (registry/runner/artifact repo)")
	}
	if in.Task == nil {
		return executor.SystemStepResult{}, errors.New("rag.extract: task is nil")
	}
	var payload taskPayloadInputs
	if len(in.Task.Payload) > 0 {
		// JSON-decode is best-effort. A task with a non-JSON-shaped
		// payload (legacy tasks, edge cases) falls through to the
		// "no input artifacts" check below.
		_ = json.Unmarshal(in.Task.Payload, &payload)
	}
	ids := payload.Context.InputArtifactIDs
	if len(ids) == 0 {
		return executor.SystemStepResult{}, errors.New("rag.extract: no input artifacts on task — workflow expects context.inputArtifactIDs[] to be populated by the upstream channel (companion delegate / REST POST)")
	}

	out := extractResultEnvelope{Extracted: make([]extractedEntry, 0, len(ids))}
	for _, artifactID := range ids {
		art, err := h.artifactRepo.Get(ctx, artifactID)
		if err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("rag.extract: load artifact %s: %w", artifactID, err)
		}
		mime := ""
		if art.MimeType != nil {
			mime = *art.MimeType
		}
		if mime == "" {
			// Fall back to the filename-extension table — many
			// uploads land with a nil MimeType pointer
			// (especially the early companion-direct paths).
			mime = extractor.MimeFromFilename(art.Name)
		}
		ext, err := h.registry.For(mime)
		if err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("rag.extract: no extractor for %q on artifact %s (%s)", mime, artifactID, art.Name)
		}
		row, err := h.runner.Run(ctx, in.Task.ProjectID, artifactID, ext, extractor.Source{
			FilePath:     art.StoragePath,
			MimeType:     mime,
			OriginalName: art.Name,
		})
		if err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("rag.extract: extract %s: %w", artifactID, err)
		}
		out.Extracted = append(out.Extracted, extractedEntry{
			ArtifactID:          artifactID,
			ExtractedDocumentID: row.ID,
			SectionCount:        row.SectionCount,
		})
	}

	resultBytes, err := json.Marshal(out)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("rag.extract: marshal result: %w", err)
	}
	return executor.SystemStepResult{Result: resultBytes}, nil
}
