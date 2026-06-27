// Operator-visible UI for extracted documents — Phase 6 of the
// document-extraction design. Three surfaces:
//
//	GET  /ui/projects/{id}/documents              — list
//	GET  /ui/projects/{id}/documents/{docID}      — detail (outline)
//	POST /ui/projects/{id}/documents/{docID}/re-extract — re-run
//
// Read paths are wired even when extraction itself isn't (operators
// can audit prior extractions). Write paths nil-check the extractor
// pipeline and surface 503 when missing.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/extractor"
)

// projectDocumentsListData backs templates/project_documents.html.
// One row per extracted_document, ordered newest-first.
type projectDocumentsListData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	Documents   []extractedDocumentRow
	// TotalDocuments is the pre-truncate row count so the header
	// can render "showing N of M". When equal to len(Documents)
	// the list isn't capped.
	TotalDocuments int
	// Limit / LimitOptions drive the shared pageSizeSelector partial.
	Limit        int
	LimitOptions []int
	Success      string
	Error        string
}

type extractedDocumentRow struct {
	ID                  string
	SourceArtifactID    string
	ExtractorName       string
	ExtractorVersion    string
	MimeType            string
	Title               string
	Author              string
	SectionCount        int
	TotalTextBytes      int64
	Status              string
	ExtractedAtDisplay  string // e.g. "2 hours ago"
	ExtractedAtAbsolute string // e.g. "2026-05-21 14:33 UTC"
	DetailURL           string
}

type projectDocumentDetailData struct {
	Title        string
	CurrentPage  string
	ProjectID    string
	Document     extractedDocumentDetail
	Outline      []extractedDocumentSection
	StoragePath  string
	BackURL      string
	ReExtractURL string
	Success      string
	Error        string
	CanReExtract bool
}

type extractedDocumentDetail struct {
	ID                  string
	SourceArtifactID    string
	ExtractorName       string
	ExtractorVersion    string
	MimeType            string
	Title               string
	Author              string
	Publisher           string
	PublicationDate     string
	ISBN                string
	Language            string
	SectionCount        int
	TotalTextBytes      int64
	Status              string
	ExtractedAtDisplay  string
	ExtractedAtAbsolute string
}

type extractedDocumentSection struct {
	SectionID         string
	Title             string
	Depth             int
	PageStart         int
	TimestampStartSec int
	TextBytes         int
}

