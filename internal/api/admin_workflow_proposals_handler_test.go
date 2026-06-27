package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubProposalRepo satisfies persistence.WorkflowProposalRepository
// for the handler tests. Records the last filter/decide so we can
// assert the parsed query reached the layer correctly.
type stubProposalRepo struct {
	getResult    *persistence.WorkflowProposal
	getErr       error
	listResult   []*persistence.WorkflowProposal
	listFilter   persistence.WorkflowProposalFilter
	listErr      error
	decideErr    error
	decideStatus persistence.WorkflowProposalStatus
	decideNotes  string
	decideBy     string
}

func (s *stubProposalRepo) Insert(_ context.Context, _ *persistence.WorkflowProposal) error {
	return nil
}
func (s *stubProposalRepo) Get(_ context.Context, _ string) (*persistence.WorkflowProposal, error) {
	return s.getResult, s.getErr
}
func (s *stubProposalRepo) List(_ context.Context, f persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	s.listFilter = f
	return s.listResult, s.listErr
}
func (s *stubProposalRepo) Decide(_ context.Context, _ string, status persistence.WorkflowProposalStatus, decidedBy, notes string) error {
	s.decideStatus = status
	s.decideBy = decidedBy
	s.decideNotes = notes
	return s.decideErr
}
func (s *stubProposalRepo) MarkApplied(_ context.Context, _, _ string) error    { return nil }
func (s *stubProposalRepo) MarkRolledBack(_ context.Context, _, _ string) error { return nil }
func (s *stubProposalRepo) UpdateProposalYAML(_ context.Context, _, _, _ string) error {
	return nil
}

func newAdminProposalServer(repo persistence.WorkflowProposalRepository) *Server {
	return NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(repo),
	)
}

// LIST -----------------------------------------------------------

func TestAdminWorkflowProposalsList_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalsList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-proposals", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: want 404, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsList_NotWired(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}))
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-proposals", nil), "sk-admin")
	s.AdminWorkflowProposalsList(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsList_NoKey(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalsList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-proposals", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsList_NonAdmin(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-proposals", nil), "sk-user")
	s.AdminWorkflowProposalsList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsList_WrongMethod(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost, "/api/v1/admin/workflow-proposals", nil), "sk-admin")
	s.AdminWorkflowProposalsList(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: want 405, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsList_ThreadsFilters(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubProposalRepo{
		listResult: []*persistence.WorkflowProposal{
			{ID: "wpr-1", WorkflowID: "wf-a", Status: persistence.WorkflowProposalStatusPending, CreatedAt: now},
		},
	}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals?status=pending,approved&workflow=wf-a&limit=10", nil), "sk-admin")
	s.AdminWorkflowProposalsList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if repo.listFilter.WorkflowID != "wf-a" {
		t.Errorf("workflow filter: %q", repo.listFilter.WorkflowID)
	}
	if repo.listFilter.PageSize != 10 {
		t.Errorf("limit: %d", repo.listFilter.PageSize)
	}
	if len(repo.listFilter.Statuses) != 2 {
		t.Errorf("statuses: %v", repo.listFilter.Statuses)
	}
	var body workflowProposalListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Proposals) != 1 {
		t.Errorf("want 1 proposal in body, got %d", len(body.Proposals))
	}
}

// GET ------------------------------------------------------------

func TestAdminWorkflowProposalsItem_Get_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	decided := now.Add(time.Hour)
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-1", WorkflowID: "wf-a",
			Status:         persistence.WorkflowProposalStatusApproved,
			ProposalYAML:   "yaml",
			Motivation:     "motiv",
			EvidenceRunIDs: []string{"r-1"},
			Confidence:     0.7,
			ArchitectModel: "m",
			CreatedAt:      now,
			DecidedAt:      &decided,
			DecidedBy:      "operator",
			Notes:          "ok",
		},
	}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals/wpr-1", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	var got workflowProposalJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "wpr-1" || got.Status != "approved" {
		t.Errorf("threading: %+v", got)
	}
	if got.DecidedBy != "operator" {
		t.Errorf("decided_by: %q", got.DecidedBy)
	}
	if got.DecidedAt == "" {
		t.Error("decided_at should be set when row has a timestamp")
	}
}

