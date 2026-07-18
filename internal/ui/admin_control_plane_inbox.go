package ui

// Part B of the actionable-proposals design (LLD 2026-07-11
// control-plane-actionable-proposals §5.2): the hub Proposals tab is the
// SINGLE decision inbox. Alongside the control-plane ledger rows it lists
// the memetic architect's workflow proposals (badge "architect") and the
// self-healing candidates' proposals (badge "healing" — a healing candidate
// references its proposal via ProposalID). No schema or lifecycle change:
// each row keeps its native engine and its actions POST to the EXISTING
// endpoints (/ui/admin/workflow-proposals/{id}/decide|apply|rollback,
// /ui/admin/blackbox/candidates/{id}/run-trial|promote|reject) with a
// return_to=control-plane hint so the handlers round-trip back to the hub.
// Nil-store degradation (CE): with the workflow-proposal or healing stores
// unwired the tab renders control-plane rows only and the architect/healing
// filter options are hidden.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// Unified-inbox source keys (grow the existing ProposedBy filter, §5.2 #4).
const (
	cpSourceArchitect = "architect"
	cpSourceHealing   = "healing"
)

// cpStatusRegressed is the hub display state for a memetic proposal that
// regressed after apply. It is NOT a control-plane ledger status — regressed
// rows render as a warning with no action buttons; the native detail page
// owns the re-propose/rollback flow (design review #5).
const cpStatusRegressed = "REGRESSED"

// cpWorkflowDisplayStatus maps a memetic workflow-proposal status onto the
// hub's display states (§5.2 #1: pending→DRAFT, approved→APPROVED,
// rejected→REJECTED, applied→APPLIED, rolled_back→ROLLED_BACK,
// regressed→REGRESSED).
func cpWorkflowDisplayStatus(st persistence.WorkflowProposalStatus) string {
	switch st {
	case persistence.WorkflowProposalStatusPending:
		return persistence.ProposalStatusDraft
	case persistence.WorkflowProposalStatusApproved:
		return persistence.ProposalStatusApproved
	case persistence.WorkflowProposalStatusRejected:
		return persistence.ProposalStatusRejected
	case persistence.WorkflowProposalStatusApplied:
		return persistence.ProposalStatusApplied
	case persistence.WorkflowProposalStatusRolledBack:
		return persistence.ProposalStatusRolledBack
	case persistence.WorkflowProposalStatusRegressed:
		return cpStatusRegressed
	default:
		return strings.ToUpper(string(st))
	}
}

// buildCPInboxRows reads the memetic workflow-proposal store (and the
// healing-candidate store for the source split + trial receipts) and
// returns the architect/healing rows for the unified inbox. Degrades to
// nothing on a nil store or a read error — the tab then shows control-plane
// rows only. The returned wired flags gate the source-filter options.
func (s *Server) buildCPInboxRows(ctx context.Context) (rows []AdminCPRow, architectWired, healingWired bool) {
	architectWired = s.workflowProposalsRepo != nil
	// Healing rows ARE workflow proposals (candidate → ProposalID), so the
	// healing slice needs both stores.
	healingWired = architectWired && s.healingCandidateRepo != nil
	if !architectWired {
		return nil, false, false
	}
	wps, err := s.workflowProposalsRepo.List(ctx, persistence.WorkflowProposalFilter{PageSize: adminCPPageLimit})
	if err != nil {
		s.logger.Warn().Err(err).Msg("cp inbox: workflow-proposal list failed; rendering ledger rows only")
		return nil, architectWired, healingWired
	}
	// Index healing candidates by the proposal they reference. The repo
	// lists newest first — keep the newest candidate per proposal.
	byProposal := map[string]*persistence.HealingCandidate{}
	if healingWired {
		cands, cerr := s.healingCandidateRepo.List(ctx, persistence.HealingCandidateListFilter{PageSize: adminCPPageLimit})
		if cerr != nil {
			s.logger.Warn().Err(cerr).Msg("cp inbox: healing candidate list failed; rows render as architect")
		} else {
			for _, c := range cands {
				if c.ProposalID == "" {
					continue
				}
				if _, seen := byProposal[c.ProposalID]; !seen {
					byProposal[c.ProposalID] = c
				}
			}
		}
	}
	for _, wp := range wps {
		rows = append(rows, s.cpInboxRow(ctx, wp, byProposal[wp.ID]))
	}
	return rows, architectWired, healingWired
}

