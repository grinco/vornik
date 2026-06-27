package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubHealingTriggerRepo is an in-memory implementation tailored for
// the UI handlers. Distinct from the postgres production repo — only
// the methods these handlers touch are real, the rest are stubs that
// return zero values so tests stay focused on UI behaviour.
type stubHealingTriggerRepo struct {
	mu               sync.Mutex
	rows             map[string]*persistence.HealingTrigger
	getErr           error
	dismissErr       error
	markGeneratedErr error
	lastDismiss      string
	lastMarkGen      struct {
		id, proposalID string
	}
}

func newStubHealingTriggerRepo() *stubHealingTriggerRepo {
	return &stubHealingTriggerRepo{rows: map[string]*persistence.HealingTrigger{}}
}

func (s *stubHealingTriggerRepo) Insert(_ context.Context, t *persistence.HealingTrigger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[t.ID] = t
	return nil
}

func (s *stubHealingTriggerRepo) Get(_ context.Context, id string) (*persistence.HealingTrigger, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return t, nil
}

func (s *stubHealingTriggerRepo) List(_ context.Context, filter persistence.HealingTriggerListFilter) ([]*persistence.HealingTrigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.HealingTrigger, 0, len(s.rows))
	for _, t := range s.rows {
		if filter.ProjectID != "" && t.ProjectID != filter.ProjectID {
			continue
		}
		if filter.WorkflowID != "" && t.WorkflowID != filter.WorkflowID {
			continue
		}
		if filter.Status != "" && t.Status != filter.Status {
			continue
		}
		if filter.TriggerClass != "" && t.TriggerClass != filter.TriggerClass {
			continue
		}
		out = append(out, t)
	}
	// Newest first matches the postgres ORDER BY created_at DESC.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (s *stubHealingTriggerRepo) Dismiss(_ context.Context, id string) error {
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
	now := time.Now().UTC()
	t.ResolvedAt = &now
	s.lastDismiss = id
	return nil
}

func (s *stubHealingTriggerRepo) MarkGenerated(_ context.Context, id, proposalID string) error {
	if s.markGeneratedErr != nil {
		return s.markGeneratedErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rows[id]
	if !ok || t.Status != persistence.HealingTriggerStatusOpen {
		return persistence.ErrNotFound
	}
	t.Status = persistence.HealingTriggerStatusGeneratedCandidate
	t.ProposalID = proposalID
	now := time.Now().UTC()
	t.ResolvedAt = &now
	s.lastMarkGen.id = id
	s.lastMarkGen.proposalID = proposalID
	return nil
}

// stubArchitect satisfies MemeticArchitectUI without dragging the
// real memetic package + LLM provider in. Tests set either Proposal
// (happy path) or Err (failure path) and assert which workflow ID
// the handler passed through.
type stubArchitect struct {
	Proposal       *persistence.WorkflowProposal
	Err            error
	lastWorkflowID string
	lastEvidence   []string
	calls          int
}

func (s *stubArchitect) Propose(_ context.Context, workflowID string) (*persistence.WorkflowProposal, error) {
	s.calls++
	s.lastWorkflowID = workflowID
	return s.Proposal, s.Err
}

func (s *stubArchitect) ProposeWithEvidence(_ context.Context, workflowID string, evidenceRunIDs []string) (*persistence.WorkflowProposal, error) {
	s.calls++
	s.lastWorkflowID = workflowID
	s.lastEvidence = append([]string(nil), evidenceRunIDs...)
	return s.Proposal, s.Err
}

func openTrigger(id string) *persistence.HealingTrigger {
	now := time.Now().UTC()
	return &persistence.HealingTrigger{
		ID:                   id,
		ProjectID:            "proj-x",
		WorkflowID:           "wf-a",
		TriggerClass:         persistence.HealingTriggerFailureRateSpike,
		MetricName:           "failure_rate",
		BaselineStart:        now.Add(-7 * 24 * time.Hour),
		BaselineEnd:          now.Add(-24 * time.Hour),
		ComparisonStart:      now.Add(-24 * time.Hour),
		ComparisonEnd:        now,
		BaselineValue:        0.10,
		ComparisonValue:      0.18,
		ThresholdValue:       25.0,
		EvidenceExecutionIDs: []string{"exec-1", "exec-2", "exec-3"},
		Status:               persistence.HealingTriggerStatusOpen,
		CreatedAt:            now,
	}
}

// TestAdminBlackBoxTriggerDetail_NotWired — repo absent, page
// renders the "not wired" empty state without 500-ing.
func TestAdminBlackBoxTriggerDetail_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/x", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "x")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Error("empty-state message missing")
	}
}

