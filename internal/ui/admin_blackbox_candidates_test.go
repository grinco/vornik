package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// --- in-memory stubs --------------------------------------------------

type stubHealingCandidateRepoUI struct {
	mu     sync.Mutex
	rows   map[string]*persistence.HealingCandidate
	getErr error
}

func newStubHealingCandidateRepoUI() *stubHealingCandidateRepoUI {
	return &stubHealingCandidateRepoUI{rows: map[string]*persistence.HealingCandidate{}}
}

func (s *stubHealingCandidateRepoUI) Insert(_ context.Context, c *persistence.HealingCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	s.rows[c.ID] = &cp
	return nil
}

func (s *stubHealingCandidateRepoUI) Get(_ context.Context, id string) (*persistence.HealingCandidate, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return c, nil
}

func (s *stubHealingCandidateRepoUI) List(_ context.Context, filter persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.HealingCandidate, 0, len(s.rows))
	for _, c := range s.rows {
		if filter.WorkflowID != "" && c.WorkflowID != filter.WorkflowID {
			continue
		}
		if filter.Status != "" && c.Status != filter.Status {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *stubHealingCandidateRepoUI) SetStatus(_ context.Context, id string, status persistence.HealingCandidateStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = status
	return nil
}

func (s *stubHealingCandidateRepoUI) BeginTrial(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		return false, nil
	}
	if c.Status.IsTerminal() || c.Status == persistence.HealingCandidateTrialRunning {
		return false, nil
	}
	c.Status = persistence.HealingCandidateTrialRunning
	return true, nil
}

func (s *stubHealingCandidateRepoUI) Promote(_ context.Context, id, promotedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = persistence.HealingCandidatePromoted
	c.PromotedBy = promotedBy
	return nil
}

func (s *stubHealingCandidateRepoUI) Reject(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = persistence.HealingCandidateRejected
	return nil
}

type stubHealingTrialRepoUI struct {
	mu      sync.Mutex
	byCand  map[string][]*persistence.HealingTrial
	listErr error
}

func newStubHealingTrialRepoUI() *stubHealingTrialRepoUI {
	return &stubHealingTrialRepoUI{byCand: map[string][]*persistence.HealingTrial{}}
}

func (s *stubHealingTrialRepoUI) Insert(_ context.Context, tr *persistence.HealingTrial) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCand[tr.CandidateID] = append(s.byCand[tr.CandidateID], tr)
	return nil
}

func (s *stubHealingTrialRepoUI) Get(_ context.Context, _ string) (*persistence.HealingTrial, error) {
	return nil, persistence.ErrNotFound
}

func (s *stubHealingTrialRepoUI) ListByCandidate(_ context.Context, candidateID string) ([]*persistence.HealingTrial, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byCand[candidateID], nil
}

func (s *stubHealingTrialRepoUI) Finish(_ context.Context, _ string, _ persistence.HealingTrialVerdict, _, _, _ string) error {
	return nil
}

type stubTrialRunnerUI struct {
	err        error
	lastMode   string
	lastCandID string
	calls      int
	asyncCalls int
}

func (s *stubTrialRunnerUI) RunTrial(_ context.Context, candidateID, mode string, _ []string) error {
	s.calls++
	s.lastMode = mode
	s.lastCandID = candidateID
	return s.err
}

func (s *stubTrialRunnerUI) RunTrialAsync(_ context.Context, candidateID, mode string, _ []string) error {
	s.asyncCalls++
	s.lastMode = mode
	s.lastCandID = candidateID
	return s.err
}

type stubPromoterUI struct {
	promoteErr error
	rejectErr  error
	promoted   int
	rejected   int
	returnCand *persistence.HealingCandidate
	lastPromBy string
}

func (s *stubPromoterUI) Promote(_ context.Context, _, promotedBy string) (*persistence.HealingCandidate, error) {
	if s.promoteErr != nil {
		return nil, s.promoteErr
	}
	s.promoted++
	s.lastPromBy = promotedBy
	if s.returnCand != nil {
		return s.returnCand, nil
	}
	return &persistence.HealingCandidate{WorkflowID: "wf", ProposalID: "wpr-1"}, nil
}