// cpInboxRow converts one memetic workflow proposal (plus its healing
// candidate, when one references it) into a hub inbox row. Gates mirror the
// native surfaces verbatim: architect rows offer approve/reject only when
// pending, apply only when approved (+ applier wired), rollback only when
// applied (+ rollbacker wired); healing rows offer promote/reject only for a
// trial_passed candidate and run-trial for draft/trial_failed. A REGRESSED
// row gets NO actions — detail link only (review #5).
func (s *Server) cpInboxRow(ctx context.Context, wp *persistence.WorkflowProposal, cand *persistence.HealingCandidate) AdminCPRow {
	row := AdminCPRow{
		ID:         wp.ID,
		Title:      wp.WorkflowID,
		Status:     cpWorkflowDisplayStatus(wp.Status),
		Kind:       string(wp.Kind),
		WorkflowID: wp.WorkflowID,
		Source:     cpSourceArchitect,
		ProposedBy: cpSourceArchitect,
		Rationale:  skillBodyPreview(wp.Motivation, 200),
		Confidence: strconv.FormatFloat(float64(wp.Confidence), 'f', 2, 32),
		DetailHref: "/ui/admin/workflow-proposals/" + url.PathEscape(wp.ID),
		Regressed:  wp.Status == persistence.WorkflowProposalStatusRegressed,
	}
	if cand != nil {
		row.Source = cpSourceHealing
		row.ProposedBy = cpSourceHealing
		row.CandidateID = cand.ID
		row.TrialVerdict = s.cpLatestTrialVerdict(ctx, cand.ID)
		if !row.Regressed {
			row.DetailHref = "/ui/admin/blackbox/candidates/" + url.PathEscape(cand.ID)
		}
	}
	if row.Regressed {
		// Warning state: no action buttons; the detail link stays on the
		// workflow-proposal page, where the native re-propose/rollback flow
		// lives (review #5).
		return row
	}
	if cand != nil {
		// Healing rows drive the candidate lifecycle, not the memetic
		// decide/apply forms — promotion IS the gated apply.
		if s.healingPromoter != nil && cand.Status == persistence.HealingCandidateTrialPassed && !cand.Status.IsTerminal() {
			row.CanPromote = true
			row.CanReject = true
		}
		if s.healingTrialRunner != nil &&
			(cand.Status == persistence.HealingCandidateDraft || cand.Status == persistence.HealingCandidateTrialFailed) {
			row.CanRunTrial = true
		}
		return row
	}
	switch wp.Status {
	case persistence.WorkflowProposalStatusPending:
		row.CanApprove = true
		row.CanReject = true
	case persistence.WorkflowProposalStatusApproved:
		row.CanApply = s.workflowApplier != nil
	case persistence.WorkflowProposalStatusApplied:
		row.CanRollback = s.workflowRollbacker != nil
	}
	return row
}

// cpLatestTrialVerdict formats the newest trial's verdict for the inline
// "trial: passed (replay)" receipt on a healing row. Empty when the trial
// ledger isn't wired or the candidate has no trials yet.
func (s *Server) cpLatestTrialVerdict(ctx context.Context, candidateID string) string {
	if s.healingTrialRepo == nil {
		return ""
	}
	trials, err := s.healingTrialRepo.ListByCandidate(ctx, candidateID)
	if err != nil || len(trials) == 0 {
		return ""
	}
	return string(trials[0].Verdict) + " (" + string(trials[0].Mode) + ")"
}

