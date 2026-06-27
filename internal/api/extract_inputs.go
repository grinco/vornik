// REST CreateTask → input-artifacts auto-extraction pipeline.
// Mirrors the dispatcher's per-snapshot trigger
// (dispatcher.ToolExecutor.autoExtract) but at the api-layer
// boundary: callers submit base64-encoded InputArtifacts inline
// via POST /tasks; the handler snapshots each into the artifact
// store, fires extraction, and folds the resulting inputFiles /
// inputArtifactIDs / inputExtractions into the task's context
// JSON before delegating to taskcreate.Creator.
//
// Without this layer the REST path's CreateTaskRequest.InputArtifacts
// field was dead — declared but never read. With it, scripted
// callers (vornikctl, CI runners, third-party integrations) get the
// same memory-ready trailer the dispatcher's chat-driven path
// produces.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

// inputArtifactResult is the per-artifact outcome the REST handler
// folds into the outgoing task payload. Bundling these in one
// struct keeps the caller-side merge code straightforward —
// extractions stay aligned with inputFiles by index.
type inputArtifactResult struct {
	StoragePath string
	ArtifactID  string
	Extraction  map[string]any // nil when extraction skipped/failed
}

// processInputArtifactsOpts controls per-call behaviour of the input-
// artifact pipeline. Default (zero-value) preserves the legacy
// Telegram / email shape — files are stored AND auto-extracted into
// project memory at upload time. The companion delegate path opts out
// of auto-extract because the caller explicitly chose a workflow that
// itself ingests files; double-ingesting at upload time + workflow
// time both wastes work AND makes the workflow think the file is
// "already extracted" so it skips staging the raw file (the agent
// then can't find what to ingest — observed 2026-05-28 on
// task_20260528134611, B-10).
type processInputArtifactsOpts struct {
	// SkipAutoExtract makes processInputArtifacts store the file
	// + record the artifact but NOT run tryAutoExtract. The task
	// payload's context will have inputFiles + inputArtifactIDs
	// but NO inputExtractions, so the executor's staging code
	// stages the raw file into the agent container's
	// /app/workspace/artifacts/in/ instead of bypassing it.
	SkipAutoExtract bool
}

// processInputArtifacts decodes each inline InputArtifact, writes
// it to a temp file, snapshots it via the artifact store, and
// runs the extractor pipeline. Returns one result per input in
// order (or an error explaining the first failure — the REST
// surface fails the whole request rather than half-creating a
// task with some files attached and others dropped).
//
// Returns (nil, nil) when no artifact store is wired — callers
// treat that as "REST uploads aren't enabled on this daemon" and
// surface a 503 rather than silently losing the bytes.
func (s *Server) processInputArtifacts(ctx context.Context, projectID string, inputs []InputArtifact) ([]inputArtifactResult, error) {
	return s.processInputArtifactsWithOpts(ctx, projectID, inputs, processInputArtifactsOpts{})
}