// TestAdminBlackBoxTriggerDetail_NotFound — missing row maps to 404.
func TestAdminBlackBoxTriggerDetail_NotFound(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/missing", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing row: want 404, got %d", rec.Code)
	}
}

// TestAdminBlackBoxTriggerDetail_OpenRendersBothActions — open
// trigger with both repo + architect wired must render both action
// forms and the evidence list.
func TestAdminBlackBoxTriggerDetail_OpenRendersBothActions(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-1"))
	arch := &stubArchitect{}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/ht-1", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"wf-a", "exec-1", "exec-2", "exec-3",
		"failure_rate_spike", "Dismiss", "Generate candidate",
		"/ui/admin/blackbox/triggers/ht-1/dismiss",
		"/ui/admin/blackbox/triggers/ht-1/generate-candidate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestAdminBlackBoxTriggerDetail_OpenWithoutArchitect — Dismiss
// shows but Generate-candidate is hidden, replaced by an
// "architect not wired" hint.
func TestAdminBlackBoxTriggerDetail_OpenWithoutArchitect(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-2"))
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/ht-2", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-2")
	body := rec.Body.String()
	if !strings.Contains(body, "Dismiss") {
		t.Error("Dismiss form should still render")
	}
	if strings.Contains(body, "/generate-candidate") {
		t.Error("Generate-candidate form should be hidden when architect is unwired")
	}
	if !strings.Contains(body, "architect not wired") {
		t.Error("expected 'architect not wired' hint when wiring is absent")
	}
}

// TestAdminBlackBoxTriggerDetail_TerminalHidesForms — dismissed /
// generated_candidate rows render the metadata + a status note,
// no action forms.
func TestAdminBlackBoxTriggerDetail_TerminalHidesForms(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	tr := openTrigger("ht-3")
	tr.Status = persistence.HealingTriggerStatusGeneratedCandidate
	tr.ProposalID = "wpr-99"
	resolved := time.Now().UTC()
	tr.ResolvedAt = &resolved
	_ = repo.Insert(context.Background(), tr)
	arch := &stubArchitect{}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/ht-3", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-3")
	body := rec.Body.String()
	if strings.Contains(body, "/dismiss\"") || strings.Contains(body, "/generate-candidate\"") {
		t.Error("terminal trigger should not render action form actions")
	}
	if !strings.Contains(body, "wpr-99") {
		t.Error("proposal link should render for generated_candidate row")
	}
	if !strings.Contains(body, "candidate proposal was generated") {
		t.Error("status note missing for generated_candidate row")
	}
}

// TestAdminBlackBoxTriggerDetail_ActionErrorRoundTrip — the
// detail page surfaces ?action_error= as an inline banner so a
// failed POST redirect lands somewhere the operator can read.
func TestAdminBlackBoxTriggerDetail_ActionErrorRoundTrip(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-4"))
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/ui/admin/blackbox/triggers/ht-4?action_error=architect+failed", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-4")
	if !strings.Contains(rec.Body.String(), "architect failed") {
		t.Error("action_error banner should render")
	}
}

func TestAdminBlackBoxTriggerDetail_ActionErrorEscapesHTML(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-xss"))
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/ui/admin/blackbox/triggers/ht-xss?action_error=%3Cscript%3Ealert(1)%3C/script%3E", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-xss")
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("action_error rendered as raw script")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("escaped action_error banner missing")
	}
}

func TestBlackboxTriggerDetailURL_EscapesPathAndQuery(t *testing.T) {
	got := blackboxTriggerDetailURL("hb/../x", `bad"><script>alert(1)</script>`)
	if strings.Contains(got, "hb/../x") || strings.Contains(got, "<script>") || strings.Contains(got, `bad"`) {
		t.Fatalf("unsafe redirect URL: %q", got)
	}
	if !strings.Contains(got, "/hb%2F..%2Fx?") || !strings.Contains(got, "action_error=") {
		t.Fatalf("encoded URL missing expected parts: %q", got)
	}
}

