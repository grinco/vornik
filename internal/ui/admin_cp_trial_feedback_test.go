package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Operator report, 2026-09-16: "I clicked on 'start trial', got 'Trial
// started — the verdict lands on the candidate's trial history' — but I see
// no change from the proposals perspective, it still shows the 'run trial'
// button."
//
// Everything the operator saw was correct except what they were TOLD. The
// hub's Run trial posts no mode, so it runs a STATIC trial, synchronously; by
// the time the flash message appeared the trial had finished and passed, and a
// static pass deliberately leaves the candidate at draft — promotion requires
// a REPLAY-gated pass (self-healing-workflow-genome-design; trial.go's advance
// switch). The row is therefore still DRAFT and still offers Run trial, which
// is the designed outcome.
//
// The defects are that the hub could not say so:
//
//  1. THE MESSAGE DESCRIBED THE OTHER PATH. "Trial started — the verdict lands
//     on the candidate's trial history" is the async REPLAY notice. For a
//     synchronous static run the verdict already exists, and the operator was
//     told to go wait for something that had already happened. Tenet 4: a
//     control that cannot distinguish "examined and clean" from "never
//     examined" reports the first and means the second — here it reported
//     neither.
//
//  2. THE SEAM THREW THE ANSWER AWAY. HealingTrialRunnerUI.RunTrial returned
//     only an error, with a comment asserting "the ui handler only needs the
//     error … so the concrete verdict is not part of this seam". The
//     synchronous runner HAS the verdict; discarding it is what forced the
//     handler to guess, and is why the fix belongs at the interface rather
//     than in a smarter message.
//
//  3. THE HUB'S ONLY TRIAL WAS THE ONE THAT CANNOT ADVANCE THE CANDIDATE. The
//     detail page offers static|replay; the hub row hardcoded static. So from
//     the decision inbox the button was a permanent no-op on status — DRAFT
//     before, DRAFT after, however many times it is pressed.

// The static path must report the verdict it already has.
func TestRunTrial_StaticReportsItsVerdictAndSaysPromotionNeedsReplay(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	runner := &stubTrialRunnerUI{verdict: string(persistence.HealingTrialPassed)}
	s := NewServer(WithHealingCandidateRepository(repo), WithHealingTrialRunner(runner))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/run-trial",
		strings.NewReader("mode=static&return_to=control-plane"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")

	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "done=trial-started") {
		t.Error("a synchronous static trial reported the ASYNC notice: the operator is told to wait " +
			"for a verdict that already exists")
	}
	if !strings.Contains(loc, "done=trial-static-passed") {
		t.Errorf("Location=%q, want done=trial-static-passed", loc)
	}
	msg := cpFlashMessages["trial-static-passed"]
	if msg == "" {
		t.Fatal("no flash message for a passed static trial")
	}
	// The whole point of the message: say why the candidate is still draft.
	if !strings.Contains(strings.ToLower(msg), "replay") {
		t.Errorf("the static-pass message does not mention that promotion needs a replay trial; "+
			"without it the unchanged DRAFT row is inexplicable: %q", msg)
	}
}

// A failing static trial is a different operator action from a passing one,
// so it must not collapse into the same notice.
func TestRunTrial_StaticFailureIsNotReportedAsSuccess(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	runner := &stubTrialRunnerUI{verdict: string(persistence.HealingTrialFailed)}
	s := NewServer(WithHealingCandidateRepository(repo), WithHealingTrialRunner(runner))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/run-trial",
		strings.NewReader("mode=static&return_to=control-plane"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=trial-static-failed") {
		t.Errorf("Location=%q, want done=trial-static-failed", loc)
	}
}

// Replay IS async, so "started" remains the honest word there.
func TestRunTrial_ReplayStillReportsThatItStarted(t *testing.T) {
	repo := newStubHealingCandidateRepoUI()
	seedCandidate(repo, "whc-1", persistence.HealingCandidateDraft)
	runner := &stubTrialRunnerUI{}
	s := NewServer(WithHealingCandidateRepository(repo), WithHealingTrialRunner(runner))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/candidates/whc-1/run-trial",
		strings.NewReader("mode=replay&return_to=control-plane"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.AdminBlackBoxCandidateRunTrial(rec, req, "whc-1")

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=trial-replay-started") {
		t.Errorf("Location=%q, want done=trial-replay-started", loc)
	}
}

// The hub row must be able to start the trial that actually gates promotion.
// Without this the decision inbox's only action cannot change the state the
// same row displays.
func TestControlPlaneHub_HealingRowOffersTheReplayTrial(t *testing.T) {
	wf := newStubProposalsRepo()
	seedWorkflowProposal(wf, "wpr-9", "ingest", persistence.WorkflowProposalStatusPending)
	cands := newStubHealingCandidateRepoUI()
	seedCandidate(cands, "whc-f", persistence.HealingCandidateDraft)
	s := NewServer(
		// The hub renders "the proposal ledger is not wired" without the
		// control-plane store, whatever the inbox extensions hold.
		WithProposalStore(newProposalRepoUI(t)),
		WithWorkflowProposalsRepository(wf),
		WithHealingCandidateRepository(cands),
		WithHealingTrialRunner(&stubTrialRunnerUI{}),
	)
	body := getCPProposals(t, s, "&source=healing")

	if !strings.Contains(body, `name="mode"`) {
		t.Fatal("the hub healing row posts no trial mode, so it can only ever run static — " +
			"the one trial that by design leaves the candidate at draft")
	}
	if !strings.Contains(body, `value="replay"`) {
		t.Error("replay is not offered on the hub row; promotion is unreachable from the decision inbox")
	}
	if !strings.Contains(body, `value="static"`) {
		t.Error("static must stay available: it is the cheap shape check, and replay re-runs real evidence")
	}
}