// processInputArtifactsWithOpts is the options-aware variant used by
// callers (today: companion delegate via SkipAutoExtract) that need to
// suppress auto-extraction. Backward-compatible: the no-opts form
// above continues to call this with a zero-value struct.
func (s *Server) processInputArtifactsWithOpts(ctx context.Context, projectID string, inputs []InputArtifact, opts processInputArtifactsOpts) ([]inputArtifactResult, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if s.inputArtifactStore == nil {
		return nil, fmt.Errorf("input artifact store is not configured")
	}

	tmpRoot, err := os.MkdirTemp("", "vornik-rest-inputs-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	// Best-effort cleanup; StoreInput copies the bytes into the
	// artifact store so the temp file is safe to remove on return.
	defer func() { _ = os.RemoveAll(tmpRoot) }()

	out := make([]inputArtifactResult, 0, len(inputs))
	for i, in := range inputs {
		if in.Name == "" {
			return nil, fmt.Errorf("inputArtifacts[%d]: name is required", i)
		}
		safeName, err := safepath.CleanFileName(in.Name)
		if err != nil {
			return nil, fmt.Errorf("inputArtifacts[%d]: %w", i, err)
		}
		// Inline base64 is the documented Content shape. We accept
		// both standard and URL-safe encodings to be lenient with
		// CLI tooling.
		decoded, err := decodeArtifactContent(in.Content)
		if err != nil {
			return nil, fmt.Errorf("inputArtifacts[%d]: %w", i, err)
		}
		tmpPath, err := safepath.JoinUnder(tmpRoot, safeName)
		if err != nil {
			return nil, fmt.Errorf("inputArtifacts[%d]: %w", i, err)
		}
		if err := os.WriteFile(tmpPath, decoded, 0o600); err != nil {
			return nil, fmt.Errorf("inputArtifacts[%d]: write temp: %w", i, err)
		}
		art, err := s.inputArtifactStore.StoreInput(ctx, projectID, safeName, tmpPath)
		if err != nil {
			return nil, fmt.Errorf("inputArtifacts[%d]: store: %w", i, err)
		}
		// Auto-extract synchronously per artifact (best-effort:
		// extractor errors don't fail the request — the task still
		// creates with the raw snapshot path). Skipped when the
		// caller asked for raw staging via SkipAutoExtract.
		var extraction map[string]any
		if !opts.SkipAutoExtract {
			extraction = s.tryAutoExtract(ctx, projectID, art)
		}
		out = append(out, inputArtifactResult{
			StoragePath: art.StoragePath,
			ArtifactID:  art.ID,
			Extraction:  extraction,
		})
	}
	return out, nil
}

// tryAutoExtract is the api-side equivalent of
// dispatcher.ToolExecutor.autoExtract — same MIME-fallback shape,
// same idempotent cache via the runner, same best-effort posture.
// Returns nil for every "no extraction" outcome so the caller
// doesn't pollute the task payload with empty entries.
func (s *Server) tryAutoExtract(ctx context.Context, projectID string, art *persistence.Artifact) map[string]any {
	if s == nil || s.extractorRegistry == nil || s.extractorRunner == nil || s.extractedDocsRepo == nil || art == nil {
		return nil
	}
	mime := ""
	if art.MimeType != nil {
		mime = *art.MimeType
	}
	if mime == "" || isGenericMIME(mime) {
		if guessed := extractor.MimeFromFilename(art.Name); guessed != "" {
			mime = guessed
		}
	}
	if mime == "" {
		return nil
	}
	ext, err := s.extractorRegistry.For(mime)
	if err != nil {
		return nil
	}

	// Cache hit: reuse the existing extracted_documents row when
	// the same (extractor name, version) already produced output
	// for this artifact. Matches the dispatcher + email paths.
	if cached, err := s.extractedDocsRepo.GetByArtifact(ctx, art.ID); err == nil && cached != nil {
		if cached.ExtractorName == ext.Name() && cached.ExtractorVersion == ext.Version() &&
			cached.Status == persistence.ExtractedDocumentStatusOK {
			meta := summarizeMetadata(cached)
			return buildExtractionMap(art.ID, cached.ID, meta, cached.SectionCount, 0)
		}
	}

	src := extractor.Source{
		MimeType:     mime,
		OriginalName: art.Name,
	}
	sourcePath, cleanup, err := s.materializeArtifactForExtraction(ctx, art)
	if err != nil {
		s.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("artifact_id", art.ID).
			Msg("rest: auto-extract failed to read artifact bytes")
		return nil
	}
	defer cleanup()
	src.FilePath = sourcePath
	doc, err := s.extractorRunner.Run(ctx, projectID, art.ID, ext, src)
	if err != nil {
		s.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("artifact_id", art.ID).
			Str("mime_type", mime).
			Str("extractor", ext.Name()).
			Msg("rest: auto-extract failed; task created without extraction summary")
		return nil
	}
	chunks := 0
	if s.memoryIndexer != nil {
		ingested, ierr := s.ingestSectionsFromDoc(ctx, doc)
		if ierr != nil {
			s.logger.Warn().Err(ierr).
				Str("project_id", projectID).
				Str("extracted_document_id", doc.ID).
				Msg("rest: memory ingest failed")
		}
		chunks = ingested
	}
	meta := summarizeMetadata(doc)
	s.logger.Info().
		Str("project_id", projectID).
		Str("artifact_id", art.ID).
		Str("extracted_document_id", doc.ID).
		Int("section_count", doc.SectionCount).
		Int("chunks_ingested", chunks).
		Msg("rest: input artifact auto-extracted into project memory")
	return buildExtractionMap(art.ID, doc.ID, meta, doc.SectionCount, chunks)
}

// buildExtractionMap shapes the per-artifact extraction summary the
// executor's prompt builder picks up via
// task.Payload.context.inputExtractions. Kept as map[string]any
// (not a typed struct) because it lands in opaque context JSON.
func buildExtractionMap(artifactID, docID string, meta extractor.Metadata, sectionCount, chunksIngested int) map[string]any {
	return map[string]any{
		"artifact_id":           artifactID,
		"extracted_document_id": docID,
		"title":                 meta.Title,
		"author":                meta.Author,
		"section_count":         sectionCount,
		"chunks_ingested":       chunksIngested,
	}
}

// decodeArtifactContent decodes the InputArtifact.Content field.
// Accepts standard base64 (with or without padding) and URL-safe
// base64; the operator-friendly defaults vornikctl produces are
// std-encoded, but CI pipelines often use URL-safe out of habit.
//
// Strict-decode failures surface as a 400-Validation error to the
// REST caller via the outer error chain.
func decodeArtifactContent(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("content is empty")
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("content is not valid base64")
}

// mergeInputsIntoContext folds the snapshotted-artifact metadata
// into the REST request's RawContext JSON so the executor's
// payload reader (executor/plan.go) sees the same context.inputFiles,
// context.inputArtifactIDs, and context.inputExtractions shape the
// dispatcher's chat-driven path produces.
//
// Preserves any pre-existing keys the caller set explicitly —
// only the three artifact-related fields are merged.
func mergeInputsIntoContext(raw json.RawMessage, results []inputArtifactResult) (json.RawMessage, error) {
	if len(results) == 0 {
		return raw, nil
	}
	ctx := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &ctx); err != nil {
			return nil, fmt.Errorf("context is not a JSON object: %w", err)
		}
	}
	files := make([]string, 0, len(results))
	ids := make([]string, 0, len(results))
	extractions := make([]map[string]any, 0, len(results))
	for _, r := range results {
		files = append(files, r.StoragePath)
		ids = append(ids, r.ArtifactID)
		if r.Extraction != nil {
			extractions = append(extractions, r.Extraction)
		}
	}
	ctx["inputFiles"] = files
	ctx["inputArtifactIDs"] = ids
	if len(extractions) > 0 {
		ctx["inputExtractions"] = extractions
	}
	out, err := json.Marshal(ctx)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Keep the filepath import used even when tests don't reach the
// path-sanitisation branches directly.
var _ = filepath.Base
