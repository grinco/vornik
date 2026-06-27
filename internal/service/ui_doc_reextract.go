// UI → re-extract adapter. Bridges the documents-detail page's
// POST /re-extract button to the existing extraction pipeline.
// Forces a fresh extraction by deleting the cached
// extracted_documents row first; the next runner pass produces
// a brand-new row with the current extractor version + bumps
// chunks into project memory.
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

// uiDocumentReExtractor satisfies ui.DocumentReExtractor by
// reusing the same emailAutoExtractor logic with a "force"
// preface — drop the cached row so the underlying Runner sees
// no match and re-runs the parser. Pure wrapper.
type uiDocumentReExtractor struct {
	repo    persistence.ExtractedDocumentRepository
	inner   *emailAutoExtractor
	logger  zerolog.Logger
	artRepo persistence.ArtifactRepository
}

func newUIDocumentReExtractor(
	reg *extractor.Registry,
	runner *extractor.Runner,
	repo persistence.ExtractedDocumentRepository,
	artRepo persistence.ArtifactRepository,
	indexer *memory.Indexer,
	opener extractionArtifactOpener,
	logger zerolog.Logger,
) *uiDocumentReExtractor {
	return &uiDocumentReExtractor{
		repo:    repo,
		inner:   newEmailAutoExtractor(reg, runner, repo, indexer, opener, logger),
		logger:  logger,
		artRepo: artRepo,
	}
}

// ReExtract drops any cached extracted_documents row for the
// (sourceArtifactID) and runs the AutoExtractor again. The caller
// (UI handler) renders the redirect message from the returned
// error. Idempotent on the "no cached row" path — operators can
// click the button on a freshly-failed extraction and the next
// run produces a clean row.
func (u *uiDocumentReExtractor) ReExtract(ctx context.Context, projectID, sourceArtifactID string) error {
	if u == nil || u.repo == nil || u.inner == nil || u.artRepo == nil {
		return errors.New("re-extract: pipeline not configured")
	}

	// Cache delete — the inner extractor short-circuits on a
	// cached OK row, which is the wrong behaviour for a manual
	// re-extract trigger. Drop the row so the runner re-parses.
	// We accept the cost of losing existing memory_chunks
	// provenance pointers (they were stamped against the old row
	// id); the new chunks will re-stamp.
	if cached, err := u.repo.GetByArtifact(ctx, sourceArtifactID); err == nil && cached != nil {
		if delErr := u.repo.Delete(ctx, cached.ID); delErr != nil {
			return fmt.Errorf("delete cached row: %w", delErr)
		}
	}

	// Re-derive the AutoExtract request shape from the artifact.
	art, err := u.artRepo.Get(ctx, sourceArtifactID)
	if err != nil {
		return fmt.Errorf("lookup artifact: %w", err)
	}
	if art == nil || art.ProjectID != projectID {
		return fmt.Errorf("artifact %q not found in project %q", sourceArtifactID, projectID)
	}
	mime := ""
	if art.MimeType != nil {
		mime = *art.MimeType
	}
	if _, err := u.inner.AutoExtract(ctx, email.AutoExtractRequest{
		ProjectID:   projectID,
		ArtifactID:  art.ID,
		Name:        art.Name,
		MimeType:    mime,
		StoragePath: art.StoragePath,
	}); err != nil {
		return fmt.Errorf("re-extract: %w", err)
	}
	return nil
}

// Compile-time assertion that uiDocumentReExtractor satisfies the
// ui.DocumentReExtractor interface — catches drift at build time
// rather than at runtime when an operator clicks the button.
var _ ui.DocumentReExtractor = (*uiDocumentReExtractor)(nil)
