package ui

// Tests for the unified proposals inbox (Part B of the 2026-07-11
// control-plane actionable-proposals design, §5.2): architect/healing rows
// folded into the hub Proposals tab, the return_to=control-plane round-trip,
// source-filter slicing, nil-store (CE) degradation, the CE Diagnose
// caption, the latency-signal Diagnose deep-link, and the swarm-scope ack
// checkbox (§4.5).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func seedWorkflowProposal(repo *stubProposalsRepo, id, workflowID string, status persistence.WorkflowProposalStatus) {
	p := &persistence.WorkflowProposal{
		ID:         id,
		WorkflowID: workflowID,
		Status:     status,
		Motivation: "motivation for " + id,
		Confidence: 0.82,
		CreatedAt:  time.Now().UTC(),
	}
	_ = repo.Insert(context.Background(), p)
}

func getCPProposals(t *testing.T, s *Server, query string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/control-plane?section=proposals"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("hub GET status %d", rec.Code)
	}
	return rec.Body.String()
}

func TestCPWorkflowDisplayStatus_Mapping(t *testing.T) {
	cases := map[persistence.WorkflowProposalStatus]string{
		persistence.WorkflowProposalStatusPending:    persistence.ProposalStatusDraft,
		persistence.WorkflowProposalStatusApproved:   persistence.ProposalStatusApproved,
		persistence.WorkflowProposalStatusRejected:   persistence.ProposalStatusRejected,
		persistence.WorkflowProposalStatusApplied:    persistence.ProposalStatusApplied,
		persistence.WorkflowProposalStatusRolledBack: persistence.ProposalStatusRolledBack,
		persistence.WorkflowProposalStatusRegressed:  "REGRESSED",
	}
	for native, want := range cases {
		if got := cpWorkflowDisplayStatus(native); got != want {
			t.Errorf("cpWorkflowDisplayStatus(%s) = %q, want %q", native, got, want)
		}
	}
}