func (s *stubPromoterUI) Reject(_ context.Context, _ string) (*persistence.HealingCandidate, error) {
	if s.rejectErr != nil {
		return nil, s.rejectErr
	}
	s.rejected++
	if s.returnCand != nil {
		return s.returnCand, nil
	}
	return &persistence.HealingCandidate{WorkflowID: "wf"}, nil
}

func seedCandidate(repo *stubHealingCandidateRepoUI, id string, status persistence.HealingCandidateStatus) *persistence.HealingCandidate {
	c := &persistence.HealingCandidate{
		ID:             id,
		TriggerID:      "wht-1",
		ProjectID:      "proj-a",
		WorkflowID:     "ingest",
		ProposalID:     "wpr-9",
		CandidateClass: persistence.HealingCandidateRetryBudget,
		RiskLevel:      persistence.HealingRiskMedium,
		Motivation:     "retry storm on the fetch step burned budget without recovering",
		ExpectedEffect: "cap retries at 2 and fail fast to the reviewer",
		Status:         status,
		CreatedAt:      time.Now().UTC(),
	}
	_ = repo.Insert(context.Background(), c)
	return c
}

// --- list tests -------------------------------------------------------

func TestAdminBlackBoxCandidates_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidates(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired on this deployment") {
		t.Errorf("expected not-wired empty state, got: %s", rec.Body.String())
	}
}

func TestAdminBlackBoxCandidates_ListAndStatusFilter(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-passed", persistence.HealingCandidateTrialPassed)
	c2 := seedCandidate(repo, "whc-draft", persistence.HealingCandidateDraft)
	c2.WorkflowID = "ingest"
	s := NewServer(WithHealingCandidateRepository(repo))

	// Unfiltered: both rows render.
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidates(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "whc-passed") || !strings.Contains(body, "whc-draft") {
		t.Errorf("unfiltered list missing rows: %s", body)
	}

	// Filtered to trial_passed: only the passed row renders.
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidates(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates?status=trial_passed", nil))
	body = rec.Body.String()
	if !strings.Contains(body, "whc-passed") {
		t.Errorf("filtered list missing passed row: %s", body)
	}
	if strings.Contains(body, "whc-draft") {
		t.Errorf("filtered list leaked the draft row: %s", body)
	}
}

// --- detail tests -----------------------------------------------------

func TestAdminBlackBoxCandidateDetail_404OnMissing(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	s := NewServer(WithHealingCandidateRepository(repo))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates/nope", nil), "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestAdminBlackBoxCandidateDetail_RendersTrustSurface(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateTrialPassed)
	trials := newStubHealingTrialRepoUI()
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		ID:               "wht-trial-1",
		CandidateID:      "whc-1",
		Mode:             persistence.HealingTrialModeReplay,
		Verdict:          persistence.HealingTrialPassed,
		BaselineSummary:  `{"runs":5,"successes":2,"avg_cost_usd":0.10,"avg_duration_seconds":12,"hallucination_rate":0.3,"verifier_failure_rate":0.2}`,
		CandidateSummary: `{"runs":5,"successes":5,"avg_cost_usd":0.08,"avg_duration_seconds":10,"hallucination_rate":0.0,"verifier_failure_rate":0.0}`,
		Scorecard:        `{"success_delta":0.6,"cost_delta_pct":-0.2,"latency_delta_pct":-0.16,"hallucination_delta":-0.3,"verifier_delta":-0.2,"verdict":"passed","reasons":["success rate improved","cost dropped"],"inconclusive":false}`,
		StartedAt:        time.Now().UTC(),
	})
	trig := newStubHealingTriggerRepo()
	_ = trig.Insert(context.Background(), &persistence.HealingTrigger{ID: "wht-1", EvidenceExecutionIDs: []string{"exec-aaa", "exec-bbb"}})

	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingTrialRepository(trials),
		WithHealingTriggerRepository(trig),
		WithHealingCandidatePromoter(&stubPromoterUI{}),
		WithHealingTrialRunner(&stubTrialRunnerUI{}),
	)
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates/whc-1", nil), "whc-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"retry storm on the fetch step", // motivation
		"cap retries at 2",              // expected effect
		"exec-aaa", "exec-bbb",          // evidence links
		"Success rate", "60.0%", // scorecard delta ("+" is html-escaped to &#43;)
		"Promote to production", // promote form (trial_passed)
		"Run trial",             // run-trial form
		"Reject candidate",      // reject form
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
}

