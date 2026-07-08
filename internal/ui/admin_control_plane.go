package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
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

// AdminCPSection is one top-level hub tab (Overview / Proposals / Diagnose /
// MCP). Sections are added as they ship; Href carries the ?section= link.
type AdminCPSection struct {
	Key    string
	Label  string
	Href   string
	Active bool
}

// Hub section keys.
const (
	cpSectionOverview  = "overview"
	cpSectionProposals = "proposals"
	cpSectionDiagnose  = "diagnose"
	cpSectionMCP       = "mcp"
)

// AdminCPSourceCount is the open-DRAFT count for one proposal source
// (ProposedBy), shown on the Overview tab.
type AdminCPSourceCount struct {
	Source string
	Label  string
	Open   int
}

// AdminCPIncident is one open self-heal incident (a DRAFT proposed by
// self-heal), summarised for the Overview tab.
type AdminCPIncident struct {
	ID        string
	ProjectID string
	Title     string
	RootCause string
}

// cpSourceLabels maps a ProposedBy token to a human label for the Overview.
var cpSourceLabels = map[string]string{
	"operator-ui":   "Operator (UI)",
	"tune-detector": "Tune detector",
	"instinct":      "Instinct",
	"diagnose":      "Diagnose",
	"self-heal":     "Self-heal",
}

func cpSourceLabel(src string) string {
	if l, ok := cpSourceLabels[src]; ok {
		return l
	}
	if src == "" {
		return "(unknown)"
	}
	return src
}

// AdminControlPlaneData backs admin_control_plane.html.
type AdminControlPlaneData struct {
	adminCommonData
	Available    bool
	Section      string           // active hub section
	Sections     []AdminCPSection // top-level hub tabs
	Filter       string
	SourceFilter string // ProposedBy filter (proposals section)
	Tabs         []AdminCPTab
	SourceTabs   []AdminCPTab
	Rows         []AdminCPRow
	// Overview section.
	SourceCounts  []AdminCPSourceCount
	OpenIncidents []AdminCPIncident
	OpenTotal     int
	// Diagnose section.
	Diagnose *AdminCPDiagnoseResult
	// MCP section.
	MCPRows     []AdminCPMCPRow
	MCPWritable bool
	Flash       string
	Error       string
}

// AdminCPDiagnoseResult backs the Diagnose tab after a run.
type AdminCPDiagnoseResult struct {
	Focus           string
	Ran             bool
	RootCause       string
	Confidence      string
	Evidence        []string
	SuggestedChange string
	ProposalID      string
	Err             string
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
	// MCP-tab (hub §4) write outcomes.
	"mcp-proposed":      "MCP change proposed — review the diff + apply on the Proposals tab.",
	"mcp-bad-name":      "Invalid server name (use letters, digits, - or _).",
	"mcp-bad-transport": "Transport must be stdio, sse, or streamable-http.",
	"mcp-bad-endpoint":  "sse/streamable-http need a valid http(s) URL; stdio needs a command.",
	"mcp-secret":        "A field looked like a literal secret — use a ${ENV_VAR} placeholder instead.",
	"mcp-not-found":     "No MCP server by that name to remove.",
}

// AdminControlPlane renders /ui/admin/control-plane (GET) and applies an
// approve/reject/apply/rollback decision (POST form).
func (s *Server) AdminControlPlane(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.adminCPDecide(w, r)
		return
	}
	section := normalizeCPSection(r.URL.Query().Get("section"))
	filter := normalizeCPFilter(r.URL.Query().Get("status"))
	data := AdminControlPlaneData{
		adminCommonData: adminCommonData{Title: "Control plane", CurrentPage: "admin-control-plane", IsAdmin: true},
		Available:       s.proposalStore != nil,
		Section:         section,
		Filter:          filter,
		Flash:           cpFlashMessages[r.URL.Query().Get("done")],
	}
	data.Sections = cpSections(section)
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
	switch section {
	case cpSectionOverview:
		s.buildCPOverview(&data, all)
	case cpSectionDiagnose:
		// GET renders the empty form; the verdict comes from the POST handler.
	case cpSectionMCP:
		s.buildCPMCP(ctx, &data)
	default: // proposals
		data.SourceFilter = strings.TrimSpace(r.URL.Query().Get("source"))
		s.buildCPProposals(&data, all, filter, data.SourceFilter)
	}
	s.render(w, "admin_control_plane.html", data)
}

// cpSections builds the top-level hub tab list, marking the active one. Diagnose
// + MCP are added as those sections ship.
func cpSections(active string) []AdminCPSection {
	defs := []struct{ key, label string }{
		{cpSectionOverview, "Overview"},
		{cpSectionProposals, "Proposals"},
		{cpSectionDiagnose, "Diagnose"},
		{cpSectionMCP, "MCP servers"},
	}
	out := make([]AdminCPSection, 0, len(defs))
	for _, d := range defs {
		href := "/ui/admin/control-plane?section=" + d.key
		out = append(out, AdminCPSection{Key: d.key, Label: d.label, Href: href, Active: d.key == active})
	}
	return out
}

