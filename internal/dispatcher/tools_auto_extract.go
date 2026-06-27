// Dispatcher-side document-extraction trigger. Fires per
// snapshotted input artifact in createTask. See
// https://docs.vornik.io §8.1 (case
// "LLM-driven routing"). The email channel's parallel trigger
// covers attachments-on-arrival; this one covers every channel
// that routes uploads through create_task → artifactStore.StoreInput
// (Telegram, webchat, API, CLI).
package dispatcher

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// autoExtract invokes the configured AttachmentAutoExtractor with
// a per-call timeout and returns the JSON-shaped summary the
// caller folds into the task payload context. Returns nil for
// every "no extraction" outcome (no extractor wired, unknown MIME,
// extraction failed) so the caller doesn't pollute the payload
// with empty entries.
//
// The returned shape is map[string]any (not a typed struct)
// because it lands in args.Input["context"] which is itself
// untyped JSON — easier to serialise once at the boundary than to
// route a typed struct through three packages.
func (te *ToolExecutor) autoExtract(ctx context.Context, projectID string, art *persistence.Artifact) map[string]any {
	if te == nil || te.attachmentAutoExtractor == nil || art == nil {
		return nil
	}
	mime := ""
	if art.MimeType != nil {
		mime = *art.MimeType
	}
	req := AutoExtractRequest{
		ProjectID:   projectID,
		ArtifactID:  art.ID,
		Name:        art.Name,
		MimeType:    mime,
		StoragePath: art.StoragePath,
	}

	callCtx := ctx
	var cancel context.CancelFunc
	if te.attachmentAutoExtractTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, te.attachmentAutoExtractTimeout)
	}
	summary, err := te.attachmentAutoExtractor.AutoExtract(callCtx, req)
	if cancel != nil {
		cancel()
	}
	if err != nil {
		// Best-effort: log + continue. The task still creates with
		// the raw artifact path and the worker can choose how to
		// proceed (file_read for small docs, give up for huge ones).
		te.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("artifact_id", art.ID).
			Str("mime_type", mime).
			Str("name", art.Name).
			Msg("dispatcher: auto-extract failed; task created without extraction summary")
		return nil
	}
	if summary == nil {
		// Implementation signalled "no extractor for this MIME" —
		// not an error. The worker falls back to whatever tooling
		// it has for this format. Silently skip.
		return nil
	}
	te.logger.Info().
		Str("project_id", projectID).
		Str("artifact_id", art.ID).
		Str("extracted_document_id", summary.ExtractedDocumentID).
		Int("section_count", summary.SectionCount).
		Int("chunks_ingested", summary.ChunksIngested).
		Msg("dispatcher: input artifact auto-extracted into project memory")
	return map[string]any{
		"artifact_id":           art.ID,
		"extracted_document_id": summary.ExtractedDocumentID,
		"title":                 summary.Title,
		"author":                summary.Author,
		"section_count":         summary.SectionCount,
		"chunks_ingested":       summary.ChunksIngested,
	}
}

// Keep the time import used even when callers don't reach it
// directly (the timeout field's type lives in tools.go via this
// package's import set).
var _ = time.Second
