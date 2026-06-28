// Package ui: hermetic tests for the UI memory mutation surface
// (MemoryRollbackAction / MemoryQuarantineAction).
package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Stub repos with the minimum methods these handlers need.

type uiStubEpochRepo struct {
	persistence.CorpusEpochRepository
	rollbackFn      func(ctx context.Context, projectID, target, by, reason string) (int, int, int, error)
	listEpochsFn    func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error)
	listRollbacksFn func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error)
}

func (s *uiStubEpochRepo) RollbackTo(ctx context.Context, projectID, target, by, reason string) (int, int, int, error) {
	if s.rollbackFn != nil {
		return s.rollbackFn(ctx, projectID, target, by, reason)
	}
	return 0, 0, 0, nil
}

func (s *uiStubEpochRepo) CountRollbackRestorable(context.Context, string, string) (int, int, error) {
	return 0, 0, nil
}

func (s *uiStubEpochRepo) Deactivate(context.Context, string, string, string) error {
	return nil
}

func (s *uiStubEpochRepo) ListEpochs(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
	if s.listEpochsFn != nil {
		return s.listEpochsFn(ctx, projectID, limit)
	}
	return nil, nil
}

func (s *uiStubEpochRepo) ListRollbacks(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error) {
	if s.listRollbacksFn != nil {
		return s.listRollbacksFn(ctx, projectID, limit)
	}
	return nil, nil
}

type uiStubQuarantineRepo struct {
	persistence.MemoryQuarantineRepository
	getFn         func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error)
	markDroppedFn func(ctx context.Context, id string) error
	listPendingFn func(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error)
	countByGateFn func(ctx context.Context, projectID string) (map[string]int, error)
}

func (s *uiStubQuarantineRepo) CountByGate(ctx context.Context, projectID string) (map[string]int, error) {
	if s.countByGateFn != nil {
		return s.countByGateFn(ctx, projectID)
	}
	return map[string]int{}, nil
}

func (s *uiStubQuarantineRepo) Get(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
	if s.getFn != nil {
		return s.getFn(ctx, id)
	}
	return nil, errors.New("not configured")
}
func (s *uiStubQuarantineRepo) MarkDropped(ctx context.Context, id string) error {
	if s.markDroppedFn != nil {
		return s.markDroppedFn(ctx, id)
	}
	return nil
}
func (s *uiStubQuarantineRepo) ListPending(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
	if s.listPendingFn != nil {
		return s.listPendingFn(ctx, projectID, limit)
	}
	return nil, nil
}

type uiStubIngestQueueRepo struct {
	persistence.IngestQueueRepository
	queueDepthFn func(ctx context.Context, projectID string) (int, error)
}

func (s *uiStubIngestQueueRepo) QueueDepth(ctx context.Context, projectID string) (int, error) {
	if s.queueDepthFn != nil {
		return s.queueDepthFn(ctx, projectID)
	}
	return 0, nil
}

// --- MemoryRollbackAction --------------------------------------------

