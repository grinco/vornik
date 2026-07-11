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
// renders as escaped text (html/template auto-escaping); the success flash
// comes from a fixed message set (done=<token>), while action_error carries
// the round-tripped failure reason as escaped free text (same pattern as the
// candidate/workflow-proposal detail pages).

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
	CanReject   bool // DRAFT (reject) or APPROVED (withdraw)
	CanApply    bool // APPROVED + has an apply target
	CanRollback bool // APPLIED
	ReviewOnly  bool // APPROVED but no apply target → "action by hand"
	IsDaemon    bool // daemon-scope → DAEMON badge + warning line
	LiveApply   bool // applies live (skips the busy gate) → no idle wait
	// NeedsAck generalises the apply-ack checkbox (design §4.5): daemon
	// AND swarm blast radii require the operator to tick it. AckLabel is
	// the scope-appropriate wording; the form field stays ackDaemon (the
	// engine is being extended to enforce swarm server-side separately).
	NeedsAck bool
	AckLabel string
	// DiagnoseHref deep-links a latency-signal row to the hub Diagnose tab
	// pre-filled with the project. Empty unless the diagnoser is wired.
	DiagnoseHref string
	// Unified-inbox extensions (Part B §5.2): architect/healing rows folded
	// in from the memetic workflow-proposal + healing-candidate stores.
	Source       string // "" = control-plane ledger row; "architect" | "healing"
	WorkflowID   string
	Confidence   string // formatted architect confidence, e.g. "0.82"
	DetailHref   string // native detail page (diff / scorecard depth)
	Regressed    bool   // memetic regressed → warning style, NO actions
	CandidateID  string // healing rows: the candidate the actions target
	TrialVerdict string // healing rows: latest trial, e.g. "passed (replay)"
	CanRunTrial  bool   // healing rows: candidate draft/trial_failed
	CanPromote   bool   // healing rows: candidate trial_passed
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
	"operator-ui":     "Operator (UI)",
	"tune-detector":   "Tune detector",
	"instinct":        "Instinct",
	"diagnose":        "Diagnose",
	"self-heal":       "Self-heal",
	cpSourceArchitect: "Architect",
	cpSourceHealing:   "Healing",
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
	// DiagnoseLocked marks a CE daemon (no diagnoser wired): the Diagnose
	// tab is dropped from Sections and a muted upgrade caption renders in
	// its place (design §7 CE note).
	DiagnoseLocked bool
	// Overview section.
	SourceCounts  []AdminCPSourceCount
	OpenIncidents []AdminCPIncident
	OpenTotal     int
	// Black Box open triggers folded into the Overview (item 5 part 3).
	// Empty in CE / when the healing-trigger repo is unwired.
	OpenTriggers         []HealingTriggerRow
	OpenTriggerCount     int
	TriggerGenerateWired bool // the architect is wired → show Generate candidate
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
	"ack-required":  "Refused: a daemon- or swarm-scope change needs the blast-radius acknowledgement checkbox.",
	"review-only":   "This proposal has no applyable change — action it by hand.",
	"stale-base":    "Refused: config.yaml changed since this proposal was drafted — re-draft it (nothing was written).",
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
	// Unified-inbox round-trip tokens (Part B §5.2): the memetic workflow-
	// proposal and healing-candidate handlers redirect here on success when
	// the hub form carried return_to=control-plane.
	"wf-approved":        "Workflow proposal approved.",
	"wf-rejected":        "Workflow proposal rejected.",
	"wf-applied":         "Workflow proposal applied (WORKFLOW.md updated).",
	"wf-rolled-back":     "Workflow proposal rolled back.",
	"trial-started":      "Trial started — the verdict lands on the candidate's trial history.",
	"candidate-promoted": "Candidate promoted — the repair was applied via its workflow proposal.",
	"candidate-rejected": "Candidate rejected — production untouched.",
	// Black Box trigger actions folded into the Overview (item 5 part 3).
	"trigger-dismissed":   "Black Box trigger dismissed.",
	"candidate-generated": "Candidate generated — review it on the Proposals tab.",
}