// --- return_to=control-plane round-trip (§5.2 #2, review #10) -------------

// cpHubReturnRequested reports whether the posting form asked to round-trip
// back to the hub Proposals tab. Handlers without the param keep their
// native redirects — behaviour unchanged for pre-existing callers.
func cpHubReturnRequested(r *http.Request) bool {
	return r.FormValue("return_to") == "control-plane"
}

// cpHubProposalsURL builds the hub Proposals-tab redirect target. done is a
// fixed flash token (cpFlashMessages); errMsg is the operator-facing failure
// reason surfaced via the action_error banner (html/template escapes it).
func cpHubProposalsURL(done, errMsg string) string {
	u := "/ui/admin/control-plane?section=" + cpSectionProposals
	if done != "" {
		u += "&done=" + url.QueryEscape(done)
	}
	if errMsg != "" {
		if len(errMsg) > 200 {
			errMsg = errMsg[:200]
		}
		u += "&action_error=" + url.QueryEscape(errMsg)
	}
	return u
}

// cpHubOverviewURL builds the hub Overview-tab redirect target — where the
// folded Black Box trigger actions live. Same fixed-token discipline as
// cpHubProposalsURL (never echoes a raw param).
func cpHubOverviewURL(done, errMsg string) string {
	u := "/ui/admin/control-plane?section=" + cpSectionOverview
	if done != "" {
		u += "&done=" + url.QueryEscape(done)
	}
	if errMsg != "" {
		if len(errMsg) > 200 {
			errMsg = errMsg[:200]
		}
		u += "&action_error=" + url.QueryEscape(errMsg)
	}
	return u
}

// cpAwareCandidateRedirect routes a healing-candidate action outcome either
// back to the hub Proposals tab (return_to=control-plane) or to the native
// candidate detail page — the pre-existing behaviour.
//
// The targets embed request-derived free text (the errMsg action-banner and
// the candidate id). Both builders prepend a constant in-app path and
// url-escape the dynamic parts, but the target is passed through
// safeSameOriginPath before http.Redirect so a value that somehow resolved to
// an absolute or scheme-relative URL collapses to a fixed in-app fallback
// rather than redirecting the browser off-site (go/unvalidated-url-redirection).
func cpAwareCandidateRedirect(w http.ResponseWriter, r *http.Request, id, done, errMsg string) {
	if cpHubReturnRequested(r) {
		target := safeSameOriginPath(cpHubProposalsURL(done, errMsg),
			"/ui/admin/control-plane?section="+cpSectionProposals)
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	target := safeSameOriginPath(candidateDetailURL(id, errMsg), "/ui/admin/blackbox")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeSameOriginPath returns target only when it is a same-origin RELATIVE
// path: it must begin with a single "/" and carry no scheme or host, so a
// crafted "http://evil", "//evil.com", "https:/evil" or "/\evil" can never
// redirect the browser off-site (go/unvalidated-url-redirection). Anything
// else collapses to fallback (a fixed in-app path). Belt-and-suspenders: an
// explicit prefix guard plus a net/url parse that rejects any host component.
func safeSameOriginPath(target, fallback string) string {
	if !strings.HasPrefix(target, "/") ||
		strings.HasPrefix(target, "//") ||
		strings.HasPrefix(target, "/\\") {
		return fallback
	}
	u, err := url.Parse(target)
	if err != nil || u.IsAbs() || u.Host != "" || u.Hostname() != "" {
		return fallback
	}
	return target
}

// --- applyable-row polish (§4.4 latency signal → Diagnose deep-link) ------

// cpEvidenceHasLatencySignal detects the latency detector's structured
// evidence ("signal":"latency_p95_seconds") tolerating JSON re-marshals
// that add a space after the colon.
func cpEvidenceHasLatencySignal(evidence string) bool {
	return strings.Contains(evidence, `"signal":"latency_p95_seconds"`) ||
		strings.Contains(evidence, `"signal": "latency_p95_seconds"`)
}