// TestAdminBlackBoxTriggerDismiss_HappyPath — POST flips status,
// writes audit, redirects to detail.
func TestAdminBlackBoxTriggerDismiss_HappyPath(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-5"))
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithAdminAuditRepository(audit),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-5/dismiss", nil)
	s.AdminBlackBoxTriggerDismiss(rec, req, "ht-5")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/ui/admin/blackbox/triggers/ht-5" {
		t.Errorf("redirect: %q", loc)
	}
	if repo.lastDismiss != "ht-5" {
		t.Errorf("repo.Dismiss not invoked with the right id: %q", repo.lastDismiss)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-trigger.dismissed" {
		t.Errorf("audit row: %#v", audit.rows)
	}
}

// TestAdminBlackBoxTriggerDismiss_AlreadyTerminal — repo returns
// ErrNotFound (its sentinel for missing OR terminal), handler
// redirects with action_error rather than 404-ing the page.
func TestAdminBlackBoxTriggerDismiss_AlreadyTerminal(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	// Don't insert; Dismiss will hit ErrNotFound.
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-missing/dismiss", nil)
	s.AdminBlackBoxTriggerDismiss(rec, req, "ht-missing")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303 with error", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "action_error=") {
		t.Errorf("redirect should carry action_error: %q", loc)
	}
}

// TestAdminBlackBoxTriggerDismiss_MethodNotAllowed — GET returns 405.
func TestAdminBlackBoxTriggerDismiss_MethodNotAllowed(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/ui/admin/blackbox/triggers/ht-x/dismiss", nil)
	s.AdminBlackBoxTriggerDismiss(rec, req, "ht-x")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: %d, want 405", rec.Code)
	}
}

// TestAdminBlackBoxTriggersBulkDismiss_HappyPath — POST with three
// `ids` checkbox values dismisses each one and writes three audit
// rows. Redirects to the index page with no error param.
func TestAdminBlackBoxTriggersBulkDismiss_HappyPath(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	for _, id := range []string{"ht-a", "ht-b", "ht-c"} {
		tr := openTrigger(id)
		_ = repo.Insert(context.Background(), tr)
	}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithAdminAuditRepository(audit),
	)
	form := strings.NewReader("ids=ht-a&ids=ht-b&ids=ht-c")
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/bulk-dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/ui/admin/blackbox" {
		t.Errorf("redirect: %q, want bare /ui/admin/blackbox on full success", loc)
	}
	if len(audit.rows) != 3 {
		t.Fatalf("audit rows: %d, want 3", len(audit.rows))
	}
	for i, row := range audit.rows {
		if row.Action != "blackbox-trigger.dismissed" || row.Before != "bulk" {
			t.Errorf("audit row %d: action=%q before=%q", i, row.Action, row.Before)
		}
	}
}

// TestAdminBlackBoxTriggersBulkDismiss_NoSelection — empty form
// redirects with a clear "no triggers selected" banner.
func TestAdminBlackBoxTriggersBulkDismiss_NoSelection(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/bulk-dismiss", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "no+triggers+selected") {
		t.Errorf("redirect: %q, want no-selection banner", loc)
	}
}

// TestAdminBlackBoxTriggersBulkDismiss_PartialFailure — one ID
// missing, two ok. Handler must keep going, audit two successes,
// and surface the first error in the summary banner.
func TestAdminBlackBoxTriggersBulkDismiss_PartialFailure(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-x"))
	_ = repo.Insert(context.Background(), openTrigger("ht-y"))
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithAdminAuditRepository(audit),
	)
	form := strings.NewReader("ids=ht-x&ids=ht-missing&ids=ht-y")
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/bulk-dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminBlackBoxTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "action_error=") || !strings.Contains(loc, "dismissed+2+of+3") {
		t.Errorf("redirect missing partial-failure summary: %q", loc)
	}
	if len(audit.rows) != 2 {
		t.Errorf("audit rows: %d, want 2 (one per successful dismiss)", len(audit.rows))
	}
}

// TestAdminBlackBoxTriggersBulkDismiss_NotWired — 503 when repo absent.
func TestAdminBlackBoxTriggersBulkDismiss_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/bulk-dismiss", strings.NewReader("ids=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxTriggersBulkDismiss(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d, want 503", rec.Code)
	}
}