// ProjectDocuments handles GET /ui/projects/{id}/documents.
// Lists every extracted_documents row for the project, newest
// first. Empty projects render an empty-state row, matching the
// posture of ProjectArtifacts.
func (s *Server) ProjectDocuments(w http.ResponseWriter, r *http.Request, projectID string) {
	if err := validateProjectIDComponent(projectID); err != nil {
		http.Error(w, "Invalid project id: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Project-scope guard — a caller scoped to other projects must
	// not be able to list (or probe the existence of) this project's
	// documents. 404 (not 403) mirrors the cross-project guards in the
	// detail/re-extract paths so wrong-scope is indistinguishable from
	// not-found.
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}
	if s.extractedDocsRepo == nil {
		http.Error(w, "Document-extraction pipeline is not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	limit := parsePageSize(r.URL.Query().Get("limit"))
	// Fetch one row more than the limit so we can tell "exactly
	// `limit` rows on this page" from "more behind the cap" — the
	// extra row drives the "showing N of M+" header copy below.
	// Capped at the historical ceiling of 200 so a pathological
	// project doesn't hammer the repo.
	const totalsCap = 200
	docs, err := s.extractedDocsRepo.ListByProject(ctx, projectID, totalsCap)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("ProjectDocuments: list failed")
		http.Error(w, "Failed to list documents: "+err.Error(), http.StatusInternalServerError)
		return
	}
	total := len(docs)
	if limit > 0 && len(docs) > limit {
		docs = docs[:limit]
	}

	rows := make([]extractedDocumentRow, 0, len(docs))
	for _, d := range docs {
		meta := decodeMetaForList(d.MetadataBlob)
		rows = append(rows, extractedDocumentRow{
			ID:                  d.ID,
			SourceArtifactID:    d.SourceArtifactID,
			ExtractorName:       d.ExtractorName,
			ExtractorVersion:    d.ExtractorVersion,
			MimeType:            d.MimeType,
			Title:               meta.Title,
			Author:              meta.Author,
			SectionCount:        d.SectionCount,
			TotalTextBytes:      d.TotalTextBytes,
			Status:              d.Status,
			ExtractedAtDisplay:  humanAgo(d.ExtractedAt),
			ExtractedAtAbsolute: d.ExtractedAt.UTC().Format("2006-01-02 15:04 UTC"),
			DetailURL:           fmt.Sprintf("/ui/projects/%s/documents/%s", projectID, d.ID),
		})
	}

	data := projectDocumentsListData{
		Title:          "Documents: " + projectID,
		CurrentPage:    "projects",
		ProjectID:      projectID,
		Documents:      rows,
		TotalDocuments: total,
		Limit:          limit,
		LimitOptions:   PageSizeOptions,
		Success:        r.URL.Query().Get("ok"),
		Error:          r.URL.Query().Get("err"),
	}
	s.render(w, "project_documents.html", data)
}

// ProjectDocumentDetail handles GET /ui/projects/{id}/documents/{docID}.
// Shows the document's full metadata + outline so an operator can
// confirm extraction quality before running tasks that consume it.
func (s *Server) ProjectDocumentDetail(w http.ResponseWriter, r *http.Request, projectID, docID string) {
	if err := validateProjectIDComponent(projectID); err != nil {
		http.Error(w, "Invalid project id: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Project-scope guard — complements (does not replace) the
	// doc.ProjectID==projectID match below. Rejecting wrong-scope
	// callers up front means they can't probe document existence or
	// reach the repo at all. 404 keeps not-allowed and not-found
	// indistinguishable.
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}
	if s.extractedDocsRepo == nil {
		http.Error(w, "Document-extraction pipeline is not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	doc, err := s.extractedDocsRepo.Get(ctx, docID)
	if err != nil {
		s.logger.Error().Err(err).Str("doc_id", docID).Msg("ProjectDocumentDetail: get failed")
		http.Error(w, "Failed to load document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if doc == nil {
		http.NotFound(w, r)
		return
	}
	// Cross-project guard — never leak another project's document
	// even when the URL guesses an id.
	if doc.ProjectID != projectID {
		http.NotFound(w, r)
		return
	}

	meta := decodeMetaForList(doc.MetadataBlob)
	outline := decodeOutlineForList(doc.OutlineBlob)
	sections := make([]extractedDocumentSection, 0, len(outline))
	for _, e := range outline {
		sections = append(sections, extractedDocumentSection{
			SectionID:         e.SectionID,
			Title:             e.Title,
			Depth:             e.Depth,
			PageStart:         e.PageStart,
			TimestampStartSec: e.TimestampStartSec,
			TextBytes:         e.TextBytes,
		})
	}

	data := projectDocumentDetailData{
		Title:       "Document: " + meta.Title,
		CurrentPage: "projects",
		ProjectID:   projectID,
		Document: extractedDocumentDetail{
			ID:                  doc.ID,
			SourceArtifactID:    doc.SourceArtifactID,
			ExtractorName:       doc.ExtractorName,
			ExtractorVersion:    doc.ExtractorVersion,
			MimeType:            doc.MimeType,
			Title:               meta.Title,
			Author:              meta.Author,
			Publisher:           meta.Publisher,
			PublicationDate:     meta.PublicationDate,
			ISBN:                meta.ISBN,
			Language:            meta.Language,
			SectionCount:        doc.SectionCount,
			TotalTextBytes:      doc.TotalTextBytes,
			Status:              doc.Status,
			ExtractedAtDisplay:  humanAgo(doc.ExtractedAt),
			ExtractedAtAbsolute: doc.ExtractedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		},
		Outline:      sections,
		StoragePath:  doc.StoragePath,
		BackURL:      fmt.Sprintf("/ui/projects/%s/documents", projectID),
		ReExtractURL: fmt.Sprintf("/ui/projects/%s/documents/%s/re-extract", projectID, doc.ID),
		Success:      r.URL.Query().Get("ok"),
		Error:        r.URL.Query().Get("err"),
		CanReExtract: s.documentReExtractor != nil,
	}
	s.render(w, "project_document_detail.html", data)
}

// ProjectDocumentReExtract handles POST
// /ui/projects/{id}/documents/{docID}/re-extract. Triggers a fresh
// extraction over the same source artifact via the existing
// extract pipeline — useful when an extractor upgrade should
// replay over historical inputs, or when the original extraction
// produced a PARTIAL row the operator wants retried.
//
// Redirects back to the detail page with ?ok or ?err so the
// operator sees the outcome without watching daemon logs.
func (s *Server) ProjectDocumentReExtract(w http.ResponseWriter, r *http.Request, projectID, docID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := validateProjectIDComponent(projectID); err != nil {
		http.Error(w, "Invalid project id: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Project-scope guard — refuse before any repo Get or ReExtract
	// work so a wrong-scope caller can neither probe existence nor
	// trigger a (potentially paid / load-bearing) re-extraction over
	// another project's artifact. 404 mirrors the existing guard below.
	if !api.RequestAllowsProject(r, projectID) {
		http.NotFound(w, r)
		return
	}
	if s.extractedDocsRepo == nil || s.documentReExtractor == nil {
		http.Error(w, "Document-extraction pipeline is not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	doc, err := s.extractedDocsRepo.Get(ctx, docID)
	if err != nil || doc == nil || doc.ProjectID != projectID {
		http.NotFound(w, r)
		return
	}

	dest := fmt.Sprintf("/ui/projects/%s/documents/%s", projectID, docID)
	if err := s.documentReExtractor.ReExtract(ctx, projectID, doc.SourceArtifactID); err != nil {
		// 303 redirect to the same detail page with the error
		// surfaced. Operators always land somewhere they can read
		// the message rather than getting a raw 5xx body.
		http.Redirect(w, r, dest+"?err="+urlQueryEscape(truncateMsg(err.Error())), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, dest+"?ok=re-extracted", http.StatusSeeOther)
}

// DocumentReExtractor is the narrow seam between the UI and the
// extract pipeline so the documents page can re-run extractions
// without taking a hard dependency on api.Server. Production
// wires a thin adapter; nil disables the re-extract action
// gracefully (the button is still rendered, but the page shows
// "not configured").
type DocumentReExtractor interface {
	ReExtract(ctx context.Context, projectID, sourceArtifactID string) error
}

// WithDocumentReExtractor wires the re-extract trigger.
func WithDocumentReExtractor(x DocumentReExtractor) ServerOption {
	return func(s *Server) { s.documentReExtractor = x }
}

// decodeMetaForList tolerates malformed metadata blobs by
// returning a zero-value Metadata rather than panicking. The list
// page must render even when one document has a corrupt blob;
// the operator can investigate via the detail page.
func decodeMetaForList(blob []byte) extractor.Metadata {
	var m extractor.Metadata
	if len(blob) == 0 {
		return m
	}
	_ = json.Unmarshal(blob, &m)
	return m
}

// decodeOutlineForList tolerates malformed outline JSON the same
// way. Empty outline → empty slice; the detail template shows
// "no sections recorded" rather than blowing up.
func decodeOutlineForList(blob []byte) []extractor.OutlineEntry {
	if len(blob) == 0 {
		return nil
	}
	var out []extractor.OutlineEntry
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil
	}
	return out
}

// truncateMsg caps a redirect-flash message so a stack-trace-
// shaped error doesn't blow past browser URL caps. The shared
// urlQueryEscape (artifacts.go) handles the escape itself.
func truncateMsg(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// humanAgo renders a duration like "2 hours ago" / "3 days ago" /
// "just now". Operator UI consistently displays both relative +
// absolute time (the absolute lives in title= tooltips) so
// scanning the list is fast but precise timestamps are one hover
// away. Kept local to documents.go because the rest of the UI
// uses a different relative-time format (older code) and we don't
// want to flicker between styles.
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 30*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d months ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%d years ago", int(d.Hours()/(24*365)))
	}
}
