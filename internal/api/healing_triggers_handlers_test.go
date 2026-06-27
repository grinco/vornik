package api

// REST API tests for the workflow-healing trigger + override
// admin endpoints (Black Box Phase B). The handlers' UI siblings
// in internal/ui have their own coverage; these tests pin the
// wire shape + admin-gate behaviour the api package owns.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// apiStubHealingTriggerRepo is the api-package stub. Distinct
// from the ui-package stub of the same shape because both
// packages own their tests and the api can't import the ui
// package without an import cycle.
type apiStubHealingTriggerRepo struct {
	mu          sync.Mutex
	rows        map[string]*persistence.HealingTrigger
	dismissErr  error
	markGenErr  error
	lastMarkGen struct{ id, proposalID string }
}

func newAPIStubHealingTriggerRepo() *apiStubHealingTriggerRepo {
	return &apiStubHealingTriggerRepo{rows: map[string]*persistence.HealingTrigger{}}
}

func (s *apiStubHealingTriggerRepo) Insert(_ context.Context, t *persistence.HealingTrigger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *t
	s.rows[t.ID] = &cp
	return nil
}

func (s *apiStubHealingTriggerRepo) Get(_ context.Context, id string) (*persistence.HealingTrigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return t, nil
}

func (s *apiStubHealingTriggerRepo) List(_ context.Context, _ persistence.HealingTriggerListFilter) ([]*persistence.HealingTrigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.HealingTrigger, 0, len(s.rows))
	for _, t := range s.rows {
		out = append(out, t)
	}
	return out, nil
}

