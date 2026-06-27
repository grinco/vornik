package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// errStubRollup is the sentinel a stub rollup source returns to
// exercise the graceful heuristic-fallback path.
var errStubRollup = errors.New("stub rollup error")

// stubProposalsRepo is a minimal in-memory implementation. Only
// the methods the UI handlers touch are real; the rest return zero
// values so tests stay focused.
type stubProposalsRepo struct {
	mu         sync.Mutex
	rows       map[string]*persistence.WorkflowProposal
	listErr    error
	getErr     error
	decideErr  error
	lastDecide struct {
		id, decidedBy, notes string
		status               persistence.WorkflowProposalStatus
	}
	updateErr  error
	lastModify struct{ id, yaml, editedBy string }
}

func newStubProposalsRepo() *stubProposalsRepo {
	return &stubProposalsRepo{rows: map[string]*persistence.WorkflowProposal{}}
}

func (s *stubProposalsRepo) Insert(_ context.Context, p *persistence.WorkflowProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[p.ID] = p
	return nil
}

func (s *stubProposalsRepo) Get(_ context.Context, id string) (*persistence.WorkflowProposal, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return p, nil
}

func (s *stubProposalsRepo) List(_ context.Context, _ persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.WorkflowProposal, 0, len(s.rows))
	for _, p := range s.rows {
		out = append(out, p)
	}
	return out, nil
}

func (s *stubProposalsRepo) Decide(_ context.Context, id string, status persistence.WorkflowProposalStatus, decidedBy, notes string) error {
	if s.decideErr != nil {
		return s.decideErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastDecide.id = id
	s.lastDecide.status = status
	s.lastDecide.decidedBy = decidedBy
	s.lastDecide.notes = notes
	if p, ok := s.rows[id]; ok {
		p.Status = status
		p.DecidedBy = decidedBy
		p.Notes = notes
		now := time.Now().UTC()
		p.DecidedAt = &now
	}
	return nil
}

func (s *stubProposalsRepo) MarkApplied(_ context.Context, _, _ string) error    { return nil }
func (s *stubProposalsRepo) MarkRolledBack(_ context.Context, _, _ string) error { return nil }

func (s *stubProposalsRepo) UpdateProposalYAML(_ context.Context, id, yaml, editedBy string) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastModify.id = id
	s.lastModify.yaml = yaml
	s.lastModify.editedBy = editedBy
	if p, ok := s.rows[id]; ok {
		p.ProposalYAML = yaml
	}
	return nil
}

// TestAdminWorkflowProposals_NotWired — repo absent, page renders
// the empty state without 500-ing.
func TestAdminWorkflowProposals_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposals(rec, httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("empty-state message missing")
	}
}