func TestAdminWorkflowProposalsItem_Get_NotFound(t *testing.T) {
	repo := &stubProposalRepo{getErr: persistence.ErrNotFound}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals/wpr-missing", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Get_WrongMethod(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPut,
		"/api/v1/admin/workflow-proposals/wpr-1", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: want 405, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_EmptyID(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals/", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty id: want 404, got %d", rec.Code)
	}
}

// DECIDE ---------------------------------------------------------

func TestAdminWorkflowProposalsItem_Decide_Approve(t *testing.T) {
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-1", WorkflowID: "wf-a",
			Status:    persistence.WorkflowProposalStatusApproved,
			CreatedAt: time.Now().UTC(),
		},
	}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	body := `{"status":"approved","notes":"ship it"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if repo.decideStatus != persistence.WorkflowProposalStatusApproved {
		t.Errorf("decide status: %q", repo.decideStatus)
	}
	if repo.decideNotes != "ship it" {
		t.Errorf("notes: %q", repo.decideNotes)
	}
	if repo.decideBy != "sk-admin" {
		t.Errorf("decided_by should be admin key: %q", repo.decideBy)
	}
}

func TestAdminWorkflowProposalsItem_Decide_Reject(t *testing.T) {
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-1", Status: persistence.WorkflowProposalStatusRejected,
			CreatedAt: time.Now().UTC(),
		},
	}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	body := `{"status":"rejected"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

// fakeRejectionRecorder records RecordRejection calls so the wiring
// test can assert the write-back fired (or didn't).
type fakeRejectionRecorder struct {
	calls   int
	lastID  string
	wantErr error
}

func (f *fakeRejectionRecorder) RecordRejection(_ context.Context, proposal any) error {
	f.calls++
	if p, ok := proposal.(*persistence.WorkflowProposal); ok && p != nil {
		f.lastID = p.ID
	}
	return f.wantErr
}

// TestAdminWorkflowProposalsItem_Decide_Reject_WritesBack — a rejection
// fires the Consumer B write-back with the rejected proposal.
func TestAdminWorkflowProposalsItem_Decide_Reject_WritesBack(t *testing.T) {
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-1", WorkflowID: "wf-a",
			Status:    persistence.WorkflowProposalStatusRejected,
			CreatedAt: time.Now().UTC(),
		},
	}
	rec := &fakeRejectionRecorder{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(repo),
		WithWorkflowRejectionRecorder(rec),
	)
	rw := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(`{"status":"rejected"}`))), "sk-admin")
	s.AdminWorkflowProposalsItem(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rw.Code)
	}
	if rec.calls != 1 {
		t.Fatalf("expected 1 write-back call, got %d", rec.calls)
	}
	if rec.lastID != "wpr-1" {
		t.Errorf("write-back got wrong proposal: %q", rec.lastID)
	}
}

// TestAdminWorkflowProposalsItem_Decide_Approve_NoWriteBack — approving
// must NOT fire the rejection write-back.
func TestAdminWorkflowProposalsItem_Decide_Approve_NoWriteBack(t *testing.T) {
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-2", WorkflowID: "wf-a",
			Status:    persistence.WorkflowProposalStatusApproved,
			CreatedAt: time.Now().UTC(),
		},
	}
	rec := &fakeRejectionRecorder{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(repo),
		WithWorkflowRejectionRecorder(rec),
	)
	rw := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-2/decide", bytes.NewReader([]byte(`{"status":"approved"}`))), "sk-admin")
	s.AdminWorkflowProposalsItem(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rw.Code)
	}
	if rec.calls != 0 {
		t.Errorf("approval must not fire write-back, got %d calls", rec.calls)
	}
}

