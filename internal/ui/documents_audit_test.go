package ui

// Regression tests for the 2026-06 security audit finding: the
// extracted-documents UI handlers (list / detail / re-extract)
// authorized access only by the URL-embedded project id matching
// the row's project id — never by the *caller's* scope. A key
// scoped to project A could therefore list, read, or re-extract
// project B's documents by aiming the URL at B.
//
// Each test stamps a project-A scope on the request, drives the
// handler at a project-B URL, and asserts 404 (existence not
// leaked, mirroring the other UI IDOR guards). A same-project
// companion assertion proves the gate isn't always-fail.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// auditDocsRepo is a minimal in-test ExtractedDocumentRepository.
// Only the read paths exercised by the UI are wired; the rest
// return zero values so the interface is satisfied.
type auditDocsRepo struct {
	getDoc     *persistence.ExtractedDocument
	listDocs   []*persistence.ExtractedDocument
	listCalled *bool
	getCalled  *bool
}

func (m *auditDocsRepo) Upsert(_ context.Context, _ *persistence.ExtractedDocument) error {
	return nil
}

func (m *auditDocsRepo) Get(_ context.Context, _ string) (*persistence.ExtractedDocument, error) {
	if m.getCalled != nil {
		*m.getCalled = true
	}
	return m.getDoc, nil
}

func (m *auditDocsRepo) GetByArtifact(_ context.Context, _ string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}

func (m *auditDocsRepo) ListByProject(_ context.Context, _ string, _ int) ([]*persistence.ExtractedDocument, error) {
	if m.listCalled != nil {
		*m.listCalled = true
	}
	return m.listDocs, nil
}

func (m *auditDocsRepo) Delete(_ context.Context, _ string) error { return nil }

// auditDocB returns a project-B extracted document for the
// cross-project cases.
func auditDocB(id string) *persistence.ExtractedDocument {
	return &persistence.ExtractedDocument{
		ID:               id,
		ProjectID:        "B",
		SourceArtifactID: "art_b",
		ExtractorName:    "pdf",
		ExtractorVersion: "1",
		MimeType:         "application/pdf",
		Status:           "OK",
	}
}

// auditReExtractor records whether ReExtract fired so the test can
// detect a cross-project trigger even if it returns success.
type auditReExtractor struct {
	called *bool
}

func (a *auditReExtractor) ReExtract(_ context.Context, _, _ string) error {
	if a.called != nil {
		*a.called = true
	}
	return nil
}

// --- List ------------------------------------------------------------

func TestIDOR_ProjectDocuments_ScopedKeyCannotListForeign(t *testing.T) {
	listCalled := false
	repo := &auditDocsRepo{listDocs: []*persistence.ExtractedDocument{auditDocB("doc_b")}, listCalled: &listCalled}
	srv := NewServer(WithExtractedDocumentsRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/projects/B/documents", "A")
	rec := httptest.NewRecorder()
	srv.ProjectDocuments(rec, req, "B")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped documents list IDOR: status=%d, want 404 (existence-not-leaked)", rec.Code)
	}
	if listCalled {
		t.Errorf("scoped documents list IDOR: ListByProject fired against foreign project before scope check")
	}
}

func TestIDOR_ProjectDocuments_SameProjectStillWorks(t *testing.T) {
	repo := &auditDocsRepo{listDocs: []*persistence.ExtractedDocument{auditDocB("doc_b")}}
	srv := NewServer(WithExtractedDocumentsRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/projects/B/documents", "B")
	rec := httptest.NewRecorder()
	srv.ProjectDocuments(rec, req, "B")
	if rec.Code == http.StatusNotFound {
		t.Errorf("scoped key B listing B's documents: got 404, want 200; gate is over-blocking")
	}
}

// --- Detail ----------------------------------------------------------

func TestIDOR_ProjectDocumentDetail_ScopedKeyCannotReadForeign(t *testing.T) {
	getCalled := false
	repo := &auditDocsRepo{getDoc: auditDocB("doc_b"), getCalled: &getCalled}
	srv := NewServer(WithExtractedDocumentsRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/projects/B/documents/doc_b", "A")
	rec := httptest.NewRecorder()
	srv.ProjectDocumentDetail(rec, req, "B", "doc_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped document detail IDOR: status=%d, want 404", rec.Code)
	}
	if getCalled {
		t.Errorf("scoped document detail IDOR: repo Get fired against foreign project before scope check")
	}
}

func TestIDOR_ProjectDocumentDetail_SameProjectStillWorks(t *testing.T) {
	repo := &auditDocsRepo{getDoc: auditDocB("doc_b")}
	srv := NewServer(WithExtractedDocumentsRepository(repo))
	req := scopedRequest(http.MethodGet, "/ui/projects/B/documents/doc_b", "B")
	rec := httptest.NewRecorder()
	srv.ProjectDocumentDetail(rec, req, "B", "doc_b")
	if rec.Code == http.StatusNotFound {
		t.Errorf("scoped key B reading B's document: got 404, want 200; gate is over-blocking")
	}
}

// --- Re-extract ------------------------------------------------------

func TestIDOR_ProjectDocumentReExtract_ScopedKeyCannotTriggerForeign(t *testing.T) {
	reCalled := false
	repo := &auditDocsRepo{getDoc: auditDocB("doc_b")}
	srv := NewServer(
		WithExtractedDocumentsRepository(repo),
		WithDocumentReExtractor(&auditReExtractor{called: &reCalled}),
	)
	req := scopedRequest(http.MethodPost, "/ui/projects/B/documents/doc_b/re-extract", "A")
	rec := httptest.NewRecorder()
	srv.ProjectDocumentReExtract(rec, req, "B", "doc_b")
	if rec.Code != http.StatusNotFound {
		t.Errorf("scoped re-extract IDOR: status=%d, want 404", rec.Code)
	}
	if reCalled {
		t.Errorf("scoped re-extract IDOR: ReExtract fired against foreign project's artifact")
	}
}

func TestIDOR_ProjectDocumentReExtract_SameProjectStillWorks(t *testing.T) {
	reCalled := false
	repo := &auditDocsRepo{getDoc: auditDocB("doc_b")}
	srv := NewServer(
		WithExtractedDocumentsRepository(repo),
		WithDocumentReExtractor(&auditReExtractor{called: &reCalled}),
	)
	req := scopedRequest(http.MethodPost, "/ui/projects/B/documents/doc_b/re-extract", "B")
	rec := httptest.NewRecorder()
	srv.ProjectDocumentReExtract(rec, req, "B", "doc_b")
	if rec.Code == http.StatusNotFound {
		t.Errorf("scoped key B re-extracting B's document: got 404, want 303; gate is over-blocking")
	}
	if !reCalled {
		t.Errorf("scoped key B re-extracting B's document: ReExtract never fired; gate blocked a legitimate request")
	}
}
