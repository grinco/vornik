// Service-layer adapter wiring the email channel's
// AttachmentAutoExtractor seam to the daemon's document-extraction
// pipeline. See document-extraction-design.md §8.1.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

type extractionArtifactOpener interface {
	Open(ctx context.Context, artifactID string) (io.ReadCloser, error)
}

// emailAutoExtractor implements email.AttachmentAutoExtractor by
// looking up the MIME-matching extractor in the registry, running
// the Runner, and chunking the result into project memory.
//
// Returning (nil, nil) signals "no extractor for this MIME" — the
// channel treats that as a no-op without logging an error. Errors
// propagate so the channel can log them and continue.
type emailAutoExtractor struct {
	registry *extractor.Registry
	runner   *extractor.Runner
	repo     persistence.ExtractedDocumentRepository
	indexer  *memory.Indexer
	logger   zerolog.Logger
	opener   extractionArtifactOpener
}

func newEmailAutoExtractor(
	reg *extractor.Registry,
	runner *extractor.Runner,
	repo persistence.ExtractedDocumentRepository,
	indexer *memory.Indexer,
	opener extractionArtifactOpener,
	logger zerolog.Logger,
) *emailAutoExtractor {
	return &emailAutoExtractor{
		registry: reg,
		runner:   runner,
		repo:     repo,
		indexer:  indexer,
		logger:   logger,
		opener:   opener,
	}
}

// AutoExtract mirrors api.Server.ExtractArtifact's flow without the
// HTTP shell — same cache lookup, same idempotent semantics, same
// best-effort memory ingest. Returns nil when the MIME has no
// matching extractor (channel treats this as a no-op).
func (e *emailAutoExtractor) AutoExtract(ctx context.Context, in email.AutoExtractRequest) (*email.AttachmentExtraction, error) {
	if e == nil || e.registry == nil || e.runner == nil || e.repo == nil {
		return nil, errors.New("email auto-extractor: not configured")
	}
	mime := in.MimeType
	// Generic-catchall MIMEs (application/octet-stream) carry no
	// routing signal — fall back to the filename extension before
	// failing. Same logic as api.Server.ExtractArtifact.
	if mime == "" || isGenericMIME(mime) {
		if guessed := extractor.MimeFromFilename(in.Name); guessed != "" {
			mime = guessed
		}
	}
	if mime == "" {
		return nil, nil
	}
	ext, err := e.registry.For(mime)
	if err != nil {
		if errors.Is(err, extractor.ErrNoExtractor) {
			return nil, nil // unknown MIME — silently skip
		}
		return nil, err
	}

	// Idempotent cache lookup. Re-running the same extractor over
	// the same source artifact returns the existing row instead of
	// duplicating work — important when an operator forwards the
	// same book twice.
	if cached, err := e.repo.GetByArtifact(ctx, in.ArtifactID); err == nil && cached != nil {
		if cached.ExtractorName == ext.Name() && cached.ExtractorVersion == ext.Version() &&
			cached.Status == persistence.ExtractedDocumentStatusOK {
			return summarizeCached(cached), nil
		}
	}

	src := extractor.Source{
		MimeType:     mime,
		OriginalName: in.Name,
	}
	sourcePath, cleanup, err := e.materializeSource(ctx, in)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	src.FilePath = sourcePath
	doc, err := e.runner.Run(ctx, in.ProjectID, in.ArtifactID, ext, src)
	if err != nil {
		return nil, err
	}

	chunks := 0
	if e.indexer != nil {
		ingested, err := e.ingestSections(ctx, doc)
		if err != nil {
			// Memory ingest is best-effort. The extraction is durable
			// on disk + in the DB; a future operator-triggered re-run
			// can replay just the indexer step.
			e.logger.Warn().Err(err).
				Str("project_id", in.ProjectID).
				Str("extracted_document_id", doc.ID).
				Msg("email auto-extract: memory ingest failed")
		}
		chunks = ingested
	}

	meta := decodeMetadata(doc.MetadataBlob)
	return &email.AttachmentExtraction{
		ExtractedDocumentID: doc.ID,
		Title:               meta.Title,
		Author:              meta.Author,
		SectionCount:        doc.SectionCount,
		ChunksIngested:      chunks,
	}, nil
}

