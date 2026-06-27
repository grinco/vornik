package api

// Unit tests for the Self-Healing Workflow Genome v1 admin API (Unit 5).
// Each handler is exercised through httptest with in-memory fakes: the
// success path, the no-repo 503, the non-admin 403, the wrong-method 405,
// not-found, the gate-refusal (promote without trial_passed), and the
// redacted-principal audit invariant.

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

	"vornik.io/vornik/internal/persistence"
)

// --- fakes --------------------------------------------------------------

// fakeCandidateRepo is an in-memory WorkflowHealingCandidateRepository.
type fakeCandidateRepo struct {
	mu   sync.Mutex
	rows map[string]*persistence.HealingCandidate
}

func newFakeCandidateRepo() *fakeCandidateRepo {
	return &fakeCandidateRepo{rows: map[string]*persistence.HealingCandidate{}}
}

func (f *fakeCandidateRepo) put(c *persistence.HealingCandidate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[c.ID] = c
}

func (f *fakeCandidateRepo) Insert(_ context.Context, c *persistence.HealingCandidate) error {
	f.put(c)
	return nil
}
func (f *fakeCandidateRepo) Get(_ context.Context, id string) (*persistence.HealingCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return c, nil
}
func (f *fakeCandidateRepo) List(_ context.Context, _ persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	return nil, nil
}
func (f *fakeCandidateRepo) SetStatus(_ context.Context, id string, status persistence.HealingCandidateStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = status
	return nil
}
func (f *fakeCandidateRepo) BeginTrial(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	if c.Status.IsTerminal() || c.Status == persistence.HealingCandidateTrialRunning {
		return false, nil
	}
	c.Status = persistence.HealingCandidateTrialRunning
	return true, nil
}
func (f *fakeCandidateRepo) Promote(_ context.Context, id, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	now := time.Now()
	c.Status = persistence.HealingCandidatePromoted
	c.PromotedAt = &now
	c.PromotedBy = by
	return nil
}
func (f *fakeCandidateRepo) Reject(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = persistence.HealingCandidateRejected
	return nil
}

// fakeTrialRepo is an in-memory WorkflowHealingTrialRepository.
type fakeTrialRepo struct {
	mu   sync.Mutex
	rows map[string][]*persistence.HealingTrial
}

func newFakeTrialRepo() *fakeTrialRepo {
	return &fakeTrialRepo{rows: map[string][]*persistence.HealingTrial{}}
}
func (f *fakeTrialRepo) Insert(_ context.Context, tr *persistence.HealingTrial) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tr.ID == "" {
		tr.ID = "trial-" + tr.CandidateID
	}
	f.rows[tr.CandidateID] = append(f.rows[tr.CandidateID], tr)
	return nil
}
func (f *fakeTrialRepo) Get(_ context.Context, _ string) (*persistence.HealingTrial, error) {
	return nil, persistence.ErrNotFound
}
func (f *fakeTrialRepo) ListByCandidate(_ context.Context, candidateID string) ([]*persistence.HealingTrial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[candidateID], nil
}
func (f *fakeTrialRepo) Finish(_ context.Context, _ string, _ persistence.HealingTrialVerdict, _, _, _ string) error {
	return nil
}

// fakeTrialRunner implements HealingTrialRunner.
type fakeTrialRunner struct {
	outcome    *HealingTrialOutcome
	err        error
	trialID    string // returned by RunTrialAsync
	lastID     string
	lastMode   string
	asyncCalls int
	syncCalls  int
}

func (f *fakeTrialRunner) RunTrial(_ context.Context, candidateID, mode string, _ []string) (*HealingTrialOutcome, error) {
	f.syncCalls++
	f.lastID = candidateID
	f.lastMode = mode
	if f.err != nil {
		return nil, f.err
	}
	return f.outcome, nil
}