// Architect rows render with the source badge and post to the EXISTING
// workflow-proposal endpoints with the return_to hint; gates mirror the
// native surface (approve/reject pending-only, apply approved-only,
// rollback applied-only).
func TestAdminControlPlane_InboxArchitectRows(t *testing.T) {
	repo := newProposalRepoUI(t)
	wf := newStubProposalsRepo()
	seedWorkflowProposal(wf, "wfp-pend", "research", persistence.WorkflowProposalStatusPending)
	seedWorkflowProposal(wf, "wfp-appr", "research2", persistence.WorkflowProposalStatusApproved)
	seedWorkflowProposal(wf, "wfp-appl", "research3", persistence.WorkflowProposalStatusApplied)
	s := NewServer(
		WithProposalStore(repo),
		WithWorkflowProposalsRepository(wf),
		WithWorkflowApplierUI(&stubUIApplier{}),
		WithWorkflowRollbackerUI(&stubUIRollbacker{}),
	)
	body := getCPProposals(t, s, "")
	if !strings.Contains(body, ">architect</span>") {
		t.Error("architect source badge missing")
	}
	// Meta line: workflow id + confidence + motivation excerpt.
	for _, want := range []string{"research", "0.82", "motivation for wfp-pend"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Pending → decide forms (approve + reject) with the round-trip field.
	if !strings.Contains(body, "/ui/admin/workflow-proposals/wfp-pend/decide") {
		t.Error("pending architect row missing the decide form")
	}
	if !strings.Contains(body, `name="return_to" value="control-plane"`) {
		t.Error("hub action forms must carry return_to=control-plane")
	}
	// Approved → apply only; applied → rollback only.
	if !strings.Contains(body, "/ui/admin/workflow-proposals/wfp-appr/apply") {
		t.Error("approved architect row missing the apply form")
	}
	if strings.Contains(body, "/ui/admin/workflow-proposals/wfp-appr/decide") {
		t.Error("approved architect row must NOT offer approve/reject")
	}
	if !strings.Contains(body, "/ui/admin/workflow-proposals/wfp-appl/rollback") {
		t.Error("applied architect row missing the rollback form")
	}
	// Native detail link for depth.
	if !strings.Contains(body, `href="/ui/admin/workflow-proposals/wfp-pend"`) {
		t.Error("architect row missing the detail link")
	}
}

// REGRESSED rows render as a warning with NO action buttons — detail link
// only (design review #5).
func TestAdminControlPlane_InboxRegressedRowNoActions(t *testing.T) {
	repo := newProposalRepoUI(t)
	wf := newStubProposalsRepo()
	seedWorkflowProposal(wf, "wfp-reg", "flaky", persistence.WorkflowProposalStatusRegressed)
	s := NewServer(
		WithProposalStore(repo),
		WithWorkflowProposalsRepository(wf),
		WithWorkflowApplierUI(&stubUIApplier{}),
		WithWorkflowRollbackerUI(&stubUIRollbacker{}),
	)
	body := getCPProposals(t, s, "")
	if !strings.Contains(body, "REGRESSED") || !strings.Contains(body, "Regressed after apply") {
		t.Error("regressed warning state missing")
	}
	for _, action := range []string{"/decide", "/apply", "/rollback"} {
		if strings.Contains(body, "/ui/admin/workflow-proposals/wfp-reg"+action) {
			t.Errorf("regressed row must not offer the %s action", action)
		}
	}
	if !strings.Contains(body, `href="/ui/admin/workflow-proposals/wfp-reg"`) {
		t.Error("regressed row must keep the detail link")
	}
}

// A regressed proposal that a healing candidate references still shows no
// actions (promote included) and links the workflow-proposal detail page,
// where the native re-propose/rollback flow lives (review #5).
func TestAdminControlPlane_InboxRegressedHealingRowNoActions(t *testing.T) {
	repo := newProposalRepoUI(t)
	wf := newStubProposalsRepo()
	seedWorkflowProposal(wf, "wpr-9", "ingest", persistence.WorkflowProposalStatusRegressed)
	cands := newStubHealingCandidateRepoUI()
	seedCandidate(cands, "whc-r", persistence.HealingCandidateTrialPassed) // links wpr-9
	s := NewServer(
		WithProposalStore(repo),
		WithWorkflowProposalsRepository(wf),
		WithHealingCandidateRepository(cands),
		WithHealingCandidatePromoter(&stubPromoterUI{}),
		WithHealingTrialRunner(&stubTrialRunnerUI{}),
	)
	body := getCPProposals(t, s, "")
	if strings.Contains(body, "/ui/admin/blackbox/candidates/whc-r/promote") ||
		strings.Contains(body, "/ui/admin/blackbox/candidates/whc-r/run-trial") {
		t.Error("regressed healing row must not offer candidate actions")
	}
	if !strings.Contains(body, `href="/ui/admin/workflow-proposals/wpr-9"`) {
		t.Error("regressed healing row must link the workflow-proposal detail page")
	}
}

// A proposal referenced by a healing candidate renders with the healing
// badge, the latest trial verdict inline, and the candidate actions
// (promote/reject for trial_passed) posting to the existing blackbox
// endpoints — never the memetic decide forms.
func TestAdminControlPlane_InboxHealingRow(t *testing.T) {
	repo := newProposalRepoUI(t)
	wf := newStubProposalsRepo()
	seedWorkflowProposal(wf, "wpr-9", "ingest", persistence.WorkflowProposalStatusPending)
	cands := newStubHealingCandidateRepoUI()
	seedCandidate(cands, "whc-1", persistence.HealingCandidateTrialPassed) // links ProposalID wpr-9
	trials := newStubHealingTrialRepoUI()
	fin := time.Now().UTC()
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		ID: "wht-t1", CandidateID: "whc-1",
		Mode: persistence.HealingTrialModeReplay, Verdict: persistence.HealingTrialPassed,
		StartedAt: time.Now().UTC(), FinishedAt: &fin,
	})
	s := NewServer(
		WithProposalStore(repo),
		WithWorkflowProposalsRepository(wf),
		WithHealingCandidateRepository(cands),
		WithHealingTrialRepository(trials),
		WithHealingCandidatePromoter(&stubPromoterUI{}),
		WithHealingTrialRunner(&stubTrialRunnerUI{}),
	)
	body := getCPProposals(t, s, "")
	if !strings.Contains(body, ">healing</span>") {
		t.Error("healing source badge missing")
	}
	if !strings.Contains(body, "trial: passed (replay)") {
		t.Errorf("inline trial verdict missing: %s", body)
	}
	// trial_passed → promote + reject; run-trial hidden.
	if !strings.Contains(body, "/ui/admin/blackbox/candidates/whc-1/promote") {
		t.Error("trial_passed healing row missing the promote form")
	}
	if !strings.Contains(body, "/ui/admin/blackbox/candidates/whc-1/reject") {
		t.Error("trial_passed healing row missing the reject form")
	}
	if strings.Contains(body, "/ui/admin/blackbox/candidates/whc-1/run-trial") {
		t.Error("trial_passed candidate must not offer run-trial")
	}
	// Healing rows drive the candidate lifecycle — no memetic decide form.
	if strings.Contains(body, "/ui/admin/workflow-proposals/wpr-9/decide") {
		t.Error("healing row must not offer the memetic decide form")
	}
	// Detail link goes to the candidate page (scorecard depth).
	if !strings.Contains(body, `href="/ui/admin/blackbox/candidates/whc-1"`) {
		t.Error("healing row missing the candidate detail link")
	}
}

