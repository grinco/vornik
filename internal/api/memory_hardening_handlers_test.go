// Package api: HTTP-handler tests for the memory-hardening surface
// (epochs / rollback / quarantine / health). All tests are hermetic —
// they wire stub persistence implementations into a Server and drive
// the handlers through httptest. No DB, no network.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// sqlErrNoRows aliases sql.ErrNoRows so handler tests can return the
// canonical "row not found" sentinel without importing database/sql in
// every test case.
var sqlErrNoRows = sql.ErrNoRows

// --- stub repositories ------------------------------------------------

type stubCorpusEpochRepo struct {
	persistence.CorpusEpochRepository // satisfy interface via embedding

	listEpochsFn    func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error)
	getEpochFn      func(ctx context.Context, epochID string) (*persistence.CorpusEpoch, error)
	rollbackToFn    func(ctx context.Context, projectID, target, by, reason string) (int, int, int, error)
	listRollbacksFn func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error)
}

func (s *stubCorpusEpochRepo) ListEpochs(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
	if s.listEpochsFn != nil {
		return s.listEpochsFn(ctx, projectID, limit)
	}
	return nil, nil
}

func (s *stubCorpusEpochRepo) GetEpoch(ctx context.Context, epochID string) (*persistence.CorpusEpoch, error) {
	if s.getEpochFn != nil {
		return s.getEpochFn(ctx, epochID)
	}
	return nil, errors.New("not configured")
}

func (s *stubCorpusEpochRepo) RollbackTo(ctx context.Context, projectID, target, by, reason string) (int, int, int, error) {
	if s.rollbackToFn != nil {
		return s.rollbackToFn(ctx, projectID, target, by, reason)
	}
	return 0, 0, 0, nil
}

func (s *stubCorpusEpochRepo) CountRollbackRestorable(context.Context, string, string) (int, int, error) {
	return 0, 0, nil
}

func (s *stubCorpusEpochRepo) Deactivate(context.Context, string, string, string) error {
	return nil
}

func (s *stubCorpusEpochRepo) ListRollbacks(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error) {
	if s.listRollbacksFn != nil {
		return s.listRollbacksFn(ctx, projectID, limit)
	}
	return nil, nil
}

type stubMemoryQuarantineRepo struct {
	persistence.MemoryQuarantineRepository

	listPendingFn func(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error)
	countByGateFn func(ctx context.Context, projectID string) (map[string]int, error)
	getFn         func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error)
	markDroppedFn func(ctx context.Context, id string) error
}

func (s *stubMemoryQuarantineRepo) ListPending(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
	if s.listPendingFn != nil {
		return s.listPendingFn(ctx, projectID, limit)
	}
	return nil, nil
}
func (s *stubMemoryQuarantineRepo) CountByGate(ctx context.Context, projectID string) (map[string]int, error) {
	if s.countByGateFn != nil {
		return s.countByGateFn(ctx, projectID)
	}
	return map[string]int{}, nil
}
func (s *stubMemoryQuarantineRepo) Get(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
	if s.getFn != nil {
		return s.getFn(ctx, id)
	}
	return nil, errors.New("not configured")
}
func (s *stubMemoryQuarantineRepo) MarkDropped(ctx context.Context, id string) error {
	if s.markDroppedFn != nil {
		return s.markDroppedFn(ctx, id)
	}
	return nil
}

type stubIngestQueueRepo struct {
	persistence.IngestQueueRepository

	queueDepthFn func(ctx context.Context, projectID string) (int, error)
}

func (s *stubIngestQueueRepo) QueueDepth(ctx context.Context, projectID string) (int, error) {
	if s.queueDepthFn != nil {
		return s.queueDepthFn(ctx, projectID)
	}
	return 0, nil
}

// --- MemoryEpochs ----------------------------------------------------

func TestMemoryEpochs_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/epochs", nil)
	rec := httptest.NewRecorder()
	srv.MemoryEpochs(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "MEMORY_HARDENING_DISABLED") {
		t.Errorf("body missing error code: %s", rec.Body.String())
	}
}

func TestMemoryEpochs_ListError(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		listEpochsFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
			return nil, errors.New("boom")
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/epochs", nil)
	rec := httptest.NewRecorder()
	srv.MemoryEpochs(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "EPOCHS_ERROR") {
		t.Errorf("missing error code: %s", rec.Body.String())
	}
}