func TestAdminBlackBoxCandidateDetail_InconclusiveCaveatShown(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-inc", persistence.HealingCandidateTrialFailed)
	trials := newStubHealingTrialRepoUI()
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		ID:          "wht-inc",
		CandidateID: "whc-inc",
		Mode:        persistence.HealingTrialModeReplay,
		Verdict:     persistence.HealingTrialInconclusive,
		Scorecard:   `{"success_delta":0,"inconclusive":true,"inconclusive_reason":"42% of tool calls were stubbed"}`,
		StartedAt:   time.Now().UTC(),
	})
	s := NewServer(WithHealingCandidateRepository(repo), WithHealingTrialRepository(trials))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates/whc-inc", nil), "whc-inc")
	body := rec.Body.String()
	if !strings.Contains(body, "Replay-mode limitation") {
		t.Errorf("missing replay-mode caveat banner: %s", body)
	}
	if !strings.Contains(body, "42% of tool calls were stubbed") {
		t.Errorf("missing inconclusive reason")
	}
	// A failed candidate must NOT show the promote button.
	if strings.Contains(body, "Promote to production") {
		t.Errorf("promote button leaked for a non-passed candidate")
	}
}

// TestAdminBlackBoxCandidateDetail_StaticPassDoesNotUnlockPromotion:
// regression for the 2026-06-06 operator confusion — a candidate whose
// static trial PASSED still sits at draft (non-promotable by design),
// but the page said "promotion is unavailable until a trial passes",
// which reads as a contradiction next to a passed trial row. The copy
// must say a REPLAY pass is required and that a static pass doesn't
// unlock promotion.
func TestAdminBlackBoxCandidateDetail_StaticPassDoesNotUnlockPromotion(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-static", persistence.HealingCandidateDraft)
	trials := newStubHealingTrialRepoUI()
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		ID:          "wht-static",
		CandidateID: "whc-static",
		Mode:        persistence.HealingTrialModeStatic,
		Verdict:     persistence.HealingTrialPassed,
		Scorecard:   `{"verdict":"passed","reasons":["candidate workflow validates clean (static shape + policy check passed)"]}`,
		StartedAt:   time.Now().UTC(),
	})
	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingTrialRepository(trials),
		WithHealingCandidatePromoter(&stubPromoterUI{}),
		WithHealingTrialRunner(&stubTrialRunnerUI{}),
	)
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates/whc-static", nil), "whc-static")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	// Draft candidate: promote button absent, replay-required copy present.
	if strings.Contains(body, "Promote to production") {
		t.Errorf("promote button leaked for a draft (static-pass) candidate")
	}
	if !strings.Contains(body, "<strong>replay</strong> trial passes") {
		t.Errorf("missing replay-required promotion copy")
	}
	if !strings.Contains(body, "does not unlock promotion") {
		t.Errorf("missing static-pass caveat in promotion copy")
	}
}

// --- run-trial tests --------------------------------------------------

func TestAdminBlackBoxCandidateRunTrial_PostAndAudit(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	runner := &stubTrialRunnerUI{}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingTrialRunner(runner),
		WithAdminAuditRepository(audit),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/run-trial", strings.NewReader("mode=replay"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	// Replay routes through the ASYNC path (real replays run minutes
	// past the request window); the sync path must not be hit.
	if runner.asyncCalls != 1 || runner.lastMode != "replay" {
		t.Errorf("async runner not invoked with mode=replay: asyncCalls=%d mode=%q", runner.asyncCalls, runner.lastMode)
	}
	if runner.calls != 0 {
		t.Errorf("sync RunTrial called %d times for a replay trial, want 0", runner.calls)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-candidate.trial-run" {
		t.Errorf("audit row missing/incorrect: %+v", audit.rows)
	}
}