// Promote is gated to trial_passed: a draft candidate offers run-trial
// only; a trial_failed candidate offers run-trial again.
func TestAdminControlPlane_InboxHealingPromoteGate(t *testing.T) {
	for _, tc := range []struct {
		status persistence.HealingCandidateStatus
	}{{persistence.HealingCandidateDraft}, {persistence.HealingCandidateTrialFailed}} {
		repo := newProposalRepoUI(t)
		wf := newStubProposalsRepo()
		seedWorkflowProposal(wf, "wpr-9", "ingest", persistence.WorkflowProposalStatusPending)
		cands := newStubHealingCandidateRepoUI()
		seedCandidate(cands, "whc-g", tc.status)
		s := NewServer(
			WithProposalStore(repo),
			WithWorkflowProposalsRepository(wf),
			WithHealingCandidateRepository(cands),
			WithHealingCandidatePromoter(&stubPromoterUI{}),
			WithHealingTrialRunner(&stubTrialRunnerUI{}),
		)
		body := getCPProposals(t, s, "")
		if strings.Contains(body, "/ui/admin/blackbox/candidates/whc-g/promote") {
			t.Errorf("%s candidate must not offer promote", tc.status)
		}
		if !strings.Contains(body, "/ui/admin/blackbox/candidates/whc-g/run-trial") {
			t.Errorf("%s candidate should offer run-trial", tc.status)
		}
	}
}

// The source filter slices the unified list: an inbox source hides the
// control-plane rows and vice versa.
func TestAdminControlPlane_UnifiedSourceFilterSlicing(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "cp1", "tune failed-rate row", persistence.ProposalStatusDraft, false, "tune-detector")
	wf := newStubProposalsRepo()
	seedWorkflowProposal(wf, "wfp-arch", "research", persistence.WorkflowProposalStatusPending)
	seedWorkflowProposal(wf, "wpr-9", "ingest", persistence.WorkflowProposalStatusPending) // healing-linked
	cands := newStubHealingCandidateRepoUI()
	seedCandidate(cands, "whc-f", persistence.HealingCandidateTrialPassed)
	s := NewServer(
		WithProposalStore(repo),
		WithWorkflowProposalsRepository(wf),
		WithHealingCandidateRepository(cands),
	)

	// Filter options present.
	all := getCPProposals(t, s, "")
	if !strings.Contains(all, "source=architect") || !strings.Contains(all, "source=healing") {
		t.Error("architect/healing source filter options missing")
	}

	arch := getCPProposals(t, s, "&source=architect")
	if !strings.Contains(arch, "research") {
		t.Error("source=architect should show the architect row")
	}
	if strings.Contains(arch, "tune failed-rate row") || strings.Contains(arch, "ingest") {
		t.Error("source=architect must hide control-plane + healing rows")
	}

	heal := getCPProposals(t, s, "&source=healing")
	if !strings.Contains(heal, "ingest") {
		t.Error("source=healing should show the healing row")
	}
	if strings.Contains(heal, "tune failed-rate row") || strings.Contains(heal, "research<") {
		t.Error("source=healing must hide control-plane + architect rows")
	}

	cp := getCPProposals(t, s, "&source=tune-detector")
	if !strings.Contains(cp, "tune failed-rate row") {
		t.Error("source=tune-detector should show the ledger row")
	}
	if strings.Contains(cp, "research") || strings.Contains(cp, "ingest") {
		t.Error("a control-plane source selection must hide architect/healing rows")
	}
}