func (e *emailAutoExtractor) materializeSource(ctx context.Context, in email.AutoExtractRequest) (string, func(), error) {
	if e.opener == nil {
		return in.StoragePath, func() {}, nil
	}
	rc, err := e.opener.Open(ctx, in.ArtifactID)
	if err != nil {
		return "", func() {}, err
	}
	defer func() { _ = rc.Close() }()

	tmpRoot, err := os.MkdirTemp("", "vornik-extract-artifact-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpRoot) }
	safeName, err := safepath.CleanFileName(in.Name)
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

// ingestSections reads each section file off disk and pushes it
// through the memory indexer with provenance pointers populated.
// Mirrors api.Server.ingestSectionsFromDoc; the duplication is
// acceptable for now (both call sites are thin) — when a third
// caller appears we'll lift it into a shared helper.
func (e *emailAutoExtractor) ingestSections(ctx context.Context, doc *persistence.ExtractedDocument) (int, error) {
	var outline []extractor.OutlineEntry
	if len(doc.OutlineBlob) > 0 {
		if err := json.Unmarshal(doc.OutlineBlob, &outline); err != nil {
			return 0, err
		}
	}
	meta := decodeMetadata(doc.MetadataBlob)
	title := meta.Title
	if title == "" {
		title = doc.SourceArtifactID
	}

	sections := make([]memory.ExtractedSection, 0, len(outline))
	for _, entry := range outline {
		content, err := extractor.ReadSection(doc, entry.SectionID)
		if err != nil {
			e.logger.Warn().Err(err).
				Str("extracted_document_id", doc.ID).
				Str("section_id", entry.SectionID).
				Msg("email auto-extract: failed to read section from disk")
			continue
		}
		sourceName := title
		if entry.Title != "" && entry.Title != title {
			sourceName = title + " · " + entry.Title
		}
		sections = append(sections, memory.ExtractedSection{
			SectionID:  entry.SectionID,
			SourceName: sourceName,
			Content:    content,
		})
	}
	return e.indexer.IngestExtractedSections(ctx, doc.ProjectID, "", doc.SourceArtifactID, doc.ID, sections)
}

// summarizeCached produces the channel-facing summary from a cached
// extracted_documents row. The chunk count is 0 because we don't
// re-run the indexer on cache hits — the chunks are already in
// project_memory_chunks from the first extraction. (A future
// enhancement could query the chunk count for a richer reply.)
func summarizeCached(doc *persistence.ExtractedDocument) *email.AttachmentExtraction {
	meta := decodeMetadata(doc.MetadataBlob)
	return &email.AttachmentExtraction{
		ExtractedDocumentID: doc.ID,
		Title:               meta.Title,
		Author:              meta.Author,
		SectionCount:        doc.SectionCount,
		ChunksIngested:      0,
	}
}

func decodeMetadata(blob []byte) extractor.Metadata {
	var m extractor.Metadata
	if len(blob) == 0 {
		return m
	}
	_ = json.Unmarshal(blob, &m)
	return m
}

// isGenericMIME mirrors the api-side detector so both the email
// channel and the operator /extract endpoint treat the same
// catchall MIMEs as routing-by-filename. Kept in lockstep with
// api.isGenericMIME by intent — when one grows, the other follows.
func isGenericMIME(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "application/octet-stream", "binary/octet-stream", "application/x-binary":
		return true
	}
	return false
}

// dispatcherAutoExtractorAdapter wraps the same extraction logic
// for the dispatcher's AttachmentAutoExtractor interface. The
// dispatcher and email types are structurally identical but live
// in different packages — this shim translates the request /
// response shape at the boundary so both call sites share one
// implementation (and one cache, via the underlying repo).
type dispatcherAutoExtractorAdapter struct {
	inner *emailAutoExtractor
}

func newDispatcherAutoExtractor(
	reg *extractor.Registry,
	runner *extractor.Runner,
	repo persistence.ExtractedDocumentRepository,
	indexer *memory.Indexer,
	opener extractionArtifactOpener,
	logger zerolog.Logger,
) *dispatcherAutoExtractorAdapter {
	return &dispatcherAutoExtractorAdapter{
		inner: newEmailAutoExtractor(reg, runner, repo, indexer, opener, logger),
	}
}

// AutoExtract satisfies dispatcher.AttachmentAutoExtractor.
// Converts dispatcher.AutoExtractRequest → email.AutoExtractRequest
// internally; the underlying extraction is identical so cache hits
// from the email path also benefit dispatcher-triggered calls.
func (d *dispatcherAutoExtractorAdapter) AutoExtract(ctx context.Context, in dispatcher.AutoExtractRequest) (*dispatcher.AttachmentExtraction, error) {
	if d == nil || d.inner == nil {
		return nil, errors.New("dispatcher auto-extractor: not configured")
	}
	out, err := d.inner.AutoExtract(ctx, email.AutoExtractRequest{
		ProjectID:   in.ProjectID,
		ArtifactID:  in.ArtifactID,
		Name:        in.Name,
		MimeType:    in.MimeType,
		StoragePath: in.StoragePath,
	})
	if err != nil || out == nil {
		return nil, err
	}
	return &dispatcher.AttachmentExtraction{
		ExtractedDocumentID: out.ExtractedDocumentID,
		Title:               out.Title,
		Author:              out.Author,
		SectionCount:        out.SectionCount,
		ChunksIngested:      out.ChunksIngested,
	}, nil
}