// AdminControlPlane renders /ui/admin/control-plane (GET) and applies an
// approve/reject/apply/rollback decision (POST form).
func (s *Server) AdminControlPlane(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.adminCPDecide(w, r)
		return
	}
	section := normalizeCPSection(r.URL.Query().Get("section"))
	// CE gate (design §6.3): without a diagnoser the Diagnose tab does not
	// exist — a direct ?section=diagnose falls back to the Overview and the
	// tab bar renders the EE upgrade caption instead.
	if section == cpSectionDiagnose && s.diagnoser == nil {
		section = cpSectionOverview
	}
	filter := normalizeCPFilter(r.URL.Query().Get("status"))
	data := AdminControlPlaneData{
		adminCommonData: adminCommonData{Title: "Control plane", CurrentPage: "admin-control-plane", IsAdmin: true},
		Available:       s.proposalStore != nil,
		Section:         section,
		Filter:          filter,
		Flash:           cpFlashMessages[r.URL.Query().Get("done")],
		DiagnoseLocked:  s.diagnoser == nil,
	}
	// Round-tripped failure reason from a workflow-proposal / healing-
	// candidate action (return_to=control-plane). Free text, escaped by
	// html/template at render time.
	if msg := strings.TrimSpace(r.URL.Query().Get("action_error")); msg != "" {
		data.Error = msg
	}
	data.Sections = s.cpSections(section)
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
		s.buildCPOverview(ctx, &data, all)
	case cpSectionDiagnose:
		// GET renders the form; a ?focus= deep-link (latency-row "Diagnose ↗")
		// pre-fills it. The verdict comes from the POST handler.
		if focus := strings.TrimSpace(r.URL.Query().Get("focus")); focus != "" {
			data.Diagnose = &AdminCPDiagnoseResult{Focus: focus}
		}
	case cpSectionMCP:
		s.buildCPMCP(ctx, &data)
	default: // proposals
		data.SourceFilter = strings.TrimSpace(r.URL.Query().Get("source"))
		s.buildCPProposals(ctx, &data, all, filter, data.SourceFilter)
	}
	s.render(w, "admin_control_plane.html", data)
}