func (f *fakeTrialRunner) RunTrialAsync(_ context.Context, candidateID, mode string, _ []string) (string, error) {
	f.asyncCalls++
	f.lastID = candidateID
	f.lastMode = mode
	if f.err != nil {
		return "", f.err
	}
	if f.trialID == "" {
		return "wht_async_1", nil
	}
	return f.trialID, nil
}

// fakePromoter implements HealingCandidatePromoter.
type fakePromoter struct {
	promoteErr  error
	rejectErr   error
	promoteCand *persistence.HealingCandidate
	rejectCand  *persistence.HealingCandidate
	promotedBy  string
}

func (f *fakePromoter) Promote(_ context.Context, _, promotedBy string) (*persistence.HealingCandidate, error) {
	f.promotedBy = promotedBy
	if f.promoteErr != nil {
		return nil, f.promoteErr
	}
	return f.promoteCand, nil
}
func (f *fakePromoter) Reject(_ context.Context, _ string) (*persistence.HealingCandidate, error) {
	if f.rejectErr != nil {
		return nil, f.rejectErr
	}
	return f.rejectCand, nil
}

// fakeAuditRepo captures admin-audit inserts for the redaction assertion.
type fakeAuditRepo struct {
	mu      sync.Mutex
	entries []*persistence.AdminAuditEntry
}

func (f *fakeAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}
func (f *fakeAuditRepo) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

func sampleCandidate(id string, status persistence.HealingCandidateStatus) *persistence.HealingCandidate {
	return &persistence.HealingCandidate{
		ID:             id,
		TriggerID:      "trg-1",
		ProjectID:      "proj-1",
		WorkflowID:     "dev-pipeline",
		ProposalID:     "wpr-1",
		CandidateClass: persistence.HealingCandidateArchitect,
		RiskLevel:      persistence.HealingRiskMedium,
		Status:         status,
		CreatedAt:      time.Now(),
	}
}

// --- GET {id} -----------------------------------------------------------

func TestHealingCandidateGet_Happy(t *testing.T) {
	cr := newFakeCandidateRepo()
	cr.put(sampleCandidate("cand-1", persistence.HealingCandidateTrialPassed))
	tr := newFakeTrialRepo()
	_ = tr.Insert(context.Background(), &persistence.HealingTrial{
		CandidateID: "cand-1", Mode: persistence.HealingTrialModeStatic,
		Verdict: persistence.HealingTrialPassed, StartedAt: time.Now(),
		Scorecard: `{"verdict":"passed"}`,
	})
	opts := append(adminAuthOpts(),
		WithHealingCandidateRepository(cr),
		WithHealingTrialRepository(tr),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/cand-1", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var out HealingCandidateDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Candidate.ID != "cand-1" || out.Candidate.Status != "trial_passed" {
		t.Errorf("unexpected candidate: %+v", out.Candidate)
	}
	if len(out.Trials) != 1 || out.Trials[0].Verdict != "passed" {
		t.Errorf("expected 1 trial with verdict passed: %+v", out.Trials)
	}
	if string(out.Trials[0].Scorecard) != `{"verdict":"passed"}` {
		t.Errorf("scorecard not surfaced: %s", out.Trials[0].Scorecard)
	}
}

func TestHealingCandidateGet_NotFound(t *testing.T) {
	cr := newFakeCandidateRepo()
	opts := append(adminAuthOpts(), WithHealingCandidateRepository(cr))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/missing", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateGet_NoRepo503(t *testing.T) {
	opts := adminAuthOpts()
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/cand-1", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateGet_NonAdmin403(t *testing.T) {
	cr := newFakeCandidateRepo()
	cr.put(sampleCandidate("cand-1", persistence.HealingCandidateDraft))
	opts := append(adminAuthOpts(), WithHealingCandidateRepository(cr))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/cand-1", nil)
	// Auth-enabled is the default (absent → enabled); a non-allowlisted
	// key fails the admin gate with 403.
	req = withAdminKeyContext(req, "sk-not-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: %d (want 403) body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealingCandidateGet_WrongMethod405(t *testing.T) {
	cr := newFakeCandidateRepo()
	cr.put(sampleCandidate("cand-1", persistence.HealingCandidateDraft))
	opts := append(adminAuthOpts(), WithHealingCandidateRepository(cr))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/workflow-healing/candidates/cand-1", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", rec.Code)
	}
}