// TestAdminWorkflowProposals_Renders pins the happy-path render:
// status pills, workflow column, and the filter form are present.
func TestAdminWorkflowProposals_Renders(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-pending", WorkflowID: "research",
		Status:         persistence.WorkflowProposalStatusPending,
		Motivation:     "found a retry loop in step3",
		Confidence:     0.78,
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		CreatedAt:      time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposals(rec, httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"research", "pending", "0.78", "retry loop"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestAdminWorkflowProposals_StatusFilter ensures the "all" status
// short-circuits filtering and a specific status drives the SQL filter.
func TestAdminWorkflowProposals_StatusFilter(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-1", WorkflowID: "wf-a",
		Status:    persistence.WorkflowProposalStatusPending,
		CreatedAt: time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/admin/workflow-proposals?status=all&workflow=wf-a", nil)
	s.AdminWorkflowProposals(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	// Selected option for "all" should be the active dropdown choice.
	if !strings.Contains(body, `value="all" selected`) {
		t.Errorf("status=all dropdown not selected, body=%q", body[:min(300, len(body))])
	}
}

// TestAdminWorkflowProposalDetail_HappyPath — drill-down renders
// the YAML body and the approve/reject form for a pending row.
func TestAdminWorkflowProposalDetail_HappyPath(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-detail", WorkflowID: "research",
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "---\nworkflowId: research\n---\nbody\n",
		Motivation:     "the motivation goes here",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		ArchitectModel: "qwen3.6:35b",
		Confidence:     0.81,
		CreatedAt:      time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-detail", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-detail")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"workflowId: research", "the motivation goes here",
		"qwen3.6:35b", "Approve", "Reject", "r-1", "r-2", "r-3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
}

// TestAdminWorkflowProposalDetail_DecidedHidesForm — once a row is
// no longer pending, the approve/reject form is hidden and a
// "cannot be re-decided" message renders instead.
func TestAdminWorkflowProposalDetail_DecidedHidesForm(t *testing.T) {
	repo := newStubProposalsRepo()
	decided := time.Now().UTC()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-decided", WorkflowID: "research",
		Status:       persistence.WorkflowProposalStatusApproved,
		ProposalYAML: "---\nworkflowId: research\n---\nbody\n",
		Motivation:   "motivation",
		DecidedAt:    &decided, DecidedBy: "operator-x",
		CreatedAt: time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDetail(rec, httptest.NewRequest(http.MethodGet,
		"/admin/workflow-proposals/wpr-decided", nil), "wpr-decided")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `value="approved"`) {
		t.Error("decided row should not render approve button")
	}
	if !strings.Contains(body, "No further action available") {
		t.Error("decided row should explain no action is available")
	}
}

// TestAdminWorkflowProposalDetail_DecideErrorQuery — when the
// detail page is reached via a redirect carrying ?decide_error=,
// the message renders as an inline banner.
func TestAdminWorkflowProposalDetail_DecideErrorQuery(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-err", WorkflowID: "wf-a",
		Status:     persistence.WorkflowProposalStatusPending,
		Motivation: "m",
		CreatedAt:  time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/admin/workflow-proposals/wpr-err?decide_error=already+decided", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-err")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Decide failed") || !strings.Contains(body, "already decided") {
		t.Errorf("error banner missing, body=%q", body[:min2(400, len(body))])
	}
}