// CE nil-store degradation: without the memetic stores the tab renders
// ledger rows only, no errors, and the architect/healing options hide.
func TestAdminControlPlane_InboxNilStoreDegradation(t *testing.T) {
	repo := newProposalRepoUI(t)
	seedProposal(t, repo, "cp1", "ledger only row", persistence.ProposalStatusDraft, false, "tune-detector")
	s := NewServer(WithProposalStore(repo))
	body := getCPProposals(t, s, "")
	if !strings.Contains(body, "ledger only row") {
		t.Error("ledger row missing")
	}
	if strings.Contains(body, "source=architect") || strings.Contains(body, "source=healing") {
		t.Error("architect/healing filter options must hide when the stores are nil")
	}
	if strings.Contains(body, "failed to load") {
		t.Error("nil inbox stores must not surface an error")
	}
}

// CE caption (design §6.3): without a diagnoser the Diagnose tab does not
// render — a muted Enterprise caption takes its place — and a direct
// ?section=diagnose falls back to the Overview. With a diagnoser wired the
// tab is back and the caption gone.
func TestAdminControlPlane_DiagnoseTabCEGate(t *testing.T) {
	repo := newProposalRepoUI(t)
	ce := NewServer(WithProposalStore(repo))
	rec := httptest.NewRecorder()
	ce.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/control-plane", nil))
	body := rec.Body.String()
	if strings.Contains(body, "section=diagnose") {
		t.Error("CE hub must not link the Diagnose tab")
	}
	if !strings.Contains(body, "Diagnosis &amp; self-healing are Enterprise features.") {
		t.Error("CE hub missing the Enterprise caption in the tab bar")
	}
	// Direct deep-link falls back to the Overview.
	rec = httptest.NewRecorder()
	ce.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/control-plane?section=diagnose", nil))
	if !strings.Contains(rec.Body.String(), "open proposals awaiting review") {
		t.Error("?section=diagnose on CE should fall back to the Overview")
	}

	ee := NewServer(WithProposalStore(repo), WithDiagnoser(&fakeUIDiagnoser{}))
	rec = httptest.NewRecorder()
	ee.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/control-plane", nil))
	body = rec.Body.String()
	if !strings.Contains(body, "section=diagnose") {
		t.Error("EE hub should render the Diagnose tab")
	}
	if strings.Contains(body, "Diagnosis &amp; self-healing are Enterprise features.") {
		t.Error("EE hub must not render the upgrade caption")
	}
}

// Latency-signal ledger rows get a "Diagnose ↗" deep-link pre-filling the
// Diagnose tab with the project — only when the diagnoser is wired.
func TestAdminControlPlane_LatencyRowDiagnoseDeepLink(t *testing.T) {
	seedLatency := func(t *testing.T) persistence.ProposalRepository {
		repo := newProposalRepoUI(t)
		p := &persistence.ControlPlaneProposal{
			ID: "cpl-1", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
			BlastRadius: persistence.ProposalScopeProject, Title: "raise agent timeout",
			Status: persistence.ProposalStatusDraft, ProposedBy: "tune-detector",
			Evidence: `{"signal":"latency_p95_seconds","observed":42.1}`,
		}
		if err := repo.Create(context.Background(), p); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return repo
	}

	ee := NewServer(WithProposalStore(seedLatency(t)), WithDiagnoser(&fakeUIDiagnoser{}))
	body := getCPProposals(t, ee, "")
	if !strings.Contains(body, "Diagnose ↗") ||
		!strings.Contains(body, "section=diagnose&amp;focus=janka") {
		t.Errorf("latency row missing the Diagnose deep-link: %s", body)
	}
	// The deep-link pre-fills the Diagnose form.
	rec := httptest.NewRecorder()
	ee.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet, "/ui/admin/control-plane?section=diagnose&focus=janka", nil))
	if !strings.Contains(rec.Body.String(), `value="janka"`) {
		t.Error("Diagnose form should pre-fill focus from the query param")
	}

	ce := NewServer(WithProposalStore(seedLatency(t))) // no diagnoser
	if strings.Contains(getCPProposals(t, ce, ""), "Diagnose ↗") {
		t.Error("Diagnose deep-link must hide when the diagnoser is not wired")
	}
}