// --- run-trial ----------------------------------------------------------

func TestHealingCandidateRunTrial_Happy(t *testing.T) {
	runner := &fakeTrialRunner{outcome: &HealingTrialOutcome{
		Mode: "static", Verdict: "passed", ScorecardJSON: `{"verdict":"passed"}`,
	}}
	audit := &fakeAuditRepo{}
	opts := append(adminAuthOpts(),
		WithHealingTrialRunner(runner),
		WithAdminAuditRepository(audit),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial",
		bytes.NewReader([]byte(`{"mode":"static"}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if runner.lastID != "cand-1" || runner.lastMode != "static" {
		t.Errorf("runner not invoked correctly: id=%s mode=%s", runner.lastID, runner.lastMode)
	}
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out["verdict"] != "passed" {
		t.Errorf("verdict not surfaced: %+v", out)
	}
	// Audit row written with a redacted principal (never the raw key).
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	if audit.entries[0].Action != "blackbox-candidate.trial-run" {
		t.Errorf("audit action: %s", audit.entries[0].Action)
	}
	assertRedactedPrincipal(t, audit.entries[0].Principal)
}

// TestHealingCandidateRunTrial_ReplayIsAsync202: a replay trial opens
// asynchronously — 202 with the pending trial id, never the sync path
// (real replays run minutes past the 120s handler window; the sync
// path stranded trial_running/pending state, 2026-06-06).
func TestHealingCandidateRunTrial_ReplayIsAsync202(t *testing.T) {
	runner := &fakeTrialRunner{trialID: "wht_123"}
	audit := &fakeAuditRepo{}
	opts := append(adminAuthOpts(),
		WithHealingTrialRunner(runner),
		WithAdminAuditRepository(audit),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial",
		bytes.NewReader([]byte(`{"mode":"replay"}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if runner.asyncCalls != 1 || runner.syncCalls != 0 {
		t.Errorf("calls async=%d sync=%d, want 1/0", runner.asyncCalls, runner.syncCalls)
	}
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out["trial_id"] != "wht_123" || out["verdict"] != "pending" {
		t.Errorf("response missing pending trial id: %+v", out)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
}

// TestHealingCandidateRunTrial_AlreadyRunning409: a live concurrent
// trial maps to 409 TRIAL_RUNNING.
func TestHealingCandidateRunTrial_AlreadyRunning409(t *testing.T) {
	runner := &fakeTrialRunner{err: ErrHealingTrialRunning}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial",
		bytes.NewReader([]byte(`{"mode":"replay"}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "TRIAL_RUNNING") {
		t.Errorf("body missing TRIAL_RUNNING code: %s", rec.Body.String())
	}
}

func TestHealingCandidateRunTrial_DefaultsToStatic(t *testing.T) {
	runner := &fakeTrialRunner{outcome: &HealingTrialOutcome{Mode: "static", Verdict: "passed"}}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	// Empty body → static.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if runner.lastMode != "static" {
		t.Errorf("expected default mode static, got %q", runner.lastMode)
	}
}

func TestHealingCandidateRunTrial_NotFound(t *testing.T) {
	runner := &fakeTrialRunner{err: ErrHealingCandidateNotFound}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/missing/run-trial", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateRunTrial_BadMode400(t *testing.T) {
	runner := &fakeTrialRunner{err: ErrHealingTrialMode}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial",
		bytes.NewReader([]byte(`{"mode":"shadow"}`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateRunTrial_NoRunner503(t *testing.T) {
	opts := adminAuthOpts()
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateRunTrial_WrongMethod405(t *testing.T) {
	runner := &fakeTrialRunner{outcome: &HealingTrialOutcome{}}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", rec.Code)
	}
}

// --- promote ------------------------------------------------------------

func TestHealingCandidatePromote_Happy(t *testing.T) {
	promoter := &fakePromoter{promoteCand: sampleCandidate("cand-1", persistence.HealingCandidatePromoted)}
	audit := &fakeAuditRepo{}
	opts := append(adminAuthOpts(),
		WithHealingCandidatePromoter(promoter),
		WithAdminAuditRepository(audit),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var out HealingCandidateJSON
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Status != "promoted" {
		t.Errorf("status: %s", out.Status)
	}
	// promotedBy is a redacted principal, not the raw bearer key.
	assertRedactedPrincipal(t, promoter.promotedBy)
	if len(audit.entries) != 1 || audit.entries[0].Action != "blackbox-candidate.promoted" {
		t.Fatalf("audit not written: %+v", audit.entries)
	}
	assertRedactedPrincipal(t, audit.entries[0].Principal)
}

// TestHealingCandidatePromote_RefusesWithoutTrialPassed — the load-bearing
// safety test: promote must refuse (409) when the candidate has not cleared
// a trial. The promoter surfaces ErrHealingCandidateNotPromotable.
func TestHealingCandidatePromote_RefusesWithoutTrialPassed(t *testing.T) {
	promoter := &fakePromoter{promoteErr: ErrHealingCandidateNotPromotable}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d (want 409) body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "CANDIDATE_NOT_PROMOTABLE" {
		t.Errorf("expected CANDIDATE_NOT_PROMOTABLE, got %+v", body)
	}
}

func TestHealingCandidatePromote_NotFound(t *testing.T) {
	promoter := &fakePromoter{promoteErr: ErrHealingCandidateNotFound}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/missing/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidatePromote_NoPromoter503(t *testing.T) {
	opts := adminAuthOpts()
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidatePromote_WrongMethod405(t *testing.T) {
	promoter := &fakePromoter{promoteCand: sampleCandidate("cand-1", persistence.HealingCandidatePromoted)}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-healing/candidates/cand-1/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", rec.Code)
	}
}

// --- reject -------------------------------------------------------------

func TestHealingCandidateReject_Happy(t *testing.T) {
	promoter := &fakePromoter{rejectCand: sampleCandidate("cand-1", persistence.HealingCandidateRejected)}
	audit := &fakeAuditRepo{}
	opts := append(adminAuthOpts(),
		WithHealingCandidatePromoter(promoter),
		WithAdminAuditRepository(audit),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/reject", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var out HealingCandidateJSON
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Status != "rejected" {
		t.Errorf("status: %s", out.Status)
	}
	if len(audit.entries) != 1 || audit.entries[0].Action != "blackbox-candidate.rejected" {
		t.Fatalf("audit not written: %+v", audit.entries)
	}
	assertRedactedPrincipal(t, audit.entries[0].Principal)
}

func TestHealingCandidateReject_Terminal409(t *testing.T) {
	promoter := &fakePromoter{rejectErr: ErrHealingCandidateTerminal}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/reject", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateReject_NoPromoter503(t *testing.T) {
	opts := adminAuthOpts()
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/reject", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", rec.Code)
	}
}

// --- malformed-id routing -----------------------------------------------

func TestHealingCandidates_MalformedID(t *testing.T) {
	cr := newFakeCandidateRepo()
	opts := append(adminAuthOpts(), WithHealingCandidateRepository(cr))
	s := NewServer(opts...)
	// A nested path under the prefix that isn't a recognised action.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/a/b", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rec.Code)
	}
}

// --- extra branch coverage ---------------------------------------------

func TestHealingCandidateRunTrial_MalformedBody400(t *testing.T) {
	runner := &fakeTrialRunner{outcome: &HealingTrialOutcome{}}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial",
		bytes.NewReader([]byte(`{not json`)))
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateRunTrial_InternalError500(t *testing.T) {
	runner := &fakeTrialRunner{err: errors.New("replay engine exploded")}
	opts := append(adminAuthOpts(), WithHealingTrialRunner(runner))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/run-trial", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidatePromote_InternalError500(t *testing.T) {
	promoter := &fakePromoter{promoteErr: errors.New("git commit failed")}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidatePromote_Terminal409(t *testing.T) {
	promoter := &fakePromoter{promoteErr: ErrHealingCandidateTerminal}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/promote", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateReject_NotFound(t *testing.T) {
	promoter := &fakePromoter{rejectErr: ErrHealingCandidateNotFound}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/reject", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestHealingCandidateReject_InternalError500(t *testing.T) {
	promoter := &fakePromoter{rejectErr: errors.New("db down")}
	opts := append(adminAuthOpts(), WithHealingCandidatePromoter(promoter))
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/candidates/cand-1/reject", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", rec.Code)
	}
}

// TestHealingCandidateGet_PromotedAndFinishedFields exercises the
// promoted_at / finished_at / summary-blob branches of the JSON mappers.
func TestHealingCandidateGet_PromotedAndFinishedFields(t *testing.T) {
	now := time.Now()
	cand := sampleCandidate("cand-1", persistence.HealingCandidatePromoted)
	cand.PromotedAt = &now
	cand.PromotedBy = "api_key_sha256:abc"
	cand.CandidateGenomeHash = "deadbeef"
	cr := newFakeCandidateRepo()
	cr.put(cand)
	tr := newFakeTrialRepo()
	fin := now
	_ = tr.Insert(context.Background(), &persistence.HealingTrial{
		ID: "tr-1", CandidateID: "cand-1", Mode: persistence.HealingTrialModeReplay,
		EvidenceExecutionIDs: []string{"ev1", "ev2"},
		BaselineSummary:      `{"runs":2}`,
		CandidateSummary:     `{"runs":2,"successes":2}`,
		Scorecard:            `{"verdict":"passed"}`,
		Verdict:              persistence.HealingTrialPassed,
		StartedAt:            now, FinishedAt: &fin,
	})
	opts := append(adminAuthOpts(),
		WithHealingCandidateRepository(cr),
		WithHealingTrialRepository(tr),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-healing/candidates/cand-1", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.adminHealingCandidatesItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var out HealingCandidateDetailResponse
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Candidate.PromotedAt == "" || out.Candidate.PromotedBy == "" {
		t.Errorf("promoted fields not surfaced: %+v", out.Candidate)
	}
	if out.Candidate.CandidateGenomeHash != "deadbeef" {
		t.Errorf("genome hash not surfaced: %+v", out.Candidate)
	}
	if len(out.Trials) != 1 {
		t.Fatalf("expected 1 trial: %+v", out.Trials)
	}
	tj := out.Trials[0]
	if tj.FinishedAt == "" || len(tj.EvidenceExecutionIDs) != 2 {
		t.Errorf("trial fields not surfaced: %+v", tj)
	}
	if string(tj.BaselineSummary) != `{"runs":2}` || string(tj.CandidateSummary) == "" {
		t.Errorf("summary blobs not surfaced: %+v", tj)
	}
}

// assertRedactedPrincipal verifies the audit/promotedBy principal is the
// redacted form (never the raw bearer key "sk-admin"). With auth disabled
// in the test gate (adminAuthOpts does not enable auth), the principal
// falls back to "anonymous-admin" — also acceptable, as long as it is not
// the raw key.
func assertRedactedPrincipal(t *testing.T, principal string) {
	t.Helper()
	if principal == "sk-admin" || principal == "" {
		t.Fatalf("principal must be redacted, got %q", principal)
	}
}