// TestAdminBlackBoxCandidateRunTrial_StaticStaysSync: static trials are
// fast deterministic checks — they keep the synchronous path so the
// redirect lands with the verdict already recorded.
func TestAdminBlackBoxCandidateRunTrial_StaticStaysSync(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	runner := &stubTrialRunnerUI{}
	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingTrialRunner(runner),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/run-trial", strings.NewReader("mode=static"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	if runner.calls != 1 || runner.asyncCalls != 0 {
		t.Errorf("static trial: sync=%d async=%d, want 1/0", runner.calls, runner.asyncCalls)
	}
}

// TestAdminBlackBoxCandidateRunTrial_AlreadyRunningBanner: a live
// concurrent trial redirects with the wait-for-verdict banner instead
// of double-spawning replays.
func TestAdminBlackBoxCandidateRunTrial_AlreadyRunningBanner(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateTrialRunning)
	runner := &stubTrialRunnerUI{err: ErrUITrialRunning}
	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingTrialRunner(runner),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/run-trial", strings.NewReader("mode=replay"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "already+running") && !strings.Contains(loc, "already%20running") {
		t.Errorf("redirect %q missing already-running banner", loc)
	}
}

func TestAdminBlackBoxCandidateRunTrial_GET405(t *testing.T) {
	s := NewServer(WithHealingTrialRunner(&stubTrialRunnerUI{}))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateRunTrial(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates/x/run-trial", nil), "x")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
}

func TestAdminBlackBoxCandidateRunTrial_NotWired503(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateRunTrial(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/x/run-trial", nil), "x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

// --- promote tests ----------------------------------------------------

func TestAdminBlackBoxCandidatePromote_PostAndAudit(t *testing.T) {
	prom := &stubPromoterUI{returnCand: &persistence.HealingCandidate{WorkflowID: "ingest", ProposalID: "wpr-9", TriggerID: "wht-1"}}
	audit := &stubAdminAuditRepo{}
	s := NewServer(WithHealingCandidatePromoter(prom), WithAdminAuditRepository(audit))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/promote", nil), "whc-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	if prom.promoted != 1 {
		t.Errorf("promote not invoked")
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-candidate.promoted" {
		t.Errorf("audit row missing/incorrect: %+v", audit.rows)
	}
}

func TestAdminBlackBoxCandidatePromote_NotPromotableBanner(t *testing.T) {
	prom := &stubPromoterUI{promoteErr: ErrUICandidateNotPromotable}
	s := NewServer(WithHealingCandidatePromoter(prom))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/promote", nil), "whc-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "trial_passed") {
		t.Errorf("expected not-promotable banner in redirect, got %q", loc)
	}
}

func TestAdminBlackBoxCandidatePromote_GET405(t *testing.T) {
	s := NewServer(WithHealingCandidatePromoter(&stubPromoterUI{}))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates/x/promote", nil), "x")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
}

func TestAdminBlackBoxCandidatePromote_NotWired503(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/x/promote", nil), "x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

// --- reject tests -----------------------------------------------------

func TestAdminBlackBoxCandidateReject_PostAndAudit(t *testing.T) {
	prom := &stubPromoterUI{returnCand: &persistence.HealingCandidate{WorkflowID: "ingest"}}
	audit := &stubAdminAuditRepo{}
	s := NewServer(WithHealingCandidatePromoter(prom), WithAdminAuditRepository(audit))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/reject", nil), "whc-1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	if prom.rejected != 1 {
		t.Errorf("reject not invoked")
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "blackbox-candidate.rejected" {
		t.Errorf("audit row missing/incorrect: %+v", audit.rows)
	}
}

func TestAdminBlackBoxCandidateReject_TerminalBanner(t *testing.T) {
	prom := &stubPromoterUI{rejectErr: ErrUICandidateTerminal}
	s := NewServer(WithHealingCandidatePromoter(prom))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/x/reject", nil), "x")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "already+promoted+or+rejected") {
		t.Errorf("expected terminal banner, got %q", rec.Header().Get("Location"))
	}
}

// --- helper / branch coverage -----------------------------------------

func TestBadgeAndFormatHelpers(t *testing.T) {
	// status badges — every arm.
	for _, st := range candidateStatusOptions() {
		if statusBadgeClass(st) == "" {
			t.Errorf("empty status badge for %q", st)
		}
	}
	if statusBadgeClass("weird") == "" {
		t.Errorf("default status badge must be non-empty")
	}
	// risk badges.
	for _, rk := range []string{"low", "medium", "high", "weird"} {
		if riskBadgeClass(rk) == "" {
			t.Errorf("empty risk badge for %q", rk)
		}
	}
	// verdict badges.
	for _, vd := range []string{"passed", "failed", "errored", "inconclusive", "pending"} {
		if verdictBadgeClass(vd) == "" {
			t.Errorf("empty verdict badge for %q", vd)
		}
	}
	// firstLine: multi-line + truncation.
	if got := firstLine("line one\nline two", 0); got != "line one" {
		t.Errorf("firstLine multiline = %q", got)
	}
	if got := firstLine("abcdefgh", 3); got != "abc…" {
		t.Errorf("firstLine truncation = %q", got)
	}
	// signed formatters: negative branch.
	if got := fmtSignedPct(-0.1); got != "-10.0%" {
		t.Errorf("fmtSignedPct(-0.1) = %q", got)
	}
	if got := fmtSignedFloat(-0.5); got != "-0.50" {
		t.Errorf("fmtSignedFloat(-0.5) = %q", got)
	}
	// trialSummaryView: empty + invalid → nil.
	if trialSummaryView("") != nil || trialSummaryView("{}") != nil || trialSummaryView("not json") != nil {
		t.Errorf("trialSummaryView should be nil for empty/invalid blobs")
	}
	// scoreLinesFromScorecard: empty + invalid → no lines.
	if lines, _, _, _ := scoreLinesFromScorecard("{}"); lines != nil {
		t.Errorf("scoreLines empty blob should yield nil")
	}
	if lines, _, _, _ := scoreLinesFromScorecard("not json"); lines != nil {
		t.Errorf("scoreLines invalid blob should yield nil")
	}
}

func TestCandidateDetailURL_Truncation(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := candidateDetailURL("whc-1", long)
	if !strings.Contains(got, "action_error=") {
		t.Errorf("missing action_error: %q", got)
	}
	// fmtSignedFloat positive branch.
	if got := fmtSignedFloat(0.5); got != "+0.50" {
		t.Errorf("fmtSignedFloat(0.5) = %q", got)
	}
}

func TestAdminBlackBoxCandidates_ListErrorRendersEmpty(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	repo.getErr = nil // List has no error knob; simulate via a closed-over wrapper below
	s := NewServer(WithHealingCandidateRepository(&listErrCandidateRepo{}))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidates(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/candidates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	// List failed → no rows, but page still renders the table shell / empty state.
	if !strings.Contains(rec.Body.String(), "No candidates yet") {
		t.Errorf("list error should degrade to empty state")
	}
}

func TestAdminBlackBoxCandidateDetail_GetError500(t *testing.T) {
	s := NewServer(WithHealingCandidateRepository(&getErrCandidateRepo{}))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "x")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500", rec.Code)
	}
}

func TestAdminBlackBoxCandidateDetail_TriggerAndTrialErrorsDegrade(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	trig := newStubHealingTriggerRepo()
	trig.getErr = errSentinelBoom // trigger lookup fails → no evidence, page still renders
	trials := newStubHealingTrialRepoUI()
	trials.listErr = errSentinelBoom // trial list fails → empty trials, page still renders
	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingTriggerRepository(trig),
		WithHealingTrialRepository(trials),
	)
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/whc-1", nil), "whc-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (degraded render)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No trials yet") {
		t.Errorf("trial-list error should degrade to empty state")
	}
}