// TestAdminWorkflowProposalsItem_Decide_Reject_WriteBackErrorBestEffort
// — a write-back error must not fail the operator's rejection.
func TestAdminWorkflowProposalsItem_Decide_Reject_WriteBackErrorBestEffort(t *testing.T) {
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-3", WorkflowID: "wf-a",
			Status:    persistence.WorkflowProposalStatusRejected,
			CreatedAt: time.Now().UTC(),
		},
	}
	rec := &fakeRejectionRecorder{wantErr: errors.New("boom")}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(repo),
		WithWorkflowRejectionRecorder(rec),
	)
	rw := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-3/decide", bytes.NewReader([]byte(`{"status":"rejected"}`))), "sk-admin")
	s.AdminWorkflowProposalsItem(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("write-back error must not fail rejection, got %d", rw.Code)
	}
	if rec.calls != 1 {
		t.Errorf("expected write-back attempt, got %d", rec.calls)
	}
}

// TestAdminWorkflowProposalsItem_Decide_Reject_NoRecorderNoop — with no
// recorder wired (gate off), rejection still succeeds.
func TestAdminWorkflowProposalsItem_Decide_Reject_NoRecorderNoop(t *testing.T) {
	repo := &stubProposalRepo{
		getResult: &persistence.WorkflowProposal{
			ID: "wpr-4", WorkflowID: "wf-a",
			Status:    persistence.WorkflowProposalStatusRejected,
			CreatedAt: time.Now().UTC(),
		},
	}
	s := newAdminProposalServer(repo)
	rw := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-4/decide", bytes.NewReader([]byte(`{"status":"rejected"}`))), "sk-admin")
	s.AdminWorkflowProposalsItem(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200 with no recorder, got %d", rw.Code)
	}
}