// TestAdminBlackBoxTriggerGenerateCandidate_HappyPath — POST calls
// architect, stamps proposal_id, redirects to proposal detail, writes
// audit. Asserts the architect saw the trigger's workflow ID.
func TestAdminBlackBoxTriggerGenerateCandidate_HappyPath(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-6"))
	arch := &stubArchitect{
		Proposal: &persistence.WorkflowProposal{ID: "wpr-7", WorkflowID: "wf-a"},
	}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
		WithAdminAuditRepository(audit),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-6/generate-candidate", nil)
	s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-6")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/workflow-proposals/wpr-7" {
		t.Errorf("redirect: %q", loc)
	}
	if arch.lastWorkflowID != "wf-a" {
		t.Errorf("architect saw workflow %q, want wf-a", arch.lastWorkflowID)
	}
	if got := strings.Join(arch.lastEvidence, ","); got != "exec-1,exec-2,exec-3" {
		t.Errorf("architect evidence = %q, want trigger evidence", got)
	}
	if repo.lastMarkGen.id != "ht-6" || repo.lastMarkGen.proposalID != "wpr-7" {
		t.Errorf("MarkGenerated args: %+v", repo.lastMarkGen)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-trigger.generated_candidate" {
		t.Errorf("audit: %#v", audit.rows)
	}
}

// TestAdminBlackBoxTriggerGenerateCandidate_NoArchitect — wiring
// absent yields 503 instead of a silent skip.
func TestAdminBlackBoxTriggerGenerateCandidate_NoArchitect(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-7"))
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-7/generate-candidate", nil)
	s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-7")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d, want 503", rec.Code)
	}
}

// TestAdminBlackBoxTriggerGenerateCandidate_ArchitectFails — the
// architect returning an error must NOT advance the trigger; the
// row stays open and the operator gets an action_error redirect.
func TestAdminBlackBoxTriggerGenerateCandidate_ArchitectFails(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-8"))
	arch := &stubArchitect{Err: errors.New("LLM timeout")}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-8/generate-candidate", nil)
	s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-8")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/ui/admin/blackbox/triggers/ht-8") || !strings.Contains(loc, "action_error=") {
		t.Errorf("redirect should land back on the trigger with action_error: %q", loc)
	}
	// Trigger must stay open — no MarkGenerated call.
	if repo.lastMarkGen.id != "" {
		t.Errorf("MarkGenerated should not have been called: %+v", repo.lastMarkGen)
	}
}

// TestAdminBlackBoxTriggerDetail_HistoryRendersPastTerminalRows —
// past triggers for the same (project, workflow) render in a
// firing-history panel. The current row must NOT appear in its
// own history list.
func TestAdminBlackBoxTriggerDetail_HistoryRendersPastTerminalRows(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	cur := openTrigger("ht-cur")
	_ = repo.Insert(context.Background(), cur)
	// One past dismissed row + one past generated_candidate row,
	// same (project, workflow).
	past1 := openTrigger("ht-past1")
	past1.Status = persistence.HealingTriggerStatusDismissed
	past1.CreatedAt = cur.CreatedAt.Add(-7 * 24 * time.Hour)
	_ = repo.Insert(context.Background(), past1)
	past2 := openTrigger("ht-past2")
	past2.Status = persistence.HealingTriggerStatusGeneratedCandidate
	past2.ProposalID = "wpr-old"
	past2.CreatedAt = cur.CreatedAt.Add(-14 * 24 * time.Hour)
	_ = repo.Insert(context.Background(), past2)
	// Different workflow — must NOT appear in history.
	other := openTrigger("ht-other")
	other.WorkflowID = "wf-different"
	_ = repo.Insert(context.Background(), other)
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/ht-cur", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-cur")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Firing history") {
		t.Error("history panel heading missing")
	}
	// Past rows present, current row not duplicated as history.
	for _, want := range []string{"ht-past1", "ht-past2"} {
		if !strings.Contains(body, want) {
			t.Errorf("history missing past row %q", want)
		}
	}
	// "ht-other" belongs to a different workflow — must not show.
	if strings.Contains(body, "ht-other") {
		t.Error("history bled across workflows: ht-other should not appear")
	}
	// Current row must not appear in its own history table. Slice
	// between "Firing history" and the next major section ("CanDismiss"
	// branch starts the Dismiss form, which references the current ID
	// in its action URL — we want to scan ONLY the history table body).
	historyStart := strings.Index(body, "Firing history")
	if historyStart < 0 {
		t.Fatal("history panel missing")
	}
	historyTableEnd := strings.Index(body[historyStart:], "</table>")
	if historyTableEnd < 0 {
		t.Fatal("history table never closed")
	}
	historySlice := body[historyStart : historyStart+historyTableEnd]
	if strings.Contains(historySlice, "ht-cur") {
		t.Error("current row should be filtered out of its own history table")
	}
}