func TestMemoryEpochs_Success_DefaultAndCustomLimit(t *testing.T) {
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		url       string
		wantLimit int
	}{
		{"default-limit", "/api/v1/projects/p1/memory/epochs", 50},
		{"custom-limit", "/api/v1/projects/p1/memory/epochs?limit=10", 10},
		{"limit-zero-falls-back", "/api/v1/projects/p1/memory/epochs?limit=0", 50},
		{"limit-too-large-falls-back", "/api/v1/projects/p1/memory/epochs?limit=10000", 50},
		{"limit-non-numeric-falls-back", "/api/v1/projects/p1/memory/epochs?limit=banana", 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLimit := 0
			srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
				listEpochsFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
					gotLimit = limit
					return []*persistence.CorpusEpoch{{ID: "e1", ProjectID: projectID, CreatedAt: now, IsActive: true}}, nil
				},
			}}
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			rec := httptest.NewRecorder()
			srv.MemoryEpochs(rec, req, "p1")
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if gotLimit != tc.wantLimit {
				t.Errorf("limit: got %d, want %d", gotLimit, tc.wantLimit)
			}
			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["total"].(float64) != 1 {
				t.Errorf("total: got %v, want 1", body["total"])
			}
		})
	}
}

// --- MemoryRollback ---------------------------------------------------

func TestMemoryRollback_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestMemoryRollback_InvalidJSON(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Errorf("missing VALIDATION_ERROR: %s", rec.Body.String())
	}
}

func TestMemoryRollback_MissingTarget(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback", strings.NewReader(`{"reason":"x"}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestMemoryRollback_TargetNotFound(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		getEpochFn: func(ctx context.Context, id string) (*persistence.CorpusEpoch, error) {
			return nil, errors.New("no such epoch")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback",
		strings.NewReader(`{"target_epoch_id":"missing"}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestMemoryRollback_ProjectMismatch(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		getEpochFn: func(ctx context.Context, id string) (*persistence.CorpusEpoch, error) {
			return &persistence.CorpusEpoch{ID: id, ProjectID: "other-project"}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback",
		strings.NewReader(`{"target_epoch_id":"e1"}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "EPOCH_PROJECT_MISMATCH") {
		t.Errorf("missing mismatch code: %s", rec.Body.String())
	}
}

