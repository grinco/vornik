// Package api — document-extraction HTTP surface.
//
// One endpoint right now:
//
//	POST /api/v1/projects/{projectId}/artifacts/{artifactId}/extract
//	  Runs the registered extractor for the artifact's MIME type,
//	  persists the extracted_documents row, and (when a memory
//	  indexer is wired) chunks the sections into project_memory_chunks.
//	  Returns the extracted document summary so the operator can
//	  curl-verify the result.
//
// Future endpoints (Phase 2 / Phase 6 of the design doc):
//   - GET .../documents                              list
//   - GET .../documents/{id}                         metadata + outline
//   - GET .../documents/{id}/sections/{sectionId}    one section
//   - GET .../documents/{id}/search?q=...            per-doc search
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

// ExtractArtifactResponse mirrors the parts of an ExtractedDocument
// the curl-driving operator actually needs to see after a successful
// extract. Storage path is intentionally exposed — the operator UI
// will later link to the metadata.json / outline.json artefacts on
// disk for spot-checks.
type ExtractArtifactResponse struct {
	ExtractedDocumentID string `json:"extracted_document_id"`
	SourceArtifactID    string `json:"source_artifact_id"`
	ExtractorName       string `json:"extractor_name"`
	ExtractorVersion    string `json:"extractor_version"`
	MimeType            string `json:"mime_type"`
	Title               string `json:"title,omitempty"`
	Author              string `json:"author,omitempty"`
	SectionCount        int    `json:"section_count"`
	TotalTextBytes      int64  `json:"total_text_bytes"`
	StoragePath         string `json:"storage_path"`
	ChunksIngested      int    `json:"chunks_ingested"`
	Status              string `json:"status"`
}

// ExtractArtifact handles
// POST /api/v1/projects/{projectId}/artifacts/{artifactId}/extract.
//
// Flow:
//  1. Validate the URL has both ids and pre-flight surfaces are wired.
//  2. Load the source artifact; reject if cross-project or missing.
//  3. Dispatch via registry on the artifact's MIME type. When the
//     MIME isn't on the artifact row we fall back to the filename
//     extension — emailed attachments sometimes arrive with no
//     Content-Type. ErrNoExtractor → 415 Unsupported Media Type.
//  4. Re-use an existing extracted_documents row when one already
//     matches (source_artifact + extractor name + version). Idempotent
//     by design; cached returns avoid running a deterministic
//     extractor twice over the same bytes.
//  5. Run the extractor via the Runner; the Runner writes sections
//     to disk and Upserts the row.
//  6. (Optional) Chunk sections into project memory via the indexer.
//     Each section becomes one or more chunks with provenance
//     pointers populated.
//  7. Return the summary shape.
func (s *Server) ExtractArtifact(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	artifactID := strings.TrimSpace(r.PathValue("artifactId"))
	if artifactID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "artifactId is required")
		return
	}
	if s.extractorRegistry == nil || s.extractorRunner == nil || s.extractedDocsRepo == nil || s.artifactRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"document extraction is not configured on this daemon")
		return
	}

	ctx := r.Context()
	art, err := s.artifactRepo.Get(ctx, artifactID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load artifact")
		return
	}
	if art == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "artifact not found")
		return
	}
	if art.ProjectID != projectID {
		// Don't leak the cross-project artifact's existence — 404 is the
		// right shape here (matches the existing project-scope gate
		// pattern in the artifact read path).
		respondError(w, http.StatusNotFound, "NOT_FOUND", "artifact not found")
		return
	}

	mime := ""
	if art.MimeType != nil {
		mime = *art.MimeType
	}
	// Generic-catchall MIMEs (the Telegram/webchat channels often
	// emit "application/octet-stream" for any uploaded blob) carry
	// no extractor-routing signal. Treat them as "missing" and
	// re-derive from the filename extension before failing.
	if mime == "" || isGenericMIME(mime) {
		if guessed := extractor.MimeFromFilename(art.Name); guessed != "" {
			mime = guessed
		}
	}
	if mime == "" {
		respondError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"artifact has no MIME type and filename extension is unknown")
		return
	}

	ext, err := s.extractorRegistry.For(mime)
	if err != nil {
		if errors.Is(err, extractor.ErrNoExtractor) {
			respondError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
				"no extractor registered for MIME type "+mime)
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "registry lookup failed")
		return
	}

	// Cache lookup: if the same (source, extractor name, version)
	// triple already exists, return it without re-running. The
	// document-ingest path is idempotent by design; an operator
	// hammering the endpoint shouldn't pay the extraction cost
	// repeatedly.
	if cached, err := s.extractedDocsRepo.GetByArtifact(ctx, artifactID); err == nil && cached != nil {
		if cached.ExtractorName == ext.Name() && cached.ExtractorVersion == ext.Version() &&
			cached.Status == persistence.ExtractedDocumentStatusOK {
			respondJSON(w, http.StatusOK, buildExtractResponse(cached, summarizeMetadata(cached), 0))
			return
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
			Str("artifact_id", artifactID).
			Msg("extract: failed to read artifact bytes")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to read artifact bytes")
		return
	}
	defer cleanup()
	src.FilePath = sourcePath
	doc, err := s.extractorRunner.Run(ctx, projectID, artifactID, ext, src)
	if err != nil {
		s.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("artifact_id", artifactID).
			Str("extractor", ext.Name()).
			Msg("extract: runner failed")
		respondError(w, http.StatusUnprocessableEntity, "EXTRACTION_FAILED", err.Error())
		return
	}

	chunksIngested := 0
	if s.memoryIndexer != nil {
		ingested, err := s.ingestSectionsFromDoc(ctx, doc)
		if err != nil {
			// Extraction succeeded; memory ingest is best-effort. The
			// row + section files on disk are durable so a future
			// reindex sweep can replay. Log + continue.
			s.logger.Warn().Err(err).
				Str("project_id", projectID).
				Str("extracted_document_id", doc.ID).
				Msg("extract: memory ingest failed")
		}
		chunksIngested = ingested
	}

	respondJSON(w, http.StatusOK, buildExtractResponse(doc, summarizeMetadata(doc), chunksIngested))
}