// TestAdminWorkflowProposalDetail_NotWired — repo absent, renders
// the empty-state template.
func TestAdminWorkflowProposalDetail_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/x", nil)
	s.AdminWorkflowProposalDetail(rec, req, "x")
	if rec.Code != http.StatusOK {
		t.Fatalf("not wired: status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("empty-state message missing")
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestAdminWorkflowProposalDetail_NotFound — missing row maps to 404
// rather than rendering a partial template.
func TestAdminWorkflowProposalDetail_NotFound(t *testing.T) {
	repo := newStubProposalsRepo()
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/missing", nil)
	s.AdminWorkflowProposalDetail(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing row: want 404, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalDecide_PostApprove — POST {status:approved}
// transitions the row and redirects to the detail page.
func TestAdminWorkflowProposalDecide_PostApprove(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-1", WorkflowID: "wf-a",
		Status:    persistence.WorkflowProposalStatusPending,
		CreatedAt: time.Now().UTC(),
	})
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithAdminAuditRepository(audit),
	)
	form := strings.NewReader("status=approved&notes=ship+it")
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-1/decide", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, req, "wpr-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("redirect: want 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/wpr-1") {
		t.Errorf("location should land on detail: %q", loc)
	}
	if repo.lastDecide.status != persistence.WorkflowProposalStatusApproved {
		t.Errorf("status threaded: %q", repo.lastDecide.status)
	}
	if repo.lastDecide.notes != "ship it" {
		t.Errorf("notes: %q", repo.lastDecide.notes)
	}
	// Audit row should record the decision.
	if len(audit.rows) != 1 {
		t.Fatalf("audit entries: %d", len(audit.rows))
	}
	if audit.rows[0].Action != "workflow-proposal.approved" {
		t.Errorf("audit action: %q", audit.rows[0].Action)
	}
}

// stubWorkflowSourceUI is a fixed-YAML WorkflowSourceUI for the
// diff-panel tests.
type stubWorkflowSourceUI struct {
	yaml []byte
	err  error
}

func (s *stubWorkflowSourceUI) Load(context.Context, string) ([]byte, error) {
	return s.yaml, s.err
}

// TestAdminWorkflowProposalModify_PostUpdatesYAML — POST modify on a
// pending proposal threads the edited YAML to the repo, audits it,
// and redirects back to the detail page (§8.5 Modify button).
func TestAdminWorkflowProposalModify_PostUpdatesYAML(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-1", WorkflowID: "wf-a",
		Status:       persistence.WorkflowProposalStatusPending,
		ProposalYAML: "old: yaml\n",
		CreatedAt:    time.Now().UTC(),
	})
	audit := &stubAdminAuditRepo{}
	s := NewServer(WithWorkflowProposalsRepository(repo), WithAdminAuditRepository(audit))
	form := strings.NewReader("proposal_yaml=" + url.QueryEscape("new: yaml\nmore: true\n"))
	req := httptest.NewRequest(http.MethodPost, "/admin/workflow-proposals/wpr-1/modify", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalModify(rec, req, "wpr-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("redirect: want 303, got %d; body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastModify.id != "wpr-1" || repo.lastModify.yaml != "new: yaml\nmore: true" {
		t.Errorf("modify not threaded: %+v", repo.lastModify)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "workflow-proposal.modified" {
		t.Errorf("audit action: %+v", audit.rows)
	}
}

// TestAdminWorkflowProposalModify_EmptyRejected — blank YAML
// redirects back with an error rather than wiping the proposal.
func TestAdminWorkflowProposalModify_EmptyRejected(t *testing.T) {
	repo := newStubProposalsRepo()
	s := NewServer(WithWorkflowProposalsRepository(repo))
	form := strings.NewReader("proposal_yaml=" + url.QueryEscape("   "))
	req := httptest.NewRequest(http.MethodPost, "/admin/workflow-proposals/wpr-1/modify", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalModify(rec, req, "wpr-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want redirect, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "decide_error=") {
		t.Errorf("empty YAML should redirect with decide_error: %q", rec.Header().Get("Location"))
	}
	if repo.lastModify.id != "" {
		t.Error("empty YAML must not reach the repo")
	}
}

// TestAdminWorkflowProposalDetail_DiffPanel — a wired workflow source
// produces a populated diff + predicted-impact on the detail data.
func TestAdminWorkflowProposalDetail_DiffPanel(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-1", WorkflowID: "wf-a",
		Status:         persistence.WorkflowProposalStatusPending,
		Kind:           persistence.WorkflowProposalKindChangeTimeout,
		ProposalYAML:   "a\nB2\nc\n",
		Confidence:     0.8,
		EvidenceRunIDs: []string{"e1", "e2", "e3"},
		CreatedAt:      time.Now().UTC(),
	})
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowSourceUI(&stubWorkflowSourceUI{yaml: []byte("a\nb\nc\n")}),
	)
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-1", nil)
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDetail(rec, req, "wpr-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: want 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Diff panel header + the predicted-impact line render.
	if !strings.Contains(body, "Diff — current vs proposed") {
		t.Error("diff panel not rendered")
	}
	if !strings.Contains(body, "change timeout") {
		t.Error("predicted-impact summary not rendered")
	}
	// Modify editor is present for a pending proposal.
	if !strings.Contains(body, "/modify") {
		t.Error("modify form not rendered for pending proposal")
	}
}

// stubRollupSource is a fixed-rollup WorkflowRollupSource for the
// telemetry-backed predicted-impact tests.
type stubRollupSource struct {
	rollup *workflowtelemetry.Rollup
	err    error
	calls  int
	lastID string
}

func (s *stubRollupSource) ForWorkflow(_ context.Context, workflowID string, _ time.Time) (*workflowtelemetry.Rollup, error) {
	s.calls++
	s.lastID = workflowID
	return s.rollup, s.err
}

// TestAdminWorkflowProposalDetail_TelemetryBaseline — a wired rollup
// source populates + renders the "Current baseline" block with the
// workflow's real cost / failure-rate profile, framed honestly (not a
// forecast).
func TestAdminWorkflowProposalDetail_TelemetryBaseline(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-tb", WorkflowID: "research",
		Status:         persistence.WorkflowProposalStatusPending,
		Kind:           persistence.WorkflowProposalKindAddStep,
		ProposalYAML:   "a\nb\n",
		Motivation:     "m",
		Confidence:     0.9,
		EvidenceRunIDs: []string{"e1", "e2"},
		CreatedAt:      time.Now().UTC(),
	})
	src := &stubRollupSource{rollup: &workflowtelemetry.Rollup{
		WorkflowID:   "research",
		RunCount:     40,
		FailureCount: 10, // → 25.0%
		AvgCostUSD:   0.0123,
		TopFailureClasses: []workflowtelemetry.FailureClassCount{
			{ErrorClass: "schema_violation", Count: 7},
			{ErrorClass: "timeout", Count: 3},
		},
	}}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowRollupSource(src),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-tb", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-tb")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if src.calls != 1 || src.lastID != "research" {
		t.Fatalf("rollup source not called for workflow: calls=%d id=%q", src.calls, src.lastID)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Current baseline (last 30d)",
		"25.0%",   // failure rate
		"(10/40)", // failure count / run count
		"0.0123",  // avg cost/run
		"schema_violation",
		"×7",
		"Heuristic, not a forecast", // honest directional framing
		"not a simulated forecast",  // honest baseline framing
	} {
		if !strings.Contains(body, want) {
			t.Errorf("baseline block missing %q", want)
		}
	}
	// The heuristic one-liner still renders beneath the baseline.
	if !strings.Contains(body, "add step change") {
		t.Error("heuristic predicted-impact summary should still render beneath the baseline")
	}
}