func TestMemoryRollback_DryRun_ComputesPlan(t *testing.T) {
	target := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	later := target.Add(24 * time.Hour)
	earlier := target.Add(-24 * time.Hour)
	closed := earlier.Add(time.Hour)

	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		getEpochFn: func(ctx context.Context, id string) (*persistence.CorpusEpoch, error) {
			return &persistence.CorpusEpoch{ID: id, ProjectID: "p1", CreatedAt: target, IsActive: true}, nil
		},
		listEpochsFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
			// 1 epoch after target (active → would deactivate)
			// 1 epoch before target that's closed+inactive (would reactivate)
			// 1 epoch == target (no-op)
			return []*persistence.CorpusEpoch{
				{ID: "later", ProjectID: "p1", CreatedAt: later, IsActive: true},
				{ID: "target", ProjectID: "p1", CreatedAt: target, IsActive: true},
				{ID: "older-closed", ProjectID: "p1", CreatedAt: earlier, ClosedAt: &closed, IsActive: false},
			}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback",
		strings.NewReader(`{"target_epoch_id":"target","apply":false}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["wouldDeactivate"].(float64) != 1 {
		t.Errorf("wouldDeactivate: got %v, want 1", body["wouldDeactivate"])
	}
	if body["wouldReactivate"].(float64) != 1 {
		t.Errorf("wouldReactivate: got %v, want 1", body["wouldReactivate"])
	}
	if body["applied"].(bool) != false {
		t.Errorf("applied: got %v, want false", body["applied"])
	}
}

func TestMemoryRollback_Apply_RollbackError(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		getEpochFn: func(ctx context.Context, id string) (*persistence.CorpusEpoch, error) {
			return &persistence.CorpusEpoch{ID: id, ProjectID: "p1", CreatedAt: time.Now().UTC()}, nil
		},
		rollbackToFn: func(ctx context.Context, projectID, target, by, reason string) (int, int, int, error) {
			return 0, 0, 0, errors.New("db down")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback",
		strings.NewReader(`{"target_epoch_id":"e1","apply":true,"reason":"emergency"}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestMemoryRollback_Apply_Success_DefaultsTriggeredBy(t *testing.T) {
	gotBy := ""
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		getEpochFn: func(ctx context.Context, id string) (*persistence.CorpusEpoch, error) {
			return &persistence.CorpusEpoch{ID: id, ProjectID: "p1", CreatedAt: time.Now().UTC()}, nil
		},
		rollbackToFn: func(ctx context.Context, projectID, target, by, reason string) (int, int, int, error) {
			gotBy = by
			return 3, 1, 2, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback",
		strings.NewReader(`{"target_epoch_id":"e1","apply":true}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotBy != "system" {
		t.Errorf("default triggeredBy: got %q, want %q", gotBy, "system")
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["actuallyDeactivated"].(float64) != 3 || body["actuallyReactivated"].(float64) != 1 {
		t.Errorf("counts: %v", body)
	}
}

func TestMemoryRollback_ListEpochsError(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		getEpochFn: func(ctx context.Context, id string) (*persistence.CorpusEpoch, error) {
			return &persistence.CorpusEpoch{ID: id, ProjectID: "p1", CreatedAt: time.Now().UTC()}, nil
		},
		listEpochsFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
			return nil, errors.New("list failed")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/rollback",
		strings.NewReader(`{"target_epoch_id":"e1"}`))
	rec := httptest.NewRecorder()
	srv.MemoryRollback(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// --- MemoryRollbacks --------------------------------------------------

func TestMemoryRollbacks_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/rollbacks", nil)
	rec := httptest.NewRecorder()
	srv.MemoryRollbacks(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemoryRollbacks_ListError(t *testing.T) {
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		listRollbacksFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error) {
			return nil, errors.New("oops")
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/rollbacks", nil)
	rec := httptest.NewRecorder()
	srv.MemoryRollbacks(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemoryRollbacks_Success(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	from := "epoch-a"
	to := "epoch-b"
	reason := "data poison drill"
	srv := &Server{corpusEpochs: &stubCorpusEpochRepo{
		listRollbacksFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusRollback, error) {
			if limit != 25 {
				t.Errorf("limit: got %d, want 25", limit)
			}
			return []*persistence.CorpusRollback{{
				ID:          "rb1",
				ProjectID:   projectID,
				FromEpochID: &from,
				ToEpochID:   &to,
				TriggeredBy: "alice",
				Reason:      &reason,
				AppliedAt:   now,
			}}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/rollbacks?limit=25", nil)
	rec := httptest.NewRecorder()
	srv.MemoryRollbacks(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["total"].(float64) != 1 {
		t.Errorf("total: %v", body["total"])
	}
	rbs := body["rollbacks"].([]any)
	first := rbs[0].(map[string]any)
	if first["triggeredBy"] != "alice" || first["reason"] != "data poison drill" {
		t.Errorf("row payload: %v", first)
	}
}

// --- MemoryQuarantineList --------------------------------------------

func TestMemoryQuarantineList_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/quarantine", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineList(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemoryQuarantineList_Error(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		listPendingFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
			return nil, errors.New("boom")
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/quarantine", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineList(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemoryQuarantineList_Success_TruncatesLongContent(t *testing.T) {
	long := strings.Repeat("x", 500)
	role := "producer-role"
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		listPendingFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
			return []*persistence.MemoryQuarantineItem{{
				ID:            "q1",
				ProjectID:     projectID,
				Content:       long,
				ContentHash:   "deadbeef",
				FailedGate:    "duplicate",
				ProducerRole:  &role,
				QuarantinedAt: time.Now().UTC(),
			}}, nil
		},
		countByGateFn: func(ctx context.Context, projectID string) (map[string]int, error) {
			return map[string]int{"duplicate": 1, "policy": 0}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/quarantine", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineList(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	items := body["items"].([]any)
	first := items[0].(map[string]any)
	preview := first["contentPreview"].(string)
	if len(preview) != 240 {
		t.Errorf("preview length: got %d, want 240", len(preview))
	}
	if !strings.HasSuffix(preview, "...") {
		t.Errorf("expected ellipsis suffix on truncated preview")
	}
	if first["contentBytes"].(float64) != 500 {
		t.Errorf("contentBytes: %v", first["contentBytes"])
	}
}

// --- MemoryQuarantineAction ------------------------------------------

func TestMemoryQuarantineAction_Disabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/drop")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemoryQuarantineAction_MalformedSuffix(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{}}
	cases := []string{"q1", "a/b/c", ""}
	for _, suffix := range cases {
		t.Run(suffix, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/x", nil)
			rec := httptest.NewRecorder()
			srv.MemoryQuarantineAction(rec, req, "p1", suffix)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400", rec.Code)
			}
		})
	}
}

func TestMemoryQuarantineAction_EmptyIDOrAction(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine//drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "/drop")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestMemoryQuarantineAction_NotFound(t *testing.T) {
	// Need sql.ErrNoRows wrapping
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return nil, sqlErrNoRows
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/drop")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestMemoryQuarantineAction_GetError(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return nil, errors.New("db error")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/drop")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestMemoryQuarantineAction_ProjectMismatch(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: id, ProjectID: "different"}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/drop")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
}

func TestMemoryQuarantineAction_DropSuccess(t *testing.T) {
	gotID := ""
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: id, ProjectID: "p1"}, nil
		},
		markDroppedFn: func(ctx context.Context, id string) error {
			gotID = id
			return nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/drop")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotID != "q1" {
		t.Errorf("MarkDropped id: got %q, want q1", gotID)
	}
}

func TestMemoryQuarantineAction_DropError(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: id, ProjectID: "p1"}, nil
		},
		markDroppedFn: func(ctx context.Context, id string) error {
			return errors.New("drop failed")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/drop", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/drop")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestMemoryQuarantineAction_ReleaseNotImplemented(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: id, ProjectID: "p1"}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/release", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/release")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501", rec.Code)
	}
}

func TestMemoryQuarantineAction_UnknownAction(t *testing.T) {
	srv := &Server{memoryQuarantine: &stubMemoryQuarantineRepo{
		getFn: func(ctx context.Context, id string) (*persistence.MemoryQuarantineItem, error) {
			return &persistence.MemoryQuarantineItem{ID: id, ProjectID: "p1"}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/quarantine/q1/banana", nil)
	rec := httptest.NewRecorder()
	srv.MemoryQuarantineAction(rec, req, "p1", "q1/banana")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// --- MemoryHealth -----------------------------------------------------

func TestMemoryHealth_AllNilRepos(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/health", nil)
	rec := httptest.NewRecorder()
	srv.MemoryHealth(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["project"] != "p1" {
		t.Errorf("project: %v", body["project"])
	}
	if body["phase"] != "phase3" {
		t.Errorf("phase: %v", body["phase"])
	}
}

func TestMemoryHealth_AllReposPresent(t *testing.T) {
	srv := &Server{
		corpusEpochs: &stubCorpusEpochRepo{
			listEpochsFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
				return []*persistence.CorpusEpoch{{ID: "e1", ProjectID: projectID, CreatedAt: time.Now()}}, nil
			},
		},
		ingestQueue: &stubIngestQueueRepo{
			queueDepthFn: func(ctx context.Context, projectID string) (int, error) {
				return 7, nil
			},
		},
		memoryQuarantine: &stubMemoryQuarantineRepo{
			listPendingFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
				return []*persistence.MemoryQuarantineItem{{ID: "q1", ProjectID: projectID}}, nil
			},
			countByGateFn: func(ctx context.Context, projectID string) (map[string]int, error) {
				return map[string]int{"duplicate": 3}, nil
			},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/health", nil)
	rec := httptest.NewRecorder()
	srv.MemoryHealth(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["queueDepth"].(float64) != 7 {
		t.Errorf("queueDepth: %v", body["queueDepth"])
	}
	if body["quarantinePending"].(float64) != 1 {
		t.Errorf("quarantinePending: %v", body["quarantinePending"])
	}
}

// MemoryHealth swallows repo errors silently — verify the response is
// still 200 with the default zero values.
func TestMemoryHealth_ReposReturnErrors(t *testing.T) {
	srv := &Server{
		corpusEpochs: &stubCorpusEpochRepo{
			listEpochsFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.CorpusEpoch, error) {
				return nil, errors.New("err1")
			},
		},
		ingestQueue: &stubIngestQueueRepo{
			queueDepthFn: func(ctx context.Context, projectID string) (int, error) {
				return 0, errors.New("err2")
			},
		},
		memoryQuarantine: &stubMemoryQuarantineRepo{
			listPendingFn: func(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryQuarantineItem, error) {
				return nil, errors.New("err3")
			},
			countByGateFn: func(ctx context.Context, projectID string) (map[string]int, error) {
				return nil, errors.New("err4")
			},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/health", nil)
	rec := httptest.NewRecorder()
	srv.MemoryHealth(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
}
