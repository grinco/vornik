// Package ui: tests for the Memory index page (GET /ui/memory).
// Drives the page-render path with a populated registry + stubbed
// hardening repos.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestMemory_NoRegistry(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/memory", nil)
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestMemory_RendersWithoutHardening(t *testing.T) {
	srv := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	req := httptest.NewRequest(http.MethodGet, "/ui/memory", nil)
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "project-1") {
		t.Errorf("expected project row; got %s", rec.Body.String())
	}
}

// TestMemory_QuarantinePendingUsesUnboundedCount pins #7: the
// memory landing page must source its per-project quarantine
// tile from the live `CountByGate` map (unbounded) — NOT from
// `len(ListPending(200))`, which silently caps the displayed
// count at the page size. A project saturated at >200 pending
// rows used to under-report; the fix sums CountByGate so the
// tile, the alert widget on the project page, and the
// quarantine list all agree.
func TestMemory_QuarantinePendingUsesUnboundedCount(t *testing.T) {
	srv := NewServer(
		WithProjectRegistry(buildPopulatedUIRegistry(t)),
		WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
			// ListPending returns the page-bounded slice; if the
			// handler still uses len(items) the body will say 200,
			// not 550.
			listPendingFn: func(_ context.Context, _ string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
				out := make([]*persistence.MemoryQuarantineItem, limit)
				for i := range out {
					out[i] = &persistence.MemoryQuarantineItem{ID: "q-saturated"}
				}
				return out, nil
			},
			countByGateFn: func(_ context.Context, _ string) (map[string]int, error) {
				return map[string]int{"secret_scan": 500, "dedup_hash": 50}, nil
			},
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/ui/memory?limit=10", nil)
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The page renders the quarantine count next to each project
	// row. We want the unbounded sum (550) to surface — the bounded
	// slice length (200) would be the regression we're guarding
	// against.
	if !strings.Contains(body, "550") {
		t.Errorf("expected quarantine count 550 (CountByGate sum) in body; if 200 appears instead the handler regressed to len(ListPending). body excerpt: %.500s", body)
	}
	if strings.Contains(body, ">200<") {
		t.Errorf("body still surfaces bounded len(ListPending)=200; #7 fix regressed. body excerpt: %.500s", body)
	}
}

func TestMemory_RendersWithStubbedHardening(t *testing.T) {
	closed := time.Now().Add(-time.Hour)
	srv := NewServer(
		WithProjectRegistry(buildPopulatedUIRegistry(t)),
		WithCorpusEpochRepository(&uiStubEpochRepo{
			listEpochsFn: func(_ context.Context, _ string, _ int) ([]*persistence.CorpusEpoch, error) {
				return []*persistence.CorpusEpoch{
					{ID: "e1", IsActive: true, ClosedAt: &closed},
					{ID: "e2", IsActive: false},
				}, nil
			},
		}),
		WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
			listPendingFn: func(_ context.Context, _ string, _ int) ([]*persistence.MemoryQuarantineItem, error) {
				return []*persistence.MemoryQuarantineItem{{ID: "q1"}}, nil
			},
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/ui/memory?limit=10", nil)
	rec := httptest.NewRecorder()
	srv.Memory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
}