// TestAdminWorkflowProposalDetail_NoRollupSource_HeuristicOnly — with
// no rollup source wired, the panel falls back to the heuristic
// summary and renders no baseline block.
func TestAdminWorkflowProposalDetail_NoRollupSource_HeuristicOnly(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-nh", WorkflowID: "research",
		Status:       persistence.WorkflowProposalStatusPending,
		Kind:         persistence.WorkflowProposalKindChangeTimeout,
		ProposalYAML: "a\n",
		Motivation:   "m",
		Confidence:   0.5,
		CreatedAt:    time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo)) // no rollup source
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-nh", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-nh")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Current baseline") {
		t.Error("no rollup source: baseline block must not render")
	}
	if !strings.Contains(body, "change timeout change") {
		t.Error("heuristic summary should render when no rollup source is wired")
	}
}

// TestAdminWorkflowProposalDetail_RollupZeroRuns_NoDivByZero — an
// empty-window rollup (RunCount 0) renders the "no runs in window"
// message instead of dividing by zero.
func TestAdminWorkflowProposalDetail_RollupZeroRuns_NoDivByZero(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-zr", WorkflowID: "research",
		Status:       persistence.WorkflowProposalStatusPending,
		ProposalYAML: "a\n",
		Motivation:   "m",
		CreatedAt:    time.Now().UTC(),
	})
	src := &stubRollupSource{rollup: &workflowtelemetry.Rollup{
		WorkflowID: "research", RunCount: 0,
	}}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowRollupSource(src),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-zr", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-zr")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No runs in window") {
		t.Error("zero-run rollup should render the no-runs message")
	}
	// No NaN / Inf leaked from a divide-by-zero.
	if strings.Contains(body, "NaN") || strings.Contains(body, "Inf") {
		t.Errorf("zero-run rollup leaked NaN/Inf: %q", body)
	}
}

// TestAdminWorkflowProposalDetail_RollupError_HeuristicFallback — a
// rollup fetch error degrades gracefully to the heuristic summary
// without erroring the page.
func TestAdminWorkflowProposalDetail_RollupError_HeuristicFallback(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-re", WorkflowID: "research",
		Status:       persistence.WorkflowProposalStatusPending,
		Kind:         persistence.WorkflowProposalKindRemoveStep,
		ProposalYAML: "a\n",
		Motivation:   "m",
		CreatedAt:    time.Now().UTC(),
	})
	src := &stubRollupSource{err: errStubRollup}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowRollupSource(src),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-re", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-re")
	if rec.Code != http.StatusOK {
		t.Fatalf("error fallback: status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Current baseline") {
		t.Error("rollup error: baseline block must not render")
	}
	if !strings.Contains(body, "remove step change") {
		t.Error("rollup error should fall back to the heuristic summary")
	}
}

// TestAdminWorkflowProposalDecide_InvalidStatus redirects back to
// the detail page with an error query param rather than 4xx-ing.
func TestAdminWorkflowProposalDecide_InvalidStatus(t *testing.T) {
	repo := newStubProposalsRepo()
	s := NewServer(WithWorkflowProposalsRepository(repo))
	form := strings.NewReader("status=applied")
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-1/decide", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, req, "wpr-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "decide_error=") {
		t.Errorf("location should carry decide_error: %q", rec.Header().Get("Location"))
	}
}

