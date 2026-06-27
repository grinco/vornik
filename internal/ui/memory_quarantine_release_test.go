// Package ui: tests for the quarantine release action — the
// counterpart to "drop" that was previously stubbed as "on the
// roadmap." Wired in commit-of-this-batch. Release calls
// MarkReleased(id, "") so the quarantine row exits the pending
// list; re-insertion stays an explicit corrector action.
package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubQuarantineRepo satisfies persistence.MemoryQuarantineRepository
// for the handler tests. Captures the action so the test can pin
// "release calls MarkReleased, drop calls MarkDropped".
type stubQuarantineRepo struct {
	getReturn    *persistence.MemoryQuarantineItem
	getErr       error
	releaseCalls []string
	releaseErr   error
	dropCalls    []string
	dropErr      error
}

func (s *stubQuarantineRepo) Insert(_ context.Context, _ *persistence.MemoryQuarantineItem) error {
	return nil
}
func (s *stubQuarantineRepo) ListPending(_ context.Context, _ string, _ int) ([]*persistence.MemoryQuarantineItem, error) {
	return nil, nil
}
func (s *stubQuarantineRepo) Get(_ context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getReturn == nil {
		return &persistence.MemoryQuarantineItem{ID: id, ProjectID: "janka"}, nil
	}
	return s.getReturn, nil
}
func (s *stubQuarantineRepo) MarkReleased(_ context.Context, id, _ string) error {
	s.releaseCalls = append(s.releaseCalls, id)
	return s.releaseErr
}
func (s *stubQuarantineRepo) MarkDropped(_ context.Context, id string) error {
	s.dropCalls = append(s.dropCalls, id)
	return s.dropErr
}
func (s *stubQuarantineRepo) CountByGate(_ context.Context, _ string) (map[string]int, error) {
	return nil, nil
}

// TestMemoryQuarantineAction_ReleaseCallsMarkReleased — the
// behaviour the batch unlocks. action=release MUST hit
// MarkReleased (with empty released_chunk_id per the design note)
// — pre-batch this branch errored as "drop only."
func TestMemoryQuarantineAction_ReleaseCallsMarkReleased(t *testing.T) {
	repo := &stubQuarantineRepo{}
	srv := NewServer(WithMemoryQuarantineRepository(repo))

	form := url.Values{}
	form.Set("id", "q_42")
	form.Set("action", "release")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/quarantine", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "janka")

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	if len(repo.releaseCalls) != 1 || repo.releaseCalls[0] != "q_42" {
		t.Errorf("MarkReleased calls = %v; want [q_42]", repo.releaseCalls)
	}
	if len(repo.dropCalls) != 0 {
		t.Errorf("MarkDropped must NOT have been called for action=release; saw %v", repo.dropCalls)
	}
}

// TestMemoryQuarantineAction_DropStillWorks — drop must still work
// unchanged. Pins the symmetric path so the release wiring doesn't
// silently break the existing operator flow.
func TestMemoryQuarantineAction_DropStillWorks(t *testing.T) {
	repo := &stubQuarantineRepo{}
	srv := NewServer(WithMemoryQuarantineRepository(repo))

	form := url.Values{}
	form.Set("id", "q_99")
	form.Set("action", "drop")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/quarantine", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "janka")

	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	if len(repo.dropCalls) != 1 || repo.dropCalls[0] != "q_99" {
		t.Errorf("MarkDropped calls = %v; want [q_99]", repo.dropCalls)
	}
}

// TestMemoryQuarantineAction_ReleaseError — backend failure must
// surface as 500 with the error in the body, mirroring the drop
// path's error handling.
func TestMemoryQuarantineAction_ReleaseError(t *testing.T) {
	repo := &stubQuarantineRepo{releaseErr: errors.New("simulated DB outage")}
	srv := NewServer(WithMemoryQuarantineRepository(repo))

	form := url.Values{}
	form.Set("id", "q_1")
	form.Set("action", "release")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/quarantine", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "janka")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("release error: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "simulated DB outage") {
		t.Errorf("error body lost; got %q", rec.Body.String())
	}
}

// TestMemoryQuarantineAction_UnknownAction — defensive: any value
// other than drop|release must 400. Catches typos and any future
// regression that tries to widen the accepted actions without
// updating the handler.
func TestMemoryQuarantineAction_UnknownAction(t *testing.T) {
	repo := &stubQuarantineRepo{}
	srv := NewServer(WithMemoryQuarantineRepository(repo))

	form := url.Values{}
	form.Set("id", "q_1")
	form.Set("action", "release-with-overrides") // not a real action
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/quarantine", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "janka")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action: got %d, want 400", rec.Code)
	}
}
