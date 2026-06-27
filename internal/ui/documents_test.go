// Hermetic tests for /ui/projects/<id>/documents. The handler
// fetches rows from ExtractedDocumentRepository, truncates by the
// shared ?limit selector, and surfaces both the truncated count
// and a pre-truncate total so the "showing N of M" header is
// honest about what was capped.
package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubExtractedDocsRepo records ListByProject calls so the test can
// assert the limit param the handler asked for and feed back a
// canned row list. Other methods panic — the documents listing
// page only touches ListByProject.
type stubExtractedDocsRepo struct {
	persistence.ExtractedDocumentRepository
	listByProjectFn func(ctx context.Context, projectID string, limit int) ([]*persistence.ExtractedDocument, error)
	listCalledWith  int
}

func (s *stubExtractedDocsRepo) ListByProject(ctx context.Context, projectID string, limit int) ([]*persistence.ExtractedDocument, error) {
	s.listCalledWith = limit
	if s.listByProjectFn != nil {
		return s.listByProjectFn(ctx, projectID, limit)
	}
	return nil, nil
}

// fakeDocs returns n rows with deterministic IDs / timestamps so
// truncation tests can assert which rows survived.
func fakeDocs(n int) []*persistence.ExtractedDocument {
	out := make([]*persistence.ExtractedDocument, n)
	for i := 0; i < n; i++ {
		out[i] = &persistence.ExtractedDocument{
			ID:               fmt.Sprintf("doc-%02d", i),
			ProjectID:        "p1",
			SourceArtifactID: fmt.Sprintf("art-%02d", i),
			ExtractorName:    "pdf",
			ExtractorVersion: "1.0.0",
			MimeType:         "application/pdf",
			Status:           "OK",
			ExtractedAt:      time.Now().Add(-time.Duration(i) * time.Hour),
		}
	}
	return out
}

// TestProjectDocuments_PageSizeLimit_TruncatesAndExposesTotal — the
// "showing N of M" header copy depends on the handler reading
// ?limit, truncating, and surfacing the pre-truncate total. Pinned
// against the 10/20/50/100 shared allowlist so a stray bump to a
// different option set breaks the test.
func TestProjectDocuments_PageSizeLimit_TruncatesAndExposesTotal(t *testing.T) {
	repo := &stubExtractedDocsRepo{
		listByProjectFn: func(_ context.Context, _ string, _ int) ([]*persistence.ExtractedDocument, error) {
			return fakeDocs(30), nil
		},
	}
	s := NewServer(WithExtractedDocumentsRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/documents?limit=10", nil)
	w := httptest.NewRecorder()
	s.ProjectDocuments(w, req, "p1")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "showing 10 of 30",
		"header must show truncated count vs pre-truncate total")
	assert.Contains(t, body, "doc-00", "first row in cap")
	assert.Contains(t, body, "doc-09", "last row in cap")
	assert.NotContains(t, body, "doc-10", "row outside cap must not render")
}

// TestProjectDocuments_PageSizeLimit_DefaultsAndRejectsInvalid —
// missing / garbage / out-of-allowlist ?limit values must fall back
// to DefaultPageSize. The safety belt prevents a crafted
// ?limit=9999999 from forcing a giant repo scan; the handler
// passes a 200-row cap to the repo regardless.
func TestProjectDocuments_PageSizeLimit_DefaultsAndRejectsInvalid(t *testing.T) {
	repo := &stubExtractedDocsRepo{
		listByProjectFn: func(_ context.Context, _ string, _ int) ([]*persistence.ExtractedDocument, error) {
			return fakeDocs(30), nil
		},
	}
	s := NewServer(WithExtractedDocumentsRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/documents?limit=9999", nil)
	w := httptest.NewRecorder()
	s.ProjectDocuments(w, req, "p1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(),
		fmt.Sprintf("showing %d of 30", DefaultPageSize),
		"out-of-allowlist limit must fall back to default")
	assert.Equal(t, 200, repo.listCalledWith,
		"repo always sees the 200-row totals cap, not the user-supplied limit")
}

// TestProjectDocuments_RendersSelector — the handler must include
// the shared pageSizeSelector partial in the rendered output so an
// operator has a visible Show dropdown. Asserted via the partial's
// form-action + select-name markers so a rename of one breaks the
// test loudly.
func TestProjectDocuments_RendersSelector(t *testing.T) {
	repo := &stubExtractedDocsRepo{}
	s := NewServer(WithExtractedDocumentsRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/documents", nil)
	w := httptest.NewRecorder()
	s.ProjectDocuments(w, req, "p1")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `action="/ui/projects/p1/documents"`,
		"selector form must post to the documents page")
	assert.Contains(t, body, `name="limit"`,
		"default ParamName is 'limit' for single-list pages")
	// Smoke-check that at least one of the canonical option values renders.
	assert.True(t,
		strings.Contains(body, `value="10"`) && strings.Contains(body, `value="20"`),
		"shared PageSizeOptions must render in the dropdown")
}