// TestAdminWorkflowProposalDecide_NotWired — repo absent, returns 503.
func TestAdminWorkflowProposalDecide_NotWired(t *testing.T) {
	s := NewServer()
	form := strings.NewReader("status=approved")
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-1/decide", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, req, "wpr-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalDecide_WrongMethod — GET on /decide is
// 405.
func TestAdminWorkflowProposalDecide_WrongMethod(t *testing.T) {
	repo := newStubProposalsRepo()
	s := NewServer(WithWorkflowProposalsRepository(repo))
	req := httptest.NewRequest(http.MethodGet,
		"/admin/workflow-proposals/wpr-1/decide", nil)
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, req, "wpr-1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", rec.Code)
	}
}

// APPLY ---------------------------------------------------------

type stubUIApplier struct {
	lastID, lastBy string
	err            error
}

func (s *stubUIApplier) Apply(_ context.Context, id, by string) (*persistence.WorkflowProposal, error) {
	s.lastID = id
	s.lastBy = by
	if s.err != nil {
		return nil, s.err
	}
	return &persistence.WorkflowProposal{
		ID: id, Status: persistence.WorkflowProposalStatusApplied,
		AppliedCommit: "abc1234",
	}, nil
}

func TestAdminWorkflowProposalApply_HappyPath(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-a", Status: persistence.WorkflowProposalStatusApproved,
		WorkflowID: "wf", CreatedAt: time.Now().UTC(),
	})
	applier := &stubUIApplier{}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowApplierUI(applier),
		WithAdminAuditRepository(audit),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-a/apply", nil)
	s.AdminWorkflowProposalApply(rec, req, "wpr-a")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("redirect: want 303, got %d", rec.Code)
	}
	if applier.lastID != "wpr-a" {
		t.Errorf("id: %q", applier.lastID)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "workflow-proposal.applied" {
		t.Errorf("audit not recorded: %+v", audit.rows)
	}
}

func TestAdminWorkflowProposalApply_NotWired(t *testing.T) {
	s := NewServer(WithWorkflowProposalsRepository(newStubProposalsRepo()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-a/apply", nil)
	s.AdminWorkflowProposalApply(rec, req, "wpr-a")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalApply_WrongMethod(t *testing.T) {
	s := NewServer(
		WithWorkflowProposalsRepository(newStubProposalsRepo()),
		WithWorkflowApplierUI(&stubUIApplier{}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/admin/workflow-proposals/wpr-a/apply", nil)
	s.AdminWorkflowProposalApply(rec, req, "wpr-a")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalApply_RepoError_Redirects(t *testing.T) {
	repo := newStubProposalsRepo()
	applier := &stubUIApplier{err: persistence.ErrInvalidProposalTransition}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowApplierUI(applier),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-a/apply", nil)
	s.AdminWorkflowProposalApply(rec, req, "wpr-a")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want redirect, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "decide_error=") {
		t.Errorf("location should carry error: %q", rec.Header().Get("Location"))
	}
}

// TestAdminWorkflowProposalDetail_CanApplyShowsButton — when the
// proposal is approved and an applier is wired, the detail page
// renders the Apply form.
func TestAdminWorkflowProposalDetail_CanApplyShowsButton(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-ca", Status: persistence.WorkflowProposalStatusApproved,
		WorkflowID: "wf", Motivation: "m", CreatedAt: time.Now().UTC(),
	})
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowApplierUI(&stubUIApplier{}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-ca", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-ca")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/wpr-ca/apply") {
		t.Error("Apply form should target the apply endpoint")
	}
	// The Apply button text appears between newlines + whitespace
	// inside the <button> tag; look for the surrounding form.
	if !strings.Contains(body, `action="/ui/admin/workflow-proposals/wpr-ca/apply"`) {
		t.Errorf("Apply form action missing from body=%q", body[max(0, strings.Index(body, "Apply")-50):min2(strings.Index(body, "Apply")+200, len(body))])
	}
}

// TestAdminWorkflowProposalDetail_NoApplier_HidesButton — no
// applier wired, the Apply form is hidden even for approved rows
// so the operator can't trigger a 503.
func TestAdminWorkflowProposalDetail_NoApplier_HidesButton(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-na", Status: persistence.WorkflowProposalStatusApproved,
		WorkflowID: "wf", Motivation: "m", CreatedAt: time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-na", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-na")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/apply\"") {
		t.Error("Apply form action should be hidden when applier isn't wired")
	}
}

