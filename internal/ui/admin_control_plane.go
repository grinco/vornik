package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// Control-plane console (EE UI surface — LLD 2026-07-08-control-plane-web-
// console-design). Browse the proposal ledger and approve/reject/apply/
// rollback in the browser. It reuses the SAME proposalStore + proposalApplier
// the CLI/REST use (server-side), so every daemon-side gate (self-approval,
// APPROVED-only apply, daemon-ack, busy-refuse, atomic-write + auto-rollback)
// is inherited unchanged — the UI can't bypass them. All user-derived content
// renders as escaped text (html/template auto-escaping); flash comes from a
// fixed message set, never the raw query param.

const adminCPPageLimit = 500

// AdminCPRow is one proposal rendered to the console table.
type AdminCPRow struct {
	ID          string
	Title       string
	Status      string
	Kind        string
	BlastRadius string
	ProjectID   string
	ProposedBy  string
	Approver    string
	AppliedBy   string
	Rationale   string
	DiffPreview string
	Evidence    string
	// Gates for the action buttons (mirror the daemon state machine).
	CanApprove  bool // DRAFT
	CanReject   bool // DRAFT
	CanApply    bool // APPROVED + has an apply target
	CanRollback bool // APPLIED
	ReviewOnly  bool // APPROVED but no apply target → "action by hand"
	IsDaemon    bool // daemon-scope → needs the ack checkbox
}

// AdminCPTab is a status filter with its live count.
type AdminCPTab struct {
	Key    string
	Label  string
	Count  int
	Active bool
}

// AdminControlPlaneData backs admin_control_plane.html.
type AdminControlPlaneData struct {
	adminCommonData
	Available bool
	Filter    string
	Tabs      []AdminCPTab
	Rows      []AdminCPRow
	Flash     string
	Error     string
}

var adminCPStatuses = []struct{ Key, Label string }{
	{persistence.ProposalStatusDraft, "Pending"},
	{persistence.ProposalStatusApproved, "Approved"},
	{persistence.ProposalStatusApplied, "Applied"},
	{persistence.ProposalStatusRejected, "Rejected"},
	{persistence.ProposalStatusRolledBack, "Rolled back"},
}

// cpFlashMessages maps the done= redirect token to a FIXED message (never
// echo the raw param — XSS-safe).
var cpFlashMessages = map[string]string{
	"APPROVED":      "Proposal approved.",
	"REJECTED":      "Proposal rejected.",
	"applied":       "Proposal applied (config hot-reloaded).",
	"rolled-back":   "Proposal rolled back.",
	"self-approval": "Rejected: you can't approve your own proposal.",
	"busy":          "Refused: tasks are running in scope — retry when idle.",
	"ack-required":  "Refused: a daemon-scope change needs the 'affects all projects' checkbox.",
	"review-only":   "This proposal has no applyable change — action it by hand.",
	"not-approved":  "Refused: only an APPROVED proposal can be applied.",
	"apply-failed":  "Apply failed (auto-rolled-back if the config was rejected).",
	"error":         "That action could not be completed.",
}