// listErrCandidateRepo / getErrCandidateRepo are minimal repos that error
// on List / Get respectively, to exercise the degraded-render branches.
type listErrCandidateRepo struct{ stubHealingCandidateRepoUI }

func (r *listErrCandidateRepo) List(_ context.Context, _ persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	return nil, errSentinelBoom
}

type getErrCandidateRepo struct{ stubHealingCandidateRepoUI }

func (r *getErrCandidateRepo) Get(_ context.Context, _ string) (*persistence.HealingCandidate, error) {
	return nil, errSentinelBoom
}

func TestAdminBlackBoxCandidateDetail_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateDetail(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "x")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not wired on this deployment") {
		t.Fatalf("not-wired detail: code=%d", rec.Code)
	}
}

func TestAdminBlackBoxCandidateRunTrial_ErrorPaths(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantLoc  string
	}{
		{"not-found", ErrUICandidateNotFound, http.StatusNotFound, ""},
		{"terminal", ErrUICandidateTerminal, http.StatusSeeOther, "terminal"},
		{"bad-mode", ErrUITrialMode, http.StatusSeeOther, "unsupported+trial+mode"},
		{"unknown", errSentinelBoom, http.StatusSeeOther, "trial+run+failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(WithHealingCandidateRepository(repo), WithHealingTrialRunner(&stubTrialRunnerUI{err: tc.err}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/x/run-trial", nil)
			s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if tc.wantLoc != "" && !strings.Contains(rec.Header().Get("Location"), tc.wantLoc) {
				t.Errorf("redirect %q missing %q", rec.Header().Get("Location"), tc.wantLoc)
			}
		})
	}
}