// ROLLBACK ------------------------------------------------------

type stubUIRollbacker struct {
	lastID, lastBy string
	err            error
}

func (s *stubUIRollbacker) Rollback(_ context.Context, id, by string) (*persistence.WorkflowProposal, error) {
	s.lastID = id
	s.lastBy = by
	if s.err != nil {
		return nil, s.err
	}
	return &persistence.WorkflowProposal{
		ID: id, Status: persistence.WorkflowProposalStatusRolledBack,
		RollbackCommit: "def5678",
	}, nil
}

func TestAdminWorkflowProposalRollback_HappyPath(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-r", Status: persistence.WorkflowProposalStatusApplied,
		WorkflowID: "wf", AppliedCommit: "abc1234", CreatedAt: time.Now().UTC(),
	})
	rollbacker := &stubUIRollbacker{}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowRollbackerUI(rollbacker),
		WithAdminAuditRepository(audit),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-r/rollback", nil)
	s.AdminWorkflowProposalRollback(rec, req, "wpr-r")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("redirect: want 303, got %d", rec.Code)
	}
	if rollbacker.lastID != "wpr-r" {
		t.Errorf("id: %q", rollbacker.lastID)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "workflow-proposal.rolled_back" {
		t.Errorf("audit row: %+v", audit.rows)
	}
}

func TestAdminWorkflowProposalRollback_NotWired(t *testing.T) {
	s := NewServer(WithWorkflowProposalsRepository(newStubProposalsRepo()))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-r/rollback", nil)
	s.AdminWorkflowProposalRollback(rec, req, "wpr-r")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowProposalDetail_CanRollback_ShowsButton(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-cr", Status: persistence.WorkflowProposalStatusApplied,
		WorkflowID: "wf", AppliedCommit: "abc1234",
		Motivation: "m", CreatedAt: time.Now().UTC(),
	})
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowRollbackerUI(&stubUIRollbacker{}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-cr", nil)
	s.AdminWorkflowProposalDetail(rec, req, "wpr-cr")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/wpr-cr/rollback") {
		t.Errorf("Rollback form action missing from body")
	}
}

// TestAdminWorkflowProposalsRouter_Dispatch confirms adminRouter
// dispatches to the right handler for the four URL shapes
// (list / detail / decide / unknown subpath).
func TestAdminWorkflowProposalsRouter_Dispatch(t *testing.T) {
	repo := newStubProposalsRepo()
	_ = repo.Insert(context.Background(), &persistence.WorkflowProposal{
		ID: "wpr-r", WorkflowID: "research",
		Status:     persistence.WorkflowProposalStatusPending,
		Motivation: "m",
		CreatedAt:  time.Now().UTC(),
	})
	s := NewServer(WithWorkflowProposalsRepository(repo))

	// list
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals", nil)))
	if rec.Code != http.StatusOK {
		t.Errorf("list: %d", rec.Code)
	}

	// detail
	rec = httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/workflow-proposals/wpr-r", nil)))
	if rec.Code != http.StatusOK {
		t.Errorf("detail: %d", rec.Code)
	}

	// decide (POST)
	rec = httptest.NewRecorder()
	form := strings.NewReader("status=approved")
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-r/decide", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("decide: %d", rec.Code)
	}

	// unknown subpath → 404
	rec = httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet,
		"/admin/workflow-proposals/wpr-r/garbage", nil)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown subpath: want 404, got %d", rec.Code)
	}
}

// TestAdminWorkflowProposalDecide_RepoError — Decide returns a
// 409-class error; the user is redirected back with the message in
// the query string.
func TestAdminWorkflowProposalDecide_RepoError(t *testing.T) {
	repo := newStubProposalsRepo()
	repo.decideErr = persistence.ErrInvalidProposalTransition
	s := NewServer(WithWorkflowProposalsRepository(repo))
	form := strings.NewReader("status=approved")
	req := httptest.NewRequest(http.MethodPost,
		"/admin/workflow-proposals/wpr-1/decide", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, req, "wpr-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want redirect, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "decide_error=") {
		t.Errorf("error should round-trip via query, got %q", rec.Header().Get("Location"))
	}
}