// §4.5: swarm blast radius needs the apply ack alongside daemon; project
// scope stays ack-free. The form field remains ackDaemon.
func TestAdminControlPlane_AckCheckboxDaemonAndSwarm(t *testing.T) {
	repo := newProposalRepoUI(t)
	mk := func(id, scope string) {
		p := &persistence.ControlPlaneProposal{
			ID: id, Kind: persistence.ProposalKindConfig, BlastRadius: scope,
			Title: "t-" + id, Status: persistence.ProposalStatusDraft,
			ProposedBy: "agent", ApplyTarget: "config.yaml", ApplyContent: "x: 1\n",
		}
		if scope != persistence.ProposalScopeDaemon {
			p.ProjectID = "janka"
		}
		if err := repo.Create(context.Background(), p); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		_ = repo.SetStatus(context.Background(), id, persistence.ProposalStatusApproved, "seed-approver")
	}
	mk("cps-swarm", persistence.ProposalScopeSwarm)
	mk("cps-daemon", persistence.ProposalScopeDaemon)
	mk("cps-proj", persistence.ProposalScopeProject)
	s := NewServer(WithProposalStore(repo), WithProposalApplier(&fakeUIApplier{}))
	body := getCPProposals(t, s, "")
	if !strings.Contains(body, "affects every project using this swarm") {
		t.Error("swarm-scope apply form missing the ack checkbox label")
	}
	if !strings.Contains(body, "affects all projects") {
		t.Error("daemon-scope apply form missing the ack checkbox label")
	}
	if got := strings.Count(body, `name="ackDaemon"`); got != 2 {
		t.Errorf("ack checkbox should render for daemon+swarm only, found %d", got)
	}
}

// --- return_to=control-plane round-trip (§5.2 #2) ----------------------

func postCPForm(t *testing.T, form url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestAdminWorkflowProposalDecide_ReturnToHub(t *testing.T) {
	repo := newStubProposalsRepo()
	seedWorkflowProposal(repo, "wfp-1", "research", persistence.WorkflowProposalStatusPending)
	s := NewServer(WithWorkflowProposalsRepository(repo))

	// Success → hub with the fixed flash token.
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, postCPForm(t, url.Values{
		"status": {"approved"}, "return_to": {"control-plane"},
	}), "wfp-1")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/control-plane?section=proposals&done=wf-approved" {
		t.Errorf("decide success redirect = %q", loc)
	}

	// Error → hub with the failure reason in action_error.
	repo.decideErr = errSentinel("proposal already decided")
	rec = httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, postCPForm(t, url.Values{
		"status": {"approved"}, "return_to": {"control-plane"},
	}), "wfp-1")
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/ui/admin/control-plane?section=proposals&action_error=") ||
		!strings.Contains(loc, "decide+failed") {
		t.Errorf("decide error redirect = %q", loc)
	}

	// Without the param the native redirect is unchanged.
	repo.decideErr = nil
	rec = httptest.NewRecorder()
	s.AdminWorkflowProposalDecide(rec, postCPForm(t, url.Values{"status": {"rejected"}}), "wfp-1")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/workflow-proposals/wfp-1" {
		t.Errorf("native decide redirect changed: %q", loc)
	}
}