func TestUIMemoryRollbackAction_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1/rollback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryRollbackAction(rec, req, "p1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryRollbackAction_NotEnabled(t *testing.T) {
	srv := NewServer()
	form := url.Values{"to": []string{"e1"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/rollback",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryRollbackAction(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryRollbackAction_MissingTarget(t *testing.T) {
	srv := NewServer(WithCorpusEpochRepository(&uiStubEpochRepo{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/rollback", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryRollbackAction(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryRollbackAction_RollbackError(t *testing.T) {
	srv := NewServer(WithCorpusEpochRepository(&uiStubEpochRepo{
		rollbackFn: func(_ context.Context, _, _, _, _ string) (int, int, int, error) {
			return 0, 0, 0, errors.New("db error")
		},
	}))
	form := url.Values{"to": []string{"e1"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/rollback",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryRollbackAction(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryRollbackAction_Success(t *testing.T) {
	gotBy := ""
	srv := NewServer(WithCorpusEpochRepository(&uiStubEpochRepo{
		rollbackFn: func(_ context.Context, _, _, by, _ string) (int, int, int, error) {
			gotBy = by
			return 5, 1, 0, nil
		},
	}))
	form := url.Values{"to": []string{"e1"}, "reason": []string{"drill"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/rollback",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-User", "alice")
	rec := httptest.NewRecorder()
	srv.MemoryRollbackAction(rec, req, "p1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotBy != "ui:alice" {
		t.Errorf("by: got %q, want ui:alice", gotBy)
	}
	if !strings.Contains(rec.Header().Get("Location"), "/ui/memory/p1") {
		t.Errorf("location: %q", rec.Header().Get("Location"))
	}
}

// --- MemoryQuarantineAction ------------------------------------------

func TestUIMemoryQuarantineAction_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1/quarantine", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_NotEnabled(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_MissingFields(t *testing.T) {
	srv := NewServer(WithMemoryQuarantineRepository(&uiStubQuarantineRepo{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_NotFound(t *testing.T) {
	srv := NewServer(WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
		getFn: func(_ context.Context, _ string) (*persistence.MemoryQuarantineItem, error) {
			return nil, errors.New("not found")
		},
	}))
	form := url.Values{"id": []string{"q1"}, "action": []string{"drop"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_ProjectMismatch(t *testing.T) {
	srv := NewServer(WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
		getFn: func(_ context.Context, _ string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: "q1", ProjectID: "other"}, nil
		},
	}))
	form := url.Values{"id": []string{"q1"}, "action": []string{"drop"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_UnknownAction(t *testing.T) {
	srv := NewServer(WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
		getFn: func(_ context.Context, _ string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: "q1", ProjectID: "p1"}, nil
		},
	}))
	form := url.Values{"id": []string{"q1"}, "action": []string{"banana"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_DropError(t *testing.T) {
	srv := NewServer(WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
		getFn: func(_ context.Context, _ string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: "q1", ProjectID: "p1"}, nil
		},
		markDroppedFn: func(_ context.Context, _ string) error {
			return errors.New("db down")
		},
	}))
	form := url.Values{"id": []string{"q1"}, "action": []string{"drop"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestUIMemoryQuarantineAction_DropSuccess(t *testing.T) {
	dropped := false
	srv := NewServer(WithMemoryQuarantineRepository(&uiStubQuarantineRepo{
		getFn: func(_ context.Context, _ string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: "q1", ProjectID: "p1"}, nil
		},
		markDroppedFn: func(_ context.Context, _ string) error {
			dropped = true
			return nil
		},
	}))
	form := url.Values{"id": []string{"q1"}, "action": []string{"drop"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/quarantine",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !dropped {
		t.Errorf("MarkDropped not called")
	}
	if !strings.Contains(rec.Header().Get("Location"), "/ui/memory/p1") {
		t.Errorf("location: %q", rec.Header().Get("Location"))
	}
}

// --- MemoryProject ---------------------------------------------------

func TestMemoryProject_MissingID(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemoryProject_RendersWithoutHardening(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "p1") {
		t.Errorf("expected project id in body: %s", rec.Body.String())
	}
}

// TestMemoryProject_PerTablePageSizeLimitsRideThrough — each panel's
// "Show" selector posts an independent ?<table>_limit=N param. The
// handler must parse all four and pass each to the matching repo
// call so the operator can deep-dive one table without bloating the
// others. Verifies the wiring end-to-end via the limit values the
// stubs see.
func TestMemoryProject_PerTablePageSizeLimitsRideThrough(t *testing.T) {
	var qLimit, eLimit, rLimit int
	quarantine := &uiStubQuarantineRepo{
		listPendingFn: func(_ context.Context, _ string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
			qLimit = limit
			return nil, nil
		},
	}
	epochs := &uiStubEpochRepo{
		listEpochsFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusEpoch, error) {
			eLimit = limit
			return nil, nil
		},
		listRollbacksFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusRollback, error) {
			rLimit = limit
			return nil, nil
		},
	}
	srv := NewServer(
		WithMemoryQuarantineRepository(quarantine),
		WithCorpusEpochRepository(epochs),
	)
	req := httptest.NewRequest(http.MethodGet,
		"/ui/memory/p1?quarantine_limit=100&epochs_limit=50&rollbacks_limit=10", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if qLimit != 100 {
		t.Errorf("quarantine_limit: got %d, want 100", qLimit)
	}
	if eLimit != 50 {
		t.Errorf("epochs_limit: got %d, want 50", eLimit)
	}
	if rLimit != 10 {
		t.Errorf("rollbacks_limit: got %d, want 10", rLimit)
	}
}

// TestMemoryProject_PageSizeLimitsDefaultAndRejectInvalid — empty,
// missing, and invalid values must fall back to their respective
// defaults. Quarantine/rollbacks use parsePageSize (DefaultPageSize=20);
// epochs use parseEpochsLimit (DefaultEpochsLimit=500) so that an
// operator with >100 snapshots sees all of them in the rollback picker.
// Pins the safety belt that prevents crafted query params from forcing
// giant repo scans.
func TestMemoryProject_PageSizeLimitsDefaultAndRejectInvalid(t *testing.T) {
	var qLimit, eLimit, rLimit int
	quarantine := &uiStubQuarantineRepo{
		listPendingFn: func(_ context.Context, _ string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
			qLimit = limit
			return nil, nil
		},
	}
	epochs := &uiStubEpochRepo{
		listEpochsFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusEpoch, error) {
			eLimit = limit
			return nil, nil
		},
		listRollbacksFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusRollback, error) {
			rLimit = limit
			return nil, nil
		},
	}
	srv := NewServer(
		WithMemoryQuarantineRepository(quarantine),
		WithCorpusEpochRepository(epochs),
	)
	// Missing q-param, garbage value, out-of-ceiling value.
	req := httptest.NewRequest(http.MethodGet,
		"/ui/memory/p1?quarantine_limit=abc&epochs_limit=99999999", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if qLimit != DefaultPageSize {
		t.Errorf("garbage q-param: got %d, want default %d", qLimit, DefaultPageSize)
	}
	// epochs_limit=99999999 exceeds MaxEpochsLimit (500); parseEpochsLimit
	// falls back to DefaultEpochsLimit (500) — not DefaultPageSize (20).
	if eLimit != DefaultEpochsLimit {
		t.Errorf("out-of-ceiling epochs_limit: got %d, want DefaultEpochsLimit %d", eLimit, DefaultEpochsLimit)
	}
	if rLimit != DefaultPageSize {
		t.Errorf("missing rollbacks param: got %d, want default %d", rLimit, DefaultPageSize)
	}
}

// TestMemoryProject_RollbackPickerExposesAllEpochs — regression guard for the
// epoch-rollback-cap bug: an operator with >100 corpus snapshots could only
// select from the most recent 100 in the rollback picker because
// parsePageSize rejects any value outside PageSizeOptions (max 100).
//
// The fix: MemoryProject uses parseEpochsLimit (not parsePageSize) for
// epochs_limit — accepts up to 500, matching the API ceiling. When no
// epochs_limit param is present the default is 500 so ALL epochs appear in
// the rollback picker by default.
//
// This test:
//  1. Stubs a corpus-epoch repo that returns 200 epochs when asked for ≥200.
//  2. Hits MemoryProject with no ?epochs_limit= (default path).
//  3. Asserts the Epochs slice on the rendered page includes epoch #150
//     (which would be absent if the limit stayed at 20/100).
func TestMemoryProject_RollbackPickerExposesAllEpochs(t *testing.T) {
	const totalEpochs = 200

	epochs := &uiStubEpochRepo{
		listEpochsFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusEpoch, error) {
			// Return up to `limit` epochs; IDs are "epoch-001" … "epoch-200".
			n := limit
			if n > totalEpochs {
				n = totalEpochs
			}
			out := make([]*persistence.CorpusEpoch, n)
			for i := range out {
				id := fmt.Sprintf("epoch-%03d", i+1)
				out[i] = &persistence.CorpusEpoch{ID: id, IsActive: i == 0}
			}
			return out, nil
		},
	}

	srv := NewServer(WithCorpusEpochRepository(epochs))
	// No epochs_limit param — the handler must default to something ≥200
	// so all 200 epochs come back.
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// epoch-150 would be absent if the effective limit were ≤100.
	if !strings.Contains(body, "epoch-150") {
		t.Errorf("rollback picker cap bug: epoch-150 not found in page body — limit is still ≤100. Body excerpt: %.600s", body)
	}
	// Sanity: epoch-001 must also appear.
	if !strings.Contains(body, "epoch-001") {
		t.Errorf("epoch-001 missing from body — epochs not rendered at all. Body excerpt: %.600s", body)
	}
}

// TestMemoryProject_RollbackPickerHonorsExplicitHighLimit — operator can pass
// ?epochs_limit=500 (the API ceiling) and the picker exposes all 200 epochs.
func TestMemoryProject_RollbackPickerHonorsExplicitHighLimit(t *testing.T) {
	const totalEpochs = 200

	epochs := &uiStubEpochRepo{
		listEpochsFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusEpoch, error) {
			n := limit
			if n > totalEpochs {
				n = totalEpochs
			}
			out := make([]*persistence.CorpusEpoch, n)
			for i := range out {
				id := fmt.Sprintf("epoch-%03d", i+1)
				out[i] = &persistence.CorpusEpoch{ID: id, IsActive: i == 0}
			}
			return out, nil
		},
	}

	srv := NewServer(WithCorpusEpochRepository(epochs))
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1?epochs_limit=500", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "epoch-150") {
		t.Errorf("explicit epochs_limit=500 didn't surface epoch-150")
	}
}

// TestMemoryProject_RollbackPickerRejectsOverCeiling — ?epochs_limit values
// above 500 must fall back to the default (500), not blow up.
func TestMemoryProject_RollbackPickerRejectsOverCeiling(t *testing.T) {
	var gotLimit int
	epochs := &uiStubEpochRepo{
		listEpochsFn: func(_ context.Context, _ string, limit int) ([]*persistence.CorpusEpoch, error) {
			gotLimit = limit
			return nil, nil
		},
	}

	srv := NewServer(WithCorpusEpochRepository(epochs))
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1?epochs_limit=99999", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	const wantDefault = 500
	if gotLimit != wantDefault {
		t.Errorf("epochs_limit=99999 should clamp to default %d, got %d", wantDefault, gotLimit)
	}
}

// TestMemoryProject_RendersPerTableSelectors — confirms each panel
// header carries its own "Show" selector posting the right param
// name. The operate tab is the one that renders the four
// quarantine/epoch/rollback/eviction tables, so the test pins ?tab=
// to force that branch on. Render assertion is by substring rather
// than DOM walk; the canonical selector partial is already covered
// by its own test.
func TestMemoryProject_RendersPerTableSelectors(t *testing.T) {
	srv := NewServer(
		WithMemoryQuarantineRepository(&uiStubQuarantineRepo{}),
		WithCorpusEpochRepository(&uiStubEpochRepo{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1?tab=operate", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="quarantine_limit"`,
		`name="epochs_limit"`,
		`name="rollbacks_limit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}