func (s *apiStubHealingTriggerRepo) Dismiss(_ context.Context, id string) error {
	if s.dismissErr != nil {
		return s.dismissErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok || t.Status != persistence.HealingTriggerStatusOpen {
		return persistence.ErrNotFound
	}
	t.Status = persistence.HealingTriggerStatusDismissed
	return nil
}

func (s *apiStubHealingTriggerRepo) MarkGenerated(_ context.Context, id, proposalID string) error {
	if s.markGenErr != nil {
		return s.markGenErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok || t.Status != persistence.HealingTriggerStatusOpen {
		return persistence.ErrNotFound
	}
	t.Status = persistence.HealingTriggerStatusGeneratedCandidate
	t.ProposalID = proposalID
	s.lastMarkGen.id = id
	s.lastMarkGen.proposalID = proposalID
	return nil
}

// apiOpenTrigger builds an open trigger row for the api-package tests.
func apiOpenTrigger(id string) *persistence.HealingTrigger {
	now := time.Now().UTC()
	return &persistence.HealingTrigger{
		ID:           id,
		ProjectID:    "proj-x",
		WorkflowID:   "wf-a",
		TriggerClass: persistence.HealingTriggerFailureRateSpike,
		Status:       persistence.HealingTriggerStatusOpen,
		CreatedAt:    now,
	}
}

// adminAuthOpts builds the standard "admin enabled + one allowed
// key" wiring so the handler tests can focus on logic rather
// than gate setup.
func adminAuthOpts() []ServerOption {
	return []ServerOption{
		WithAdminConfig(config.AdminConfig{
			Enabled:     true,
			AllowedKeys: []string{"sk-admin"},
		}),
	}
}

// TestHealingTriggersBulkDismiss_Happy — POST {ids:[a,b]} dismisses
// each, returns dismissed=2.
func TestHealingTriggersBulkDismiss_Happy(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	for _, id := range []string{"t-1", "t-2"} {
		_ = repo.Insert(context.Background(), apiOpenTrigger(id))
	}
	opts := append(adminAuthOpts(), WithHealingTriggerRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/bulk-dismiss",
		bytes.NewReader([]byte(`{"ids":["t-1","t-2"]}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var out HealingTriggerBulkDismissResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Dismissed != 2 || len(out.Failures) != 0 {
		t.Errorf("expected dismissed=2 with no failures: %+v", out)
	}
}

// TestHealingTriggersBulkDismiss_PartialFailure — missing ID
// surfaces in failures[]; successes still applied.
func TestHealingTriggersBulkDismiss_PartialFailure(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	opts := append(adminAuthOpts(), WithHealingTriggerRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/bulk-dismiss",
		bytes.NewReader([]byte(`{"ids":["t-1","t-missing"]}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var out HealingTriggerBulkDismissResponse
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Dismissed != 1 || len(out.Failures) != 1 || out.Failures[0].ID != "t-missing" {
		t.Errorf("expected dismissed=1 + 1 failure on t-missing: %+v", out)
	}
}

// TestHealingTriggersBulkDismiss_EmptyIDsRejected — body without
// ids → 400.
func TestHealingTriggersBulkDismiss_EmptyIDsRejected(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	opts := append(adminAuthOpts(), WithHealingTriggerRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/bulk-dismiss",
		bytes.NewReader([]byte(`{"ids":[]}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rec.Code)
	}
}

// TestHealingTriggerGenerateCandidate_Happy — repo returns open
// row, architect succeeds, MarkGenerated stamps, response carries
// status=generated_candidate + proposal_id.
func TestHealingTriggerGenerateCandidate_Happy(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	arch := &stubArchitect{
		result: &persistence.WorkflowProposal{ID: "wpr-7", WorkflowID: "wf-a"},
	}
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if arch.lastWorkflowID != "wf-a" {
		t.Errorf("architect saw workflow %q, want wf-a", arch.lastWorkflowID)
	}
	if repo.lastMarkGen.id != "t-1" || repo.lastMarkGen.proposalID != "wpr-7" {
		t.Errorf("MarkGenerated args: %+v", repo.lastMarkGen)
	}
	var got HealingTriggerJSON
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.Status != string(persistence.HealingTriggerStatusGeneratedCandidate) {
		t.Errorf("status: %q", got.Status)
	}
	if got.ProposalID != "wpr-7" {
		t.Errorf("proposal_id: %q", got.ProposalID)
	}
}

// TestHealingTriggerGenerateCandidate_AlreadyTerminal — 409
// CONFLICT (TRIGGER_NOT_OPEN), architect not called.
func TestHealingTriggerGenerateCandidate_AlreadyTerminal(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	tr := apiOpenTrigger("t-1")
	tr.Status = persistence.HealingTriggerStatusDismissed
	_ = repo.Insert(context.Background(), tr)
	arch := &stubArchitect{
		result: &persistence.WorkflowProposal{ID: "wpr-x", WorkflowID: "wf-a"},
	}
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d, want 409 (terminal trigger)", rec.Code)
	}
	if arch.lastWorkflowID != "" {
		t.Error("architect must not be called when trigger is terminal")
	}
}

// TestHealingTriggerGenerateCandidate_ArchitectFail — architect
// returns error; trigger stays open (no MarkGenerated call).
func TestHealingTriggerGenerateCandidate_ArchitectFail(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	arch := &stubArchitect{err: errors.New("LLM timeout")}
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500 (default mapping)", rec.Code)
	}
	if repo.lastMarkGen.id != "" {
		t.Error("MarkGenerated must not be called on architect failure")
	}
}

// TestHealingTriggerGenerateCandidate_ArchitectMissing — 503 when
// the architect isn't wired, regardless of trigger status.
func TestHealingTriggerGenerateCandidate_ArchitectMissing(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	opts := append(adminAuthOpts(), WithHealingTriggerRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d, want 503", rec.Code)
	}
}

// TestHealingTriggerGenerateCandidate_NotFound — missing trigger ID
// → 404.
func TestHealingTriggerGenerateCandidate_NotFound(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	arch := &stubArchitect{
		result: &persistence.WorkflowProposal{ID: "wpr-x"},
	}
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/missing/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

// TestHealingTriggersBulkDismiss_AdminGate — no admin key → 401.
func TestHealingTriggersBulkDismiss_AdminGate(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	opts := append(adminAuthOpts(), WithHealingTriggerRepository(repo))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/bulk-dismiss",
		strings.NewReader(`{"ids":["t-1"]}`))
	// No withAdminKeyContext → unauthenticated.
	rec := httptest.NewRecorder()
	s.AdminHealingTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", rec.Code)
	}
}
