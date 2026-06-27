package api

// REST API tests for the workflow-healing override admin endpoints
// (Phase B operator-tuning, migration 81). Companion to the
// healing_triggers_handlers_test.go file — uses the same
// adminAuthOpts() helper.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// apiStubHealingOverrideRepo is the api-package stub for the
// override repo. Separate from the ui-package stub to keep the
// two packages independent.
type apiStubHealingOverrideRepo struct {
	mu          sync.Mutex
	rows        map[string]*persistence.HealingTriggerOverride
	upsertErr   error
	deleteCalls int
	lastUpsert  *persistence.HealingTriggerOverride
	lastDelete  struct{ project, workflow, class string }
}

func newAPIStubHealingOverrideRepo() *apiStubHealingOverrideRepo {
	return &apiStubHealingOverrideRepo{rows: map[string]*persistence.HealingTriggerOverride{}}
}

func (s *apiStubHealingOverrideRepo) keyOf(p, w, c string) string { return p + "|" + w + "|" + c }

func (s *apiStubHealingOverrideRepo) Upsert(_ context.Context, o *persistence.HealingTriggerOverride) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *o
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	cp.UpdatedAt = time.Now().UTC()
	s.rows[s.keyOf(o.ProjectID, o.WorkflowID, string(o.TriggerClass))] = &cp
	s.lastUpsert = &cp
	return nil
}

func (s *apiStubHealingOverrideRepo) Get(_ context.Context, p, w string, c persistence.HealingTriggerClass) (*persistence.HealingTriggerOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.rows[s.keyOf(p, w, string(c))]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return o, nil
}

func (s *apiStubHealingOverrideRepo) List(_ context.Context, _ int) ([]*persistence.HealingTriggerOverride, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.HealingTriggerOverride, 0, len(s.rows))
	for _, o := range s.rows {
		out = append(out, o)
	}
	return out, nil
}

func (s *apiStubHealingOverrideRepo) Delete(_ context.Context, p, w string, c persistence.HealingTriggerClass) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, s.keyOf(p, w, string(c)))
	s.lastDelete.project, s.lastDelete.workflow, s.lastDelete.class = p, w, string(c)
	s.deleteCalls++
	return nil
}

// TestHealingOverridesList_NotWired — repo nil → 503.
func TestHealingOverridesList_NotWired(t *testing.T) {
	s := NewServer(adminAuthOpts()...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/overrides", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverridesList(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d, want 503", rec.Code)
	}
}

// TestHealingOverridesList_Happy — populated repo, JSON shape.
func TestHealingOverridesList_Happy(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	thresh := 0.50
	until := time.Now().UTC().Add(24 * time.Hour)
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID:         "p1",
		WorkflowID:        "wf-1",
		TriggerClass:      persistence.HealingTriggerFailureRateSpike,
		ThresholdOverride: &thresh,
		MutedUntil:        &until,
		Notes:             "tuning",
	})
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/overrides", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverridesList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var out HealingOverrideListResponse
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if len(out.Entries) != 1 {
		t.Fatalf("entries: %d", len(out.Entries))
	}
	if out.Entries[0].ProjectID != "p1" || *out.Entries[0].ThresholdOverride != 0.50 {
		t.Errorf("wire shape mismatch: %+v", out.Entries[0])
	}
	if out.Entries[0].MutedUntil == "" {
		t.Error("muted_until should be set")
	}
}

// TestHealingOverrideUpsert_Happy — JSON in, JSON out, repo
// received the parsed values.
func TestHealingOverrideUpsert_Happy(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	body := `{"project_id":"p1","workflow_id":"wf-1","trigger_class":"failure_rate_spike","threshold_override":0.50,"mute_duration":"6h","notes":"tuning"}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/overrides",
		bytes.NewReader([]byte(body)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverrideUpsert(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastUpsert == nil {
		t.Fatal("repo.Upsert not called")
	}
	if repo.lastUpsert.ThresholdOverride == nil || *repo.lastUpsert.ThresholdOverride != 0.50 {
		t.Errorf("threshold: %v", repo.lastUpsert.ThresholdOverride)
	}
	if repo.lastUpsert.MutedUntil == nil {
		t.Error("mute_duration should produce a MutedUntil")
	} else {
		dt := time.Until(*repo.lastUpsert.MutedUntil)
		if dt < 5*time.Hour+30*time.Minute || dt > 6*time.Hour+30*time.Minute {
			t.Errorf("mute_until off: want ~6h from now, got %v", dt)
		}
	}
}

// TestHealingOverrideUpsert_NothingToSave — none of the action
// fields set → 400.
func TestHealingOverrideUpsert_NothingToSave(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	body := `{"project_id":"p1","workflow_id":"wf-1","trigger_class":"failure_rate_spike","notes":"hi"}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/overrides",
		bytes.NewReader([]byte(body)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverrideUpsert(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rec.Code)
	}
	if repo.lastUpsert != nil {
		t.Error("repo must not be touched when nothing to save")
	}
}

// TestHealingOverrideUpsert_UnknownClass — enum rejected at the wire.
func TestHealingOverrideUpsert_UnknownClass(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	body := `{"project_id":"p1","workflow_id":"wf-1","trigger_class":"bogus","threshold_override":0.50}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/overrides",
		bytes.NewReader([]byte(body)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverrideUpsert(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rec.Code)
	}
}

// TestHealingOverrideUpsert_ClearMute — clear_mute=true and no
// mute_duration → row stored with MutedUntil=nil even if a previous
// row had one (repo's job is overwrite; we just verify the API
// passes through correctly).
func TestHealingOverrideUpsert_ClearMute(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	body := `{"project_id":"p1","workflow_id":"wf-1","trigger_class":"failure_rate_spike","threshold_override":0.30,"clear_mute":true}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/overrides",
		bytes.NewReader([]byte(body)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverrideUpsert(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if repo.lastUpsert == nil || repo.lastUpsert.MutedUntil != nil {
		t.Errorf("clear_mute should produce MutedUntil=nil; got %+v", repo.lastUpsert)
	}
}

// TestHealingOverrideDelete_Happy — POST body removes the row.
func TestHealingOverrideDelete_Happy(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	thresh := 0.25
	_ = repo.Upsert(context.Background(), &persistence.HealingTriggerOverride{
		ProjectID: "p1", WorkflowID: "wf-1",
		TriggerClass:      persistence.HealingTriggerFailureRateSpike,
		ThresholdOverride: &thresh,
	})
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	body := `{"project_id":"p1","workflow_id":"wf-1","trigger_class":"failure_rate_spike"}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/overrides/delete",
		bytes.NewReader([]byte(body)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverrideDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	if repo.deleteCalls != 1 {
		t.Errorf("delete calls: %d", repo.deleteCalls)
	}
}

// TestHealingOverrideDelete_MissingIdempotent — deleting a row
// that doesn't exist still returns 200; repo's Delete contract is
// idempotent.
func TestHealingOverrideDelete_MissingIdempotent(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	body := `{"project_id":"p1","workflow_id":"wf-1","trigger_class":"failure_rate_spike"}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/overrides/delete",
		bytes.NewReader([]byte(body)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingOverrideDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d, want 200 (idempotent)", rec.Code)
	}
}

// TestHealingOverridesList_AdminGate — no admin key → 401.
func TestHealingOverridesList_AdminGate(t *testing.T) {
	repo := newAPIStubHealingOverrideRepo()
	opts := append(adminAuthOpts(), WithHealingOverrideRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/overrides", nil)
	rec := httptest.NewRecorder()
	s.AdminHealingOverridesList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", rec.Code)
	}
}
