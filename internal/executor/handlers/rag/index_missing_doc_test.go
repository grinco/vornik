package rag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// nilOnMissDocRepo reproduces the REAL repository's not-found contract.
//
// This is the whole reason the crash shipped. postgres.scanExtractedDocument
// returns (nil, nil) on sql.ErrNoRows, but fakeExtractedDocRepo in rag_test.go
// returns an error instead — so every existing test took the err != nil branch
// and nothing ever reached the line that dereferences a nil doc. A test double
// that is stricter than production is not a safe double.
type nilOnMissDocRepo struct {
	persistence.ExtractedDocumentRepository
}

func (nilOnMissDocRepo) Get(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil // exactly what Postgres returns for a row that is not there
}

type noopIngester struct{}

func (noopIngester) IngestExtractedSections(
	_ context.Context,
	_, _, _, _ string,
	_ []memory.ExtractedSection,
) (int, error) {
	return 0, nil
}

func (noopIngester) PatchScopeByArtifact(context.Context, string, string, string) error {
	return nil
}

// Regression: 2026-08-19. The first run of the new document-ingest tripwire
// segfaulted the bench daemon and it crash-looped 28 times in ten minutes.
//
// panic: runtime error: invalid memory address or nil pointer dereference
//
//	rag.(*IndexHandler).Execute ... handlers/rag/index.go:135
//
// The extract step recorded an ExtractedDocumentID that Get could not find,
// Get returned (nil, nil) per its not-found contract, the err != nil branch did
// not fire, and `doc.StoragePath` dereferenced nil. A missing row is a data
// condition the step should FAIL on with something an operator can read — never
// a daemon crash.
func TestIndexHandler_MissingExtractedDoc_FailsInsteadOfPanicking(t *testing.T) {
	h := NewIndexHandler(nilOnMissDocRepo{}, noopIngester{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Execute panicked on a not-found extracted document (%v) — this took "+
				"the whole daemon down 28 times on 2026-08-19", r)
		}
	}()

	prev, _ := json.Marshal(map[string]any{
		"extracted": []map[string]any{
			{"artifact_id": "art_1", "extracted_document_id": "missing-doc-id"},
		},
	})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "task_1", ProjectID: "p1"},
		Execution:  &persistence.Execution{ID: "exec_1"},
		StepID:     "index",
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: prev,
	})
	if err == nil {
		t.Fatal("expected an error naming the missing document, got nil")
	}
	if !strings.Contains(err.Error(), "missing-doc-id") {
		t.Errorf("error must name the document that could not be loaded so an operator "+
			"can find it; got: %v", err)
	}
}
