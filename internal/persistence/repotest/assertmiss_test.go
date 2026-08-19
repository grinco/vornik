package repotest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// recordingTB captures what AssertMiss would have reported, so the helper's
// own failure path is testable without failing the enclosing test.
type recordingTB struct {
	failures []string
	helpers  int
}

func (r *recordingTB) Helper() { r.helpers++ }

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestAssertMiss_passes_a_conforming_lookup(t *testing.T) {
	rec := &recordingTB{}
	AssertMiss(rec, "ExtractedDocumentRepository.Get", func() (*persistence.ExtractedDocument, error) {
		return nil, persistence.ErrNotFound
	})
	if len(rec.failures) != 0 {
		t.Fatalf("conforming lookup reported: %v", rec.failures)
	}
	if rec.helpers == 0 {
		t.Error("AssertMiss must mark itself a helper so failures point at the caller")
	}
}

// Regression: 2026-08-19. A double stricter or looser than production is
// exactly what certified the document-ingest nil dereference as covered.
func TestAssertMiss_fails_a_permissive_lookup(t *testing.T) {
	rec := &recordingTB{}
	AssertMiss(rec, "ExtractedDocumentRepository.Get", func() (*persistence.ExtractedDocument, error) {
		return nil, nil
	})
	if len(rec.failures) != 1 {
		t.Fatalf("want 1 failure, got %d: %v", len(rec.failures), rec.failures)
	}
	if !strings.Contains(rec.failures[0], "ExtractedDocumentRepository.Get") {
		t.Errorf("failure does not name the key: %q", rec.failures[0])
	}
}

func TestAssertMiss_fails_an_unregistered_key(t *testing.T) {
	rec := &recordingTB{}
	AssertMiss(rec, "MadeUpRepository.Get", func() (*persistence.ExtractedDocument, error) {
		return nil, persistence.ErrNotFound
	})
	if len(rec.failures) != 1 {
		t.Fatalf("want 1 failure for an unregistered key, got %v", rec.failures)
	}
}

// A double that hands back a value for an id it was never given is the other
// half of the same defect: the test then exercises a row production would
// not have produced.
func TestAssertMiss_fails_a_lookup_that_returns_a_value(t *testing.T) {
	rec := &recordingTB{}
	AssertMiss(rec, "ExtractedDocumentRepository.Get", func() (*persistence.ExtractedDocument, error) {
		return &persistence.ExtractedDocument{ID: "phantom"}, nil
	})
	if len(rec.failures) != 1 {
		t.Fatalf("want 1 failure, got %v", rec.failures)
	}
}

// AssertMissRepo is the shape suites use: it drives the real method with an
// id that cannot exist, so the suite proves the backend's own miss path
// rather than a hand-built pair.
func TestAssertMissRepo_drives_the_method_with_an_absent_id(t *testing.T) {
	var asked string
	rec := &recordingTB{}
	AssertMissRepo(rec, "ExtractedDocumentRepository.Get", func(_ context.Context, id string) (*persistence.ExtractedDocument, error) {
		asked = id
		return nil, persistence.ErrNotFound
	})
	if len(rec.failures) != 0 {
		t.Fatalf("conforming repo reported: %v", rec.failures)
	}
	if asked == "" {
		t.Fatal("AssertMissRepo did not call the lookup")
	}
	if !strings.Contains(asked, "absent") {
		t.Errorf("the probe id %q should say what it is, so a stray row is diagnosable", asked)
	}
}

func TestAssertMissRepo_uses_a_distinct_id_per_call(t *testing.T) {
	var ids []string
	rec := &recordingTB{}
	probe := func(_ context.Context, id string) (*persistence.ExtractedDocument, error) {
		ids = append(ids, id)
		return nil, persistence.ErrNotFound
	}
	AssertMissRepo(rec, "ExtractedDocumentRepository.Get", probe)
	AssertMissRepo(rec, "ExtractedDocumentRepository.Get", probe)
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("probe ids must be unique across calls, got %v", ids)
	}
}