// TestAdminBlackBoxTriggerDetail_HistoryHiddenWhenEmpty — the panel
// renders nothing (not a "no history" empty state) when this is the
// first trigger for the (project, workflow).
func TestAdminBlackBoxTriggerDetail_HistoryHiddenWhenEmpty(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-solo"))
	s := NewServer(WithHealingTriggerRepository(repo))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/triggers/ht-solo", nil)
	s.AdminBlackBoxTriggerDetail(rec, req, "ht-solo")
	if strings.Contains(rec.Body.String(), "Firing history") {
		t.Error("history panel should be hidden when there's no prior row")
	}
}

// TestAdminBlackBoxTriggerGenerateCandidate_AlreadyTerminal — the
// handler must refuse to advance a non-open trigger and redirect
// with a friendly action_error, regardless of architect wiring.
func TestAdminBlackBoxTriggerGenerateCandidate_AlreadyTerminal(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	tr := openTrigger("ht-9")
	tr.Status = persistence.HealingTriggerStatusDismissed
	_ = repo.Insert(context.Background(), tr)
	arch := &stubArchitect{
		Proposal: &persistence.WorkflowProposal{ID: "wpr-x", WorkflowID: "wf-a"},
	}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-9/generate-candidate", nil)
	s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-9")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	if arch.calls != 0 {
		t.Errorf("architect should not be called for terminal trigger; saw %d call(s)", arch.calls)
	}
}

// TestAdminBlackBoxTriggerGenerateCandidate_PersistsCandidateRow —
// regression pin for the 2026-06-06 empty-candidates-menu report: the
// UI generate path stamped the trigger + created the proposal but
// never inserted a workflow_healing_candidates row, so every candidate
// generated from the UI was invisible in
// /ui/admin/blackbox/candidates. The handler must persist the linking
// row (same shape as the API path, via
// workflowhealing.CandidateFromArchitectProposal).
func TestAdminBlackBoxTriggerGenerateCandidate_PersistsCandidateRow(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-7"))
	arch := &stubArchitect{
		Proposal: &persistence.WorkflowProposal{
			ID:           "wpr-8",
			WorkflowID:   "wf-a",
			ProposalYAML: "---\nworkflow_id: wf-a\n---\nbody",
			Motivation:   "telemetry says so",
		},
	}
	candRepo := newStubHealingCandidateRepoUI()
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
		WithHealingCandidateRepository(candRepo),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-7/generate-candidate", nil)
	s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-7")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303", rec.Code)
	}
	if len(candRepo.rows) != 1 {
		t.Fatalf("candidate rows = %d, want 1 (the linking row)", len(candRepo.rows))
	}
	for _, c := range candRepo.rows {
		if c.TriggerID != "ht-7" || c.ProposalID != "wpr-8" {
			t.Errorf("candidate links wrong: %+v", c)
		}
		if c.Status != persistence.HealingCandidateDraft {
			t.Errorf("status = %q, want draft", c.Status)
		}
	}
}

// TestAdminBlackBoxTriggerGenerateCandidate_CandidateRepoNil — the
// persist is best-effort: nil repo (pre-genome deployment) must not
// break the generate flow.
func TestAdminBlackBoxTriggerGenerateCandidate_CandidateRepoNil(t *testing.T) {
	repo := newStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), openTrigger("ht-8"))
	arch := &stubArchitect{
		Proposal: &persistence.WorkflowProposal{ID: "wpr-9", WorkflowID: "wf-a"},
	}
	s := NewServer(
		WithHealingTriggerRepository(repo),
		WithBlackBoxArchitect(arch),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/ui/admin/blackbox/triggers/ht-8/generate-candidate", nil)
	s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-8")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d, want 303 with nil candidate repo", rec.Code)
	}
}
