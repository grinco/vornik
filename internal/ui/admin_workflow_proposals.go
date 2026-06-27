package ui

// /ui/admin/workflow-proposals — operator review surface for the
// architect's pending / past proposals. Slice 3c of the
// memetic-workflows arc. List view + per-proposal drill-down +
// POST decide form.
//
// Same admin gate matrix as the rest of /admin/* (the surrounding
// admin.Middleware enforces presence of admin scope; rendering
// here only handles the "wired vs not" branch).

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowApplierUI is the narrow contract the UI needs from the
// applier. Same shape as api.WorkflowApplier but kept here so the
// ui package doesn't import api. *memetic.Applier satisfies both
// via the same one-line service-layer adapter.
type WorkflowApplierUI interface {
	Apply(ctx context.Context, proposalID, appliedBy string) (*persistence.WorkflowProposal, error)
}

// WorkflowRollbackerUI is the narrow contract for the Slice 5
// rollback button.
type WorkflowRollbackerUI interface {
	Rollback(ctx context.Context, proposalID, revertedBy string) (*persistence.WorkflowProposal, error)
}

// MemeticArchitectUI is the narrow contract behind the "Generate
// candidate" button on the workflow-healing trigger detail page.
// *memetic.Architect satisfies it via a one-line adapter in the
// service container so the ui package doesn't import internal/memetic.
type MemeticArchitectUI interface {
	Propose(ctx context.Context, workflowID string) (*persistence.WorkflowProposal, error)
}

// AdminWorkflowProposalsData backs the list page
// /ui/admin/workflow-proposals.
type AdminWorkflowProposalsData struct {
	adminCommonData
	Available      bool
	Proposals      []*persistence.WorkflowProposal
	FilterStatus   string
	FilterWorkflow string
	Limit          int
	LimitOptions   []int
	StatusOptions  []string
}

// AdminWorkflowProposalDetailData backs the drill-down page
// /ui/admin/workflow-proposals/{id}.
type AdminWorkflowProposalDetailData struct {
	adminCommonData
	Proposal    *persistence.WorkflowProposal
	Available   bool
	DecideError string
	// CanDecide is true when the proposal is in pending state.
	// The drill-down template hides the approve/reject form when
	// it's already decided so the operator can't accidentally
	// post a 409.
	CanDecide bool
	// CanApply is true when the proposal is approved but not yet
	// applied. The detail template renders an Apply button in
	// that case (Slice 4).
	CanApply bool
	// CanRollback is true when the proposal is applied and not
	// yet rolled back. Slice 5.
	CanRollback bool
	// HasApplier / HasRollbacker — when the wiring isn't present,
	// hide the buttons entirely rather than offering an action
	// that 503s. Both default to false (zero value).
	HasApplier    bool
	HasRollbacker bool
	// Diff is the before/after line diff of the current on-disk
	// WORKFLOW.md vs the proposed YAML (§8.5 diff panel). Nil when
	// no workflow source is wired — the template then shows the
	// proposed YAML alone (pre-§8.5 fallback).
	Diff        []DiffLine
	DiffAdded   int
	DiffRemoved int
	HasDiff     bool
	// CanModify is true for pending proposals when the repo is
	// wired — gates the "Modify" editor (§8.5). Lets the operator
	// tweak the architect's YAML before approving.
	CanModify bool
	// PredictedImpact is a short operator-facing summary of the
	// proposal's expected effect, derived from the confidence +
	// evidence count already on the row. Always populated; when a
	// telemetry rollup is available the Baseline block renders above
	// it with the workflow's real current profile.
	PredictedImpact string
	// Baseline is the telemetry-backed "Current baseline" block for
	// the predicted-impact panel — the workflow's current cost /
	// failure-rate profile (the baseline the proposal targets), NOT
	// a forward forecast. Nil when no rollup source is wired / the
	// fetch errored — the template then shows PredictedImpact alone.
	// see https://docs.vornik.io §Slice-3
	Baseline *WorkflowBaselineImpact
}