func (s *Server) materializeArtifactForExtraction(ctx context.Context, art *persistence.Artifact) (string, func(), error) {
	if art == nil {
		return "", func() {}, errors.New("artifact is nil")
	}
	if s.artifactOpener == nil {
		return art.StoragePath, func() {}, nil
	}
	rc, err := s.artifactOpener.Open(ctx, art.ID)
	if err != nil {
		return "", func() {}, err
	}
	defer func() { _ = rc.Close() }()

	tmpRoot, err := os.MkdirTemp("", "vornik-extract-artifact-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpRoot) }

	safeName, err := safepath.CleanFileName(art.Name)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	tmpPath, err := safepath.JoinUnder(tmpRoot, safeName)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmpPath, cleanup, nil
}

// ingestSectionsFromDoc reads each section file off disk and feeds
// them into the memory indexer. Kept here (not on the runner)
// because memory wiring is api-layer policy: not every binary that
// links the runner wants chunking side effects.
func (s *Server) ingestSectionsFromDoc(ctx context.Context, doc *persistence.ExtractedDocument) (int, error) {
	if s.memoryIndexer == nil || doc == nil {
		return 0, nil
	}
	// Decode outline so we can iterate sections in reading order and
	// build a citation-friendly source-name per section.
	var outline []extractor.OutlineEntry
	if len(doc.OutlineBlob) > 0 {
		if err := json.Unmarshal(doc.OutlineBlob, &outline); err != nil {
			return 0, err
		}
	}
	var metadata extractor.Metadata
	if len(doc.MetadataBlob) > 0 {
		_ = json.Unmarshal(doc.MetadataBlob, &metadata)
	}
	title := metadata.Title
	if title == "" {
		title = doc.SourceArtifactID
	}

	sections := make([]ExtractedSectionInput, 0, len(outline))
	for _, entry := range outline {
		content, err := extractor.ReadSection(doc, entry.SectionID)
		if err != nil {
			s.logger.Warn().Err(err).
				Str("extracted_document_id", doc.ID).
				Str("section_id", entry.SectionID).
				Msg("extract: failed to read section from disk")
			continue
		}
		sourceName := title
		if entry.Title != "" && entry.Title != title {
			sourceName = title + " · " + entry.Title
		}
		sections = append(sections, ExtractedSectionInput{
			SectionID:  entry.SectionID,
			SourceName: sourceName,
			Content:    content,
		})
	}
	return s.memoryIndexer.IngestExtractedSections(
		ctx,
		doc.ProjectID,
		"", // taskID — extraction is task-less when triggered directly via the operator API
		doc.SourceArtifactID,
		doc.ID,
		sections,
	)
}

// summarizeMetadata pulls the operator-visible bits out of the JSON
// blob so the response shape stays flat. Tolerant of malformed blobs:
// returns an empty Metadata rather than failing the response.
func summarizeMetadata(doc *persistence.ExtractedDocument) extractor.Metadata {
	var m extractor.Metadata
	if doc == nil || len(doc.MetadataBlob) == 0 {
		return m
	}
	_ = json.Unmarshal(doc.MetadataBlob, &m)
	return m
}

func buildExtractResponse(doc *persistence.ExtractedDocument, meta extractor.Metadata, chunks int) ExtractArtifactResponse {
	return ExtractArtifactResponse{
		ExtractedDocumentID: doc.ID,
		SourceArtifactID:    doc.SourceArtifactID,
		ExtractorName:       doc.ExtractorName,
		ExtractorVersion:    doc.ExtractorVersion,
		MimeType:            doc.MimeType,
		Title:               meta.Title,
		Author:              meta.Author,
		SectionCount:        doc.SectionCount,
		TotalTextBytes:      doc.TotalTextBytes,
		StoragePath:         doc.StoragePath,
		ChunksIngested:      chunks,
		Status:              doc.Status,
	}
}

// isGenericMIME identifies MIME types that carry no routing
// signal — uploaders that didn't sniff content often default to
// these. We treat them as "missing" and re-derive from filename
// extension. List is deliberately narrow; specific MIMEs we
// don't have an extractor for stay as 415 errors.
func isGenericMIME(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "application/octet-stream", "binary/octet-stream", "application/x-binary":
		return true
	}
	return false
}

// Ensure context is recognised as an import for the file even if
// editors strip "unused" imports — the handler signature references
// it transitively via r.Context().
var _ = context.Background