// AdminControlPlane renders /ui/admin/control-plane (GET) and applies an
// approve/reject/apply/rollback decision (POST form).
func (s *Server) AdminControlPlane(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.adminCPDecide(w, r)
		return
	}
	filter := normalizeCPFilter(r.URL.Query().Get("status"))
	data := AdminControlPlaneData{
		adminCommonData: adminCommonData{Title: "Control plane", CurrentPage: "admin", IsAdmin: true},
		Available:       s.proposalStore != nil,
		Filter:          filter,
		Flash:           cpFlashMessages[r.URL.Query().Get("done")],
	}
	if !data.Available {
		s.render(w, "admin_control_plane.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	all, err := s.proposalStore.List(ctx, persistence.ProposalListFilter{Limit: adminCPPageLimit})
	if err != nil {
		data.Error = "failed to load proposals"
		s.render(w, "admin_control_plane.html", data)
		return
	}
	counts := map[string]int{}
	for _, p := range all {
		counts[p.Status]++
	}
	data.Tabs = append(data.Tabs, AdminCPTab{Key: "", Label: "All", Count: len(all), Active: filter == ""})
	for _, st := range adminCPStatuses {
		data.Tabs = append(data.Tabs, AdminCPTab{Key: st.Key, Label: st.Label, Count: counts[st.Key], Active: filter == st.Key})
	}
	for _, p := range all {
		if filter != "" && p.Status != filter {
			continue
		}
		applyable := strings.TrimSpace(p.ApplyTarget) != ""
		data.Rows = append(data.Rows, AdminCPRow{
			ID: p.ID, Title: p.Title, Status: p.Status, Kind: p.Kind, BlastRadius: p.BlastRadius,
			ProjectID: p.ProjectID, ProposedBy: p.ProposedBy, Approver: p.Approver, AppliedBy: p.AppliedBy,
			Rationale: p.Rationale, DiffPreview: skillBodyPreview(p.Diff, 600), Evidence: p.Evidence,
			CanApprove:  p.Status == persistence.ProposalStatusDraft,
			CanReject:   p.Status == persistence.ProposalStatusDraft,
			CanApply:    p.Status == persistence.ProposalStatusApproved && applyable,
			CanRollback: p.Status == persistence.ProposalStatusApplied,
			ReviewOnly:  p.Status == persistence.ProposalStatusApproved && !applyable,
			IsDaemon:    p.BlastRadius == persistence.ProposalScopeDaemon,
		})
	}
	s.render(w, "admin_control_plane.html", data)
}

func normalizeCPFilter(v string) string {
	switch strings.TrimSpace(v) {
	case persistence.ProposalStatusDraft, persistence.ProposalStatusApproved,
		persistence.ProposalStatusApplied, persistence.ProposalStatusRejected,
		persistence.ProposalStatusRolledBack:
		return v
	default:
		return ""
	}
}

func (s *Server) adminCPDecide(w http.ResponseWriter, r *http.Request) {
	if s.proposalStore == nil {
		http.Error(w, "proposal ledger not wired", http.StatusNotImplemented)
		return
	}
	_ = r.ParseForm()
	back := "/ui/admin/control-plane"
	if f := normalizeCPFilter(r.FormValue("status")); f != "" {
		back += "?status=" + url.QueryEscape(f)
	}
	redirect := func(done string) {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		http.Redirect(w, r, back+sep+"done="+done, http.StatusSeeOther)
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	actor := adminPrincipal(r)
	if actor == "" || actor == "unknown" {
		actor = "web-admin"
	}

	switch r.FormValue("action") {
	case "approve":
		s.cpApproveReject(ctx, id, persistence.ProposalStatusApproved, actor, redirect)
	case "reject":
		s.cpApproveReject(ctx, id, persistence.ProposalStatusRejected, actor, redirect)
	case "apply":
		if s.proposalApplier == nil {
			redirect("error")
			return
		}
		ack := r.FormValue("ackDaemon") == "on" || r.FormValue("ackDaemon") == "true"
		redirect(cpApplyOutcome(s.proposalApplier.Apply(ctx, id, actor, ack)))
	case "rollback":
		if s.proposalApplier == nil {
			redirect("error")
			return
		}
		if err := s.proposalApplier.Rollback(ctx, id); err != nil {
			redirect("error")
			return
		}
		redirect("rolled-back")
	default:
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

func (s *Server) cpApproveReject(ctx context.Context, id, status, actor string, redirect func(string)) {
	err := s.proposalStore.SetStatus(ctx, id, status, actor)
	switch {
	case err == nil:
		redirect(status)
	case errors.Is(err, persistence.ErrProposalSelfApprove):
		redirect("self-approval")
	default:
		redirect("error")
	}
}

// cpApplyOutcome maps an apply engine error to a fixed flash token.
func cpApplyOutcome(err error) string {
	switch {
	case err == nil:
		return "applied"
	case errors.Is(err, persistence.ErrProposalNotApproved):
		return "not-approved"
	case errors.Is(err, controlplane.ErrBusy):
		return "busy"
	case errors.Is(err, controlplane.ErrDaemonAckRequired):
		return "ack-required"
	case errors.Is(err, controlplane.ErrReviewOnly):
		return "review-only"
	default:
		// Validation/reload failures (engine auto-rolled-back).
		return "apply-failed"
	}
}