// AdminWorkflowProposals renders the list page.
func (s *Server) AdminWorkflowProposals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := adminClampLimit(q.Get("limit"))
	statusFilter := q.Get("status")
	if statusFilter == "" {
		statusFilter = "pending"
	}
	data := AdminWorkflowProposalsData{
		adminCommonData: adminCommonData{
			Title:       "Workflow Proposals",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:      s.workflowProposalsRepo != nil,
		Limit:          limit,
		LimitOptions:   adminLimitOptions,
		FilterStatus:   statusFilter,
		FilterWorkflow: q.Get("workflow"),
		StatusOptions: []string{
			"all", "pending", "approved", "rejected", "applied", "rolled_back", "regressed",
		},
	}
	if !data.Available {
		s.render(w, "admin_workflow_proposals.html", data)
		return
	}
	filter := persistence.WorkflowProposalFilter{
		WorkflowID: data.FilterWorkflow,
		PageSize:   limit,
	}
	if statusFilter != "all" {
		filter.Statuses = []persistence.WorkflowProposalStatus{
			persistence.WorkflowProposalStatus(statusFilter),
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	proposals, err := s.workflowProposalsRepo.List(ctx, filter)
	if err != nil {
		s.logger.Warn().Err(err).Msg("workflow-proposals list failed")
	} else {
		data.Proposals = proposals
	}
	s.render(w, "admin_workflow_proposals.html", data)
}

// AdminWorkflowProposalDetail renders the drill-down page.
// Routed via adminRouter on /admin/workflow-proposals/{id}.
func (s *Server) AdminWorkflowProposalDetail(w http.ResponseWriter, r *http.Request, id string) {
	data := AdminWorkflowProposalDetailData{
		adminCommonData: adminCommonData{
			Title:       "Workflow Proposal",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available: s.workflowProposalsRepo != nil,
	}
	if !data.Available {
		s.render(w, "admin_workflow_proposal.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	got, err := s.workflowProposalsRepo.Get(ctx, id)
	if err != nil {
		// 404 the page so the URL bar doesn't get cached as
		// "this id exists" when it doesn't.
		http.NotFound(w, r)
		return
	}
	data.Proposal = got
	data.CanDecide = got.Status == persistence.WorkflowProposalStatusPending
	data.CanModify = data.CanDecide // pending proposals can be edited before approval
	data.HasApplier = s.workflowApplier != nil
	data.HasRollbacker = s.workflowRollbacker != nil
	data.CanApply = data.HasApplier &&
		got.Status == persistence.WorkflowProposalStatusApproved
	data.CanRollback = data.HasRollbacker &&
		got.Status == persistence.WorkflowProposalStatusApplied

	// §8.5 diff panel: render the current on-disk WORKFLOW.md against
	// the proposed YAML when a source is wired. Best-effort — a load
	// failure (workflow renamed/deleted) leaves HasDiff=false and the
	// template falls back to the proposed-YAML-only view.
	if s.workflowSourceUI != nil {
		if cur, lerr := s.workflowSourceUI.Load(ctx, got.WorkflowID); lerr == nil {
			data.Diff = computeWorkflowDiff(string(cur), got.ProposalYAML)
			data.DiffAdded, data.DiffRemoved = diffStats(data.Diff)
			data.HasDiff = true
		} else {
			s.logger.Debug().Err(lerr).Str("workflow_id", got.WorkflowID).
				Msg("proposal diff: current workflow load failed; showing proposed only")
		}
	}

	// Predicted-impact panel. The heuristic one-liner always renders.
	// When a telemetry rollup source is wired, also fetch the
	// workflow's CURRENT cost / failure-rate profile over a 30-day
	// window and render it as the honest "Current baseline" block —
	// the baseline the proposal targets, NOT a fabricated forecast.
	// Best-effort + nil-safe: a nil source, a fetch error, or no rows
	// leaves Baseline nil so the panel degrades to the heuristic
	// summary unchanged.
	// see https://docs.vornik.io §Slice-3
	data.PredictedImpact = predictedImpactSummary(got, data.DiffAdded, data.DiffRemoved, data.HasDiff)
	if s.workflowRollupSource != nil {
		const baselineWindowDays = 30
		since := time.Now().UTC().AddDate(0, 0, -baselineWindowDays)
		if rollup, rerr := s.workflowRollupSource.ForWorkflow(ctx, got.WorkflowID, since); rerr == nil {
			data.Baseline = buildWorkflowBaseline(got, rollup, baselineWindowDays)
		} else {
			s.logger.Debug().Err(rerr).Str("workflow_id", got.WorkflowID).
				Msg("proposal baseline: rollup fetch failed; showing heuristic impact only")
		}
	}
	// Surface the operator's last failed decide via query param so
	// a 409 round-trips back as a visible message rather than a
	// blank successful redirect.
	if msg := r.URL.Query().Get("decide_error"); msg != "" {
		data.DecideError = msg
	}
	s.render(w, "admin_workflow_proposal.html", data)
}

// AdminWorkflowProposalApply handles the POST apply form. Routed
// via adminRouter on /admin/workflow-proposals/{id}/apply.
func (s *Server) AdminWorkflowProposalApply(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.workflowApplier == nil {
		http.Error(w, "workflow applier not wired", http.StatusServiceUnavailable)
		return
	}
	appliedBy := adminPrincipal(r)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, err := s.workflowApplier.Apply(ctx, id, appliedBy)
	if err != nil {
		s.logger.Warn().Err(err).Str("proposal_id", id).Msg("apply failed")
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		http.Redirect(w, r,
			"/ui/admin/workflow-proposals/"+id+"?decide_error="+strings.ReplaceAll(msg, " ", "+"),
			http.StatusSeeOther)
		return
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: appliedBy,
			Source:    "ui",
			Action:    "workflow-proposal.applied",
			Target:    id,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, "/ui/admin/workflow-proposals/"+id, http.StatusSeeOther)
}

// AdminWorkflowProposalRollback handles the POST rollback form.
// Slice 5 entry point. Same shape as Apply.
func (s *Server) AdminWorkflowProposalRollback(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.workflowRollbacker == nil {
		http.Error(w, "workflow rollback not wired", http.StatusServiceUnavailable)
		return
	}
	revertedBy := adminPrincipal(r)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, err := s.workflowRollbacker.Rollback(ctx, id, revertedBy)
	if err != nil {
		s.logger.Warn().Err(err).Str("proposal_id", id).Msg("rollback failed")
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		http.Redirect(w, r,
			"/ui/admin/workflow-proposals/"+id+"?decide_error="+strings.ReplaceAll(msg, " ", "+"),
			http.StatusSeeOther)
		return
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: revertedBy,
			Source:    "ui",
			Action:    "workflow-proposal.rolled_back",
			Target:    id,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, "/ui/admin/workflow-proposals/"+id, http.StatusSeeOther)
}

// AdminWorkflowProposalModify handles the POST modify form (§8.5
// "Modify" button). Replaces a PENDING proposal's YAML with the
// operator's edited version before they approve it. The repo refuses
// non-pending rows so a decided/applied proposal's recorded YAML
// stays immutable. Routed on /admin/workflow-proposals/{id}/modify.
func (s *Server) AdminWorkflowProposalModify(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.workflowProposalsRepo == nil {
		http.Error(w, "workflow proposals not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	newYAML := strings.TrimSpace(r.FormValue("proposal_yaml"))
	if newYAML == "" {
		http.Redirect(w, r,
			"/ui/admin/workflow-proposals/"+id+"?decide_error=modified+YAML+cannot+be+empty",
			http.StatusSeeOther)
		return
	}
	editedBy := adminPrincipal(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.workflowProposalsRepo.UpdateProposalYAML(ctx, id, newYAML, editedBy); err != nil {
		s.logger.Warn().Err(err).Str("proposal_id", id).Msg("modify failed")
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		http.Redirect(w, r,
			"/ui/admin/workflow-proposals/"+id+"?decide_error="+strings.ReplaceAll(msg, " ", "+"),
			http.StatusSeeOther)
		return
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: editedBy,
			Source:    "ui",
			Action:    "workflow-proposal.modified",
			Target:    id,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, "/ui/admin/workflow-proposals/"+id, http.StatusSeeOther)
}

// AdminWorkflowProposalDecide handles the POST decide form.
// Routed via adminRouter on /admin/workflow-proposals/{id}/decide.
func (s *Server) AdminWorkflowProposalDecide(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.workflowProposalsRepo == nil {
		http.Error(w, "workflow proposals not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	statusRaw := strings.TrimSpace(r.FormValue("status"))
	notes := strings.TrimSpace(r.FormValue("notes"))
	switch persistence.WorkflowProposalStatus(statusRaw) {
	case persistence.WorkflowProposalStatusApproved,
		persistence.WorkflowProposalStatusRejected:
		// ok
	default:
		http.Redirect(w, r,
			"/ui/admin/workflow-proposals/"+id+
				"?decide_error=status+must+be+approved+or+rejected",
			http.StatusSeeOther)
		return
	}

	decidedBy := adminPrincipal(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := s.workflowProposalsRepo.Decide(ctx, id,
		persistence.WorkflowProposalStatus(statusRaw), decidedBy, notes)
	if err != nil {
		s.logger.Warn().Err(err).Str("proposal_id", id).Msg("decide failed")
		// Send the user back to the detail page with the error
		// surfaced. Encoding via url.QueryEscape would be cleaner
		// but the message is operator-facing free text — keep it
		// simple.
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		http.Redirect(w, r,
			"/ui/admin/workflow-proposals/"+id+"?decide_error="+strings.ReplaceAll(msg, " ", "+"),
			http.StatusSeeOther)
		return
	}
	// Stamp the operator action in the admin audit log so the
	// decision shows up in /ui/admin/audit alongside other admin
	// actions.
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: decidedBy,
			Source:    "ui",
			Action:    "workflow-proposal." + statusRaw,
			Target:    id,
			After:     notes,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, "/ui/admin/workflow-proposals/"+id, http.StatusSeeOther)
}