func normalizeCPSection(v string) string {
	switch strings.TrimSpace(v) {
	case cpSectionProposals, cpSectionDiagnose, cpSectionMCP:
		return v
	default:
		return cpSectionOverview
	}
}

// AdminControlPlaneDiagnose handles POST /ui/admin/control-plane/diagnose — the
// Diagnose tab's trigger form. Runs the diagnoser and re-renders the hub on the
// Diagnose section with the verdict inline (no redirect, so the result shows).
func (s *Server) AdminControlPlaneDiagnose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/ui/admin/control-plane?section=diagnose", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	focus := strings.TrimSpace(r.FormValue("focus"))
	propose := r.FormValue("propose") == "on" || r.FormValue("propose") == "true"
	data := AdminControlPlaneData{
		adminCommonData: adminCommonData{Title: "Control plane", CurrentPage: "admin-control-plane", IsAdmin: true},
		Available:       s.proposalStore != nil,
		Section:         cpSectionDiagnose,
	}
	data.Sections = cpSections(cpSectionDiagnose)
	res := &AdminCPDiagnoseResult{Focus: focus}
	data.Diagnose = res
	switch {
	case s.diagnoser == nil:
		res.Err = "The diagnose engine is not wired on this daemon."
	case focus == "":
		res.Err = "Enter a focus (a project id, a task id, or free text)."
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		verdict, proposalID, err := s.diagnoser.Diagnose(ctx, focus, propose)
		res.Ran = true
		if err != nil {
			res.Err = err.Error()
		} else if verdict != nil {
			res.RootCause = verdict.RootCause
			res.Confidence = verdict.Confidence
			res.Evidence = verdict.Evidence
			res.SuggestedChange = verdict.SuggestedChange
			res.ProposalID = proposalID
		}
	}
	s.render(w, "admin_control_plane.html", data)
}

// buildCPOverview fills the Overview section: open-DRAFT counts per source +
// the open self-heal incidents.
func (s *Server) buildCPOverview(data *AdminControlPlaneData, all []*persistence.ControlPlaneProposal) {
	bySource := map[string]int{}
	order := []string{}
	for _, p := range all {
		if p.Status != persistence.ProposalStatusDraft {
			continue
		}
		data.OpenTotal++
		if _, seen := bySource[p.ProposedBy]; !seen {
			order = append(order, p.ProposedBy)
		}
		bySource[p.ProposedBy]++
		if p.ProposedBy == "self-heal" {
			data.OpenIncidents = append(data.OpenIncidents, AdminCPIncident{
				ID: p.ID, ProjectID: p.ProjectID, Title: p.Title,
				RootCause: skillBodyPreview(p.Rationale, 200),
			})
		}
	}
	sort.Strings(order)
	for _, src := range order {
		data.SourceCounts = append(data.SourceCounts, AdminCPSourceCount{
			Source: src, Label: cpSourceLabel(src), Open: bySource[src],
		})
	}
}

// buildCPProposals fills the Proposals section: the status tab bar + the
// filtered ledger rows.
func (s *Server) buildCPProposals(data *AdminControlPlaneData, all []*persistence.ControlPlaneProposal, filter, sourceFilter string) {
	counts := map[string]int{}
	sourceCounts := map[string]int{}
	sourceOrder := []string{}
	for _, p := range all {
		counts[p.Status]++
		if _, seen := sourceCounts[p.ProposedBy]; !seen {
			sourceOrder = append(sourceOrder, p.ProposedBy)
		}
		sourceCounts[p.ProposedBy]++
	}
	data.Tabs = append(data.Tabs, AdminCPTab{Key: "", Label: "All", Count: len(all), Active: filter == ""})
	for _, st := range adminCPStatuses {
		data.Tabs = append(data.Tabs, AdminCPTab{Key: st.Key, Label: st.Label, Count: counts[st.Key], Active: filter == st.Key})
	}
	// Source (ProposedBy) filter tabs — only when there's more than one source.
	if len(sourceOrder) > 1 {
		data.SourceTabs = append(data.SourceTabs, AdminCPTab{Key: "", Label: "All sources", Count: len(all), Active: sourceFilter == ""})
		sort.Strings(sourceOrder)
		for _, src := range sourceOrder {
			data.SourceTabs = append(data.SourceTabs, AdminCPTab{Key: src, Label: cpSourceLabel(src), Count: sourceCounts[src], Active: sourceFilter == src})
		}
	}
	for _, p := range all {
		if filter != "" && p.Status != filter {
			continue
		}
		if sourceFilter != "" && p.ProposedBy != sourceFilter {
			continue
		}
		applyable := strings.TrimSpace(p.ApplyTarget) != "" || strings.TrimSpace(p.ApplyOps) != ""
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
	// Decisions happen on the Proposals tab — return there.
	back := "/ui/admin/control-plane?section=" + cpSectionProposals
	if f := normalizeCPFilter(r.FormValue("status")); f != "" {
		back += "&status=" + url.QueryEscape(f)
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