func TestAdminWorkflowProposalApplyRollback_ReturnToHub(t *testing.T) {
	repo := newStubProposalsRepo()
	seedWorkflowProposal(repo, "wfp-2", "research", persistence.WorkflowProposalStatusApproved)
	applier := &stubUIApplier{}
	rollbacker := &stubUIRollbacker{}
	s := NewServer(
		WithWorkflowProposalsRepository(repo),
		WithWorkflowApplierUI(applier),
		WithWorkflowRollbackerUI(rollbacker),
	)

	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalApply(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "wfp-2")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/control-plane?section=proposals&done=wf-applied" {
		t.Errorf("apply success redirect = %q", loc)
	}

	applier.err = errSentinel("git commit failed")
	rec = httptest.NewRecorder()
	s.AdminWorkflowProposalApply(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "wfp-2")
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "action_error=apply+failed%3A+git+commit+failed") {
		t.Errorf("apply error redirect = %q", loc)
	}

	rec = httptest.NewRecorder()
	s.AdminWorkflowProposalRollback(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "wfp-2")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/control-plane?section=proposals&done=wf-rolled-back" {
		t.Errorf("rollback success redirect = %q", loc)
	}

	rollbacker.err = errSentinel("nothing applied")
	rec = httptest.NewRecorder()
	s.AdminWorkflowProposalRollback(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "wfp-2")
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "action_error=rollback+failed") {
		t.Errorf("rollback error redirect = %q", loc)
	}
}

func TestAdminBlackBoxCandidateActions_ReturnToHub(t *testing.T) {
	promoter := &stubPromoterUI{}
	runner := &stubTrialRunnerUI{}
	s := NewServer(
		WithHealingCandidatePromoter(promoter),
		WithHealingTrialRunner(runner),
	)

	// Promote success.
	rec := httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "whc-1")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/control-plane?section=proposals&done=candidate-promoted" {
		t.Errorf("promote success redirect = %q", loc)
	}

	// Promote refusal (design example message).
	promoter.promoteErr = ErrUICandidateNotPromotable
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "whc-1")
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "action_error=promote+refused%3A+candidate+not+trial_passed") {
		t.Errorf("promote refusal redirect = %q", loc)
	}
	// Without the param the native banner redirect is unchanged.
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidatePromote(rec, postCPForm(t, url.Values{}), "whc-1")
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/admin/blackbox/candidates/whc-1?action_error=") {
		t.Errorf("native promote redirect changed: %q", loc)
	}

	// Reject success.
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidateReject(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "whc-1")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/control-plane?section=proposals&done=candidate-rejected" {
		t.Errorf("reject success redirect = %q", loc)
	}

	// Run-trial success (static → sync path).
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidateRunTrial(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "whc-1")
	if loc := rec.Header().Get("Location"); loc != "/ui/admin/control-plane?section=proposals&done=trial-started" {
		t.Errorf("run-trial success redirect = %q", loc)
	}
	// Run-trial refusal carries the reason.
	runner.err = ErrUITrialRunning
	rec = httptest.NewRecorder()
	s.AdminBlackBoxCandidateRunTrial(rec, postCPForm(t, url.Values{"return_to": {"control-plane"}}), "whc-1")
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "action_error=a+trial+is+already+running") {
		t.Errorf("run-trial refusal redirect = %q", loc)
	}
}

// The hub surfaces the round-tripped flash + error banners.
func TestAdminControlPlane_HubFlashAndActionError(t *testing.T) {
	repo := newProposalRepoUI(t)
	s := NewServer(WithProposalStore(repo))

	rec := httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet,
		"/ui/admin/control-plane?section=proposals&done=wf-approved", nil))
	if !strings.Contains(rec.Body.String(), "Workflow proposal approved.") {
		t.Error("wf-approved flash missing")
	}

	rec = httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet,
		"/ui/admin/control-plane?section=proposals&action_error="+url.QueryEscape("promote refused: candidate not trial_passed"), nil))
	if !strings.Contains(rec.Body.String(), "promote refused: candidate not trial_passed") {
		t.Error("action_error banner missing")
	}
	// XSS: the free-text error must render escaped.
	rec = httptest.NewRecorder()
	s.AdminControlPlane(rec, httptest.NewRequest(http.MethodGet,
		"/ui/admin/control-plane?section=proposals&action_error="+url.QueryEscape("<script>alert(1)</script>"), nil))
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("action_error rendered unescaped — XSS hole")
	}
}
