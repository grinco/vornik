package ui

// Regression: 2026-08-19 miss-contract normalisation.
//
// ExtractedDocumentRepository.Get used to answer (nil, nil) for an id that
// names no row; it now answers persistence.ErrNotFound like every other
// lookup. ProjectDocumentDetail treated any error as a server fault, so the
// change turned a stale bookmark from a 404 into a 500 that also echoed the
// storage error into the response body.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectDocumentDetail_unknownDocumentIs404NotServerError(t *testing.T) {
	// getDoc nil ⇒ the double reports the miss the way both backends do.
	repo := &auditDocsRepo{}
	srv := NewServer(WithExtractedDocumentsRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/projects/B/documents/doc_gone", "B")
	rec := httptest.NewRecorder()

	srv.ProjectDocumentDetail(rec, req, "B", "doc_gone")

	if rec.Code != http.StatusNotFound {
		t.Errorf("detail for an unknown document: status=%d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Failed to load document") {
		t.Error("an absent row must not be reported to the operator as a load failure")
	}
}