func TestAdminBlackBoxCandidatePromote_NotFoundAndUnknown(t *testing.T) {
	s := NewServer(WithHealingCandidatePromoter(&stubPromoterUI{promoteErr: ErrUICandidateNotFound}))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodPost, "/x/promote", nil), "x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not-found promote code=%d", rec.Code)
	}
	s = NewServer(WithHealingCandidatePromoter(&stubPromoterUI{promoteErr: errSentinelBoom}))
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodPost, "/x/promote", nil), "x")
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "promotion+failed") {
		t.Fatalf("unknown promote err: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	// terminal banner
	s = NewServer(WithHealingCandidatePromoter(&stubPromoterUI{promoteErr: ErrUICandidateTerminal}))
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, httptest.NewRequest(http.MethodPost, "/x/promote", nil), "x")
	if !strings.Contains(rec.Header().Get("Location"), "already+promoted+or+rejected") {
		t.Errorf("terminal promote banner missing: %q", rec.Header().Get("Location"))
	}
}

func TestAdminBlackBoxCandidateReject_ErrorPaths(t *testing.T) {
	// not found
	s := NewServer(WithHealingCandidatePromoter(&stubPromoterUI{rejectErr: ErrUICandidateNotFound}))
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, httptest.NewRequest(http.MethodPost, "/x/reject", nil), "x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not-found reject code=%d", rec.Code)
	}
	// unknown
	s = NewServer(WithHealingCandidatePromoter(&stubPromoterUI{rejectErr: errSentinelBoom}))
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, httptest.NewRequest(http.MethodPost, "/x/reject", nil), "x")
	if !strings.Contains(rec.Header().Get("Location"), "reject+failed") {
		t.Errorf("unknown reject banner missing: %q", rec.Header().Get("Location"))
	}
	// GET 405 + not-wired 503
	s = NewServer(WithHealingCandidatePromoter(&stubPromoterUI{}))
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, httptest.NewRequest(http.MethodGet, "/x/reject", nil), "x")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reject code=%d want 405", rec.Code)
	}
	s = NewServer()
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, httptest.NewRequest(http.MethodPost, "/x/reject", nil), "x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-wired reject code=%d want 503", rec.Code)
	}
}

var errSentinelBoom = errSentinel("boom")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// --- router smoke test ------------------------------------------------

func TestAdminRouter_CandidatesDispatch(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-router", persistence.HealingCandidateTrialPassed)
	s := NewServer(
		WithHealingCandidateRepository(repo),
		WithHealingCandidatePromoter(&stubPromoterUI{}),
		WithHealingTrialRunner(&stubTrialRunnerUI{}),
	)
	// list
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/blackbox/candidates", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "whc-router") {
		t.Fatalf("list dispatch failed: code=%d", rec.Code)
	}
	// detail
	rec = httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/blackbox/candidates/whc-router", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Healing candidate") {
		t.Fatalf("detail dispatch failed: code=%d", rec.Code)
	}
	// reject action
	rec = httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(httptest.NewRequest(http.MethodPost, "/admin/blackbox/candidates/whc-router/reject", nil)))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reject dispatch failed: code=%d", rec.Code)
	}
}