func TestAdminWorkflowProposalsItem_Decide_InvalidStatus(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	body := `{"status":"applied"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: want 400, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Decide_BadBody(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(`not json`))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: want 400, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Decide_NotFound(t *testing.T) {
	repo := &stubProposalRepo{decideErr: persistence.ErrNotFound}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	body := `{"status":"approved"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not found: want 404, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalsItem_Decide_InvalidTransition pins the
// 409 path. Operator A and operator B race; B's decide hits a row
// that's no longer pending, gets a clear conflict response rather
// than a generic 500.
func TestAdminWorkflowProposalsItem_Decide_InvalidTransition(t *testing.T) {
	repo := &stubProposalRepo{decideErr: persistence.ErrInvalidProposalTransition}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	body := `{"status":"approved"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("invalid transition: want 409, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Decide_GenericError(t *testing.T) {
	repo := &stubProposalRepo{decideErr: errors.New("db down")}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	body := `{"status":"approved"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("db error: want 500, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Decide_WrongMethod(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on /decide: want 405, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_UnknownSubpath(t *testing.T) {
	s := newAdminProposalServer(&stubProposalRepo{})
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/wat", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown subpath: want 404, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalsList_RepoError covers the rare DB-down
// path so mapProposalReadError's default branch is exercised.
func TestAdminWorkflowProposalsList_RepoError(t *testing.T) {
	repo := &stubProposalRepo{listErr: errors.New("db down")}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals", nil), "sk-admin")
	s.AdminWorkflowProposalsList(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("repo error: want 500, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalsList_LimitClamping — limit values
// outside (0, 500] are ignored and the default (50) applies.
// Guards against operators dumping the full table with limit=999999.
func TestAdminWorkflowProposalsList_LimitClamping(t *testing.T) {
	cases := []string{"0", "-5", "999999", "junk"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			repo := &stubProposalRepo{}
			s := newAdminProposalServer(repo)
			rec := httptest.NewRecorder()
			req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
				"/api/v1/admin/workflow-proposals?limit="+v, nil), "sk-admin")
			s.AdminWorkflowProposalsList(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("limit=%q: status %d", v, rec.Code)
			}
			if repo.listFilter.PageSize != 50 {
				t.Errorf("limit=%q should default to 50, got %d", v, repo.listFilter.PageSize)
			}
		})
	}
}

// TestAdminWorkflowProposalsItem_Get_GenericError covers the
// non-ErrNotFound branch in mapProposalReadError so 500-with-body
// path stays correct.
func TestAdminWorkflowProposalsItem_Get_GenericError(t *testing.T) {
	repo := &stubProposalRepo{getErr: errors.New("db down")}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals/wpr-1", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("generic error: want 500, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalsItem_Decide_ReadbackFails — when the
// read-after-write Get errors, we still report success with the
// minimal {id, status} payload rather than failing the whole
// request.
func TestAdminWorkflowProposalsItem_Decide_ReadbackFails(t *testing.T) {
	repo := &stubProposalRepo{getErr: errors.New("transient")}
	s := newAdminProposalServer(repo)
	rec := httptest.NewRecorder()
	body := `{"status":"approved"}`
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/decide", bytes.NewReader([]byte(body))), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readback fail should still 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"id":"wpr-1"`)) {
		t.Errorf("minimal payload should include id, body=%q", rec.Body.String())
	}
}

// TestToWorkflowProposalJSON_AppliedTimestamp pins the second
// nullable-time branch.
func TestToWorkflowProposalJSON_AppliedTimestamp(t *testing.T) {
	applied := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)
	got := toWorkflowProposalJSON(&persistence.WorkflowProposal{
		ID: "x", WorkflowID: "y",
		Status:        persistence.WorkflowProposalStatusApplied,
		CreatedAt:     applied.Add(-time.Hour),
		AppliedAt:     &applied,
		AppliedCommit: "abc1234",
	})
	if got.AppliedAt == "" {
		t.Error("AppliedAt should render when set")
	}
	if got.AppliedCommit != "abc1234" {
		t.Errorf("AppliedCommit: %q", got.AppliedCommit)
	}
}

// APPLY ---------------------------------------------------------

// stubWorkflowApplier satisfies the WorkflowApplier interface and
// captures the call.
type stubWorkflowApplier struct {
	lastID        string
	lastAppliedBy string
	result        any
	err           error
}

func (s *stubWorkflowApplier) Apply(_ context.Context, id, appliedBy string) (any, error) {
	s.lastID = id
	s.lastAppliedBy = appliedBy
	return s.result, s.err
}

func TestAdminWorkflowProposalsItem_Apply_HappyPath(t *testing.T) {
	stub := &stubWorkflowApplier{
		result: map[string]any{
			"id": "wpr-1", "status": "applied", "applied_commit": "abc1234",
		},
	}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(&stubProposalRepo{}),
		WithWorkflowApplier(stub),
	)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/apply", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if stub.lastID != "wpr-1" {
		t.Errorf("id not threaded: %q", stub.lastID)
	}
	if stub.lastAppliedBy != "sk-admin" {
		t.Errorf("applied_by should be admin key: %q", stub.lastAppliedBy)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("abc1234")) {
		t.Errorf("body should include commit SHA: %s", rec.Body.String())
	}
}

func TestAdminWorkflowProposalsItem_Apply_NotWired(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(&stubProposalRepo{}),
		// no WithWorkflowApplier
	)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/apply", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Apply_WrongMethod(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(&stubProposalRepo{}),
		WithWorkflowApplier(&stubWorkflowApplier{}),
	)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-proposals/wpr-1/apply", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on /apply: want 405, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Apply_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", persistence.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{"invalid transition", persistence.ErrInvalidProposalTransition, http.StatusConflict, "INVALID_TRANSITION"},
		{"not approved", errors.New("memetic: proposal must be approved before apply: current status=pending"),
			http.StatusConflict, "PROPOSAL_NOT_APPROVED"},
		{"generic", errors.New("disk full"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(
				WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
				WithWorkflowProposals(&stubProposalRepo{}),
				WithWorkflowApplier(&stubWorkflowApplier{err: tc.err}),
			)
			rec := httptest.NewRecorder()
			req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
				"/api/v1/admin/workflow-proposals/wpr-1/apply", nil), "sk-admin")
			s.AdminWorkflowProposalsItem(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("%s: want %d, got %d (body=%q)", tc.name, tc.status, rec.Code, rec.Body.String())
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(tc.code)) {
				t.Errorf("%s: error code %q missing from body %q", tc.name, tc.code, rec.Body.String())
			}
		})
	}
}