// cpSections builds the top-level hub tab list, marking the active one. The
// section list is data-driven: Diagnose only exists when the diagnoser is
// wired (EE) — CE renders the upgrade caption in its place (design §6.3).
func (s *Server) cpSections(active string) []AdminCPSection {
	defs := []struct{ key, label string }{
		{cpSectionOverview, "Overview"},
		{cpSectionProposals, "Proposals"},
		{cpSectionDiagnose, "Diagnose"},
		{cpSectionMCP, "MCP servers"},
	}
	out := make([]AdminCPSection, 0, len(defs))
	for _, d := range defs {
		if d.key == cpSectionDiagnose && s.diagnoser == nil {
			continue
		}
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
		DiagnoseLocked:  s.diagnoser == nil,
	}
	data.Sections = s.cpSections(cpSectionDiagnose)
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
func (s *Server) buildCPOverview(ctx context.Context, data *AdminControlPlaneData, all []*persistence.ControlPlaneProposal) {
	// Black Box open triggers — pre-decision signals folded into the hub
	// Overview so an operator can dismiss them or generate a candidate
	// without leaving for /ui/admin/blackbox. EE-only surface (nil repo in
	// CE leaves the panel hidden). The action buttons POST to the existing
	// blackbox trigger endpoints with return_to=control-plane.
	if s.healingTriggerRepo != nil {
		if trigs, err := s.healingTriggerRepo.List(ctx, persistence.HealingTriggerListFilter{
			Status: persistence.HealingTriggerStatusOpen, PageSize: 20,
		}); err == nil {
			for _, t := range trigs {
				data.OpenTriggers = append(data.OpenTriggers, healingTriggerToRow(t))
			}
			data.OpenTriggerCount = len(data.OpenTriggers)
			data.TriggerGenerateWired = s.blackboxArchitect != nil
		}
	}

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
// filtered rows of the UNIFIED inbox — control-plane ledger rows plus the
// architect/healing rows folded in from the memetic stores (Part B §5.2).
func (s *Server) buildCPProposals(ctx context.Context, data *AdminControlPlaneData, all []*persistence.ControlPlaneProposal, filter, sourceFilter string) {
	inboxRows, architectWired, healingWired := s.buildCPInboxRows(ctx)
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
	for _, r := range inboxRows {
		counts[r.Status]++
		sourceCounts[r.Source]++
	}
	total := len(all) + len(inboxRows)
	data.Tabs = append(data.Tabs, AdminCPTab{Key: "", Label: "All", Count: total, Active: filter == ""})
	for _, st := range adminCPStatuses {
		data.Tabs = append(data.Tabs, AdminCPTab{Key: st.Key, Label: st.Label, Count: counts[st.Key], Active: filter == st.Key})
	}
	// Source (ProposedBy) filter tabs — only when there's more than one
	// source to slice. The architect/healing options exist only when their
	// stores are wired (CE nil-store degradation, §6.1).
	sort.Strings(sourceOrder)
	if architectWired {
		sourceOrder = append(sourceOrder, cpSourceArchitect)
	}
	if healingWired {
		sourceOrder = append(sourceOrder, cpSourceHealing)
	}
	if len(sourceOrder) > 1 {
		data.SourceTabs = append(data.SourceTabs, AdminCPTab{Key: "", Label: "All sources", Count: total, Active: sourceFilter == ""})
		for _, src := range sourceOrder {
			data.SourceTabs = append(data.SourceTabs, AdminCPTab{Key: src, Label: cpSourceLabel(src), Count: sourceCounts[src], Active: sourceFilter == src})
		}
	}
	// Control-plane ledger rows — hidden entirely when an inbox-only source
	// (architect/healing) is selected; the inverse holds below.
	ledgerHidden := sourceFilter == cpSourceArchitect || sourceFilter == cpSourceHealing
	for _, p := range all {
		if ledgerHidden {
			break
		}
		if filter != "" && p.Status != filter {
			continue
		}
		if sourceFilter != "" && p.ProposedBy != sourceFilter {
			continue
		}
		data.Rows = append(data.Rows, s.cpLedgerRow(p))
	}
	for _, r := range inboxRows {
		if filter != "" && r.Status != filter {
			continue
		}
		if sourceFilter != "" && r.Source != sourceFilter {
			continue
		}
		data.Rows = append(data.Rows, r)
	}
}

// cpLedgerRow renders one control-plane ledger proposal as a hub row.
func (s *Server) cpLedgerRow(p *persistence.ControlPlaneProposal) AdminCPRow {
	applyable := strings.TrimSpace(p.ApplyTarget) != "" || strings.TrimSpace(p.ApplyOps) != ""
	row := AdminCPRow{
		ID: p.ID, Title: p.Title, Status: p.Status, Kind: p.Kind, BlastRadius: p.BlastRadius,
		ProjectID: p.ProjectID, ProposedBy: p.ProposedBy, Approver: p.Approver, AppliedBy: p.AppliedBy,
		Rationale: p.Rationale, DiffPreview: skillBodyPreview(p.Diff, 600), Evidence: p.Evidence,
		CanApprove: p.Status == persistence.ProposalStatusDraft,
		// Reject a DRAFT; withdraw an APPROVED-but-unappliable proposal
		// (e.g. superseded by a re-draft) — both route to REJECTED.
		CanReject:   p.Status == persistence.ProposalStatusDraft || p.Status == persistence.ProposalStatusApproved,
		CanApply:    p.Status == persistence.ProposalStatusApproved && applyable,
		CanRollback: p.Status == persistence.ProposalStatusApplied,
		ReviewOnly:  p.Status == persistence.ProposalStatusApproved && !applyable,
		IsDaemon:    p.BlastRadius == persistence.ProposalScopeDaemon,
		LiveApply:   p.LiveApply,
	}
	// Blast-radius ack (design §4.5): daemon AND swarm scopes require
	// the acknowledgement checkbox before apply. Same ackDaemon field —
	// the engine enforces swarm server-side too.
	switch p.BlastRadius {
	case persistence.ProposalScopeDaemon:
		row.NeedsAck, row.AckLabel = true, "affects all projects"
	case persistence.ProposalScopeSwarm:
		row.NeedsAck, row.AckLabel = true, "affects every project using this swarm"
	}
	// Latency-signal rows deep-link into the Diagnose tab pre-filled
	// with the project — only when the diagnoser is wired (EE).
	if s.diagnoser != nil && p.ProjectID != "" && cpEvidenceHasLatencySignal(p.Evidence) {
		row.DiagnoseHref = "/ui/admin/control-plane?section=diagnose&focus=" + url.QueryEscape(p.ProjectID)
	}
	return row
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
	case errors.Is(err, controlplane.ErrStaleBase):
		// Refused BEFORE any write (base hash mismatch) — nothing was rolled
		// back, so the generic "apply-failed" message would mislead.
		return "stale-base"
	default:
		// Validation/reload failures (engine auto-rolled-back).
		return "apply-failed"
	}
}