// ROLLBACK ------------------------------------------------------

type stubWorkflowRollbacker struct {
	lastID, lastBy string
	result         any
	err            error
}

func (s *stubWorkflowRollbacker) Rollback(_ context.Context, id, by string) (any, error) {
	s.lastID = id
	s.lastBy = by
	return s.result, s.err
}

func TestAdminWorkflowProposalsItem_Rollback_HappyPath(t *testing.T) {
	stub := &stubWorkflowRollbacker{
		result: map[string]any{"id": "wpr-1", "status": "rolled_back", "rollback_commit": "def5678"},
	}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(&stubProposalRepo{}),
		WithWorkflowRollbacker(stub),
	)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/rollback", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if stub.lastID != "wpr-1" || stub.lastBy != "sk-admin" {
		t.Errorf("threading: id=%q by=%q", stub.lastID, stub.lastBy)
	}
}

func TestAdminWorkflowProposalsItem_Rollback_NotWired(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(&stubProposalRepo{}),
	)
	rec := httptest.NewRecorder()
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-proposals/wpr-1/rollback", nil), "sk-admin")
	s.AdminWorkflowProposalsItem(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalsItem_Rollback_ErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", persistence.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{"invalid transition", persistence.ErrInvalidProposalTransition, http.StatusConflict, "INVALID_TRANSITION"},
		{"not applied", errors.New("memetic: proposal must be applied before rollback: current status=approved"),
			http.StatusConflict, "PROPOSAL_NOT_APPLIED"},
		{"generic", errors.New("repo down"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(
				WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
				WithWorkflowProposals(&stubProposalRepo{}),
				WithWorkflowRollbacker(&stubWorkflowRollbacker{err: tc.err}),
			)
			rec := httptest.NewRecorder()
			req := withAdminKeyContext(httptest.NewRequest(http.MethodPost,
				"/api/v1/admin/workflow-proposals/wpr-1/rollback", nil), "sk-admin")
			s.AdminWorkflowProposalsItem(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("%s: want %d, got %d", tc.name, tc.status, rec.Code)
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(tc.code)) {
				t.Errorf("%s: error code %q missing from body %q", tc.name, tc.code, rec.Body.String())
			}
		})
	}
}

// TestToWorkflowProposalJSON_NilSafeAndFormats — sanity-check the
// JSON projector. Empty EvidenceRunIDs renders as [] not null;
// nil DecidedAt collapses to empty string (omitempty).
func TestToWorkflowProposalJSON_NilSafeAndFormats(t *testing.T) {
	got := toWorkflowProposalJSON(&persistence.WorkflowProposal{
		ID: "x", WorkflowID: "y",
		Status:    persistence.WorkflowProposalStatusPending,
		CreatedAt: time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
	})
	if got.EvidenceRunIDs == nil || len(got.EvidenceRunIDs) != 0 {
		t.Errorf("evidence_run_ids should be empty slice, got %v", got.EvidenceRunIDs)
	}
	if got.DecidedAt != "" {
		t.Errorf("DecidedAt should collapse to empty string, got %q", got.DecidedAt)
	}
}
