package ui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/skills"
)

// Knowledge-skill browser (EE UI surface — learning-loop design). Surveys
// the whole daemon-owned skill store across projects, filterable by
// maturity, and lets an authenticated operator approve/reject a skill and
// flip its cross-project (global) reach. This is an Enterprise
// convenience; the Community-Edition approval path is the Telegram review
// digest. Both route through skills.ApplyDecision so they can't diverge.

// adminSkillsPageLimit caps how many skills the browser loads per view.
const adminSkillsPageLimit = 500

// AdminSkillRow is one skill rendered to the browser table.
type AdminSkillRow struct {
	ID          string
	Name        string
	Description string
	// Body is the FULL skill document, rendered in an expandable block so the
	// operator can read exactly what they're approving. Previously only a
	// truncated preview was shown, making informed approval impossible
	// (2026-07-08 operator report).
	Body       string
	ProjectID  string
	RepoScope  string
	Domain     string
	OriginTask string
	Maturity   string
	// Roles are the swarm roles this skill injects into (empty = any
	// role). Surfaced so the operator sees a skill's role scoping.
	Roles []string
	// IsGlobal drives the GLOBAL badge + the "affects ALL projects"
	// blast-radius label so the operator sees a skill's cross-project
	// reach before approving or while managing it.
	IsGlobal bool
	// Usage counters — let the operator watch a skill get picked up
	// (fired) and pay off (worked) across projects.
	UsageFired     int64
	UsageWorked    int64
	UsageCorrected int64
	// CanApprove/CanReject gate the action buttons by maturity.
	CanApprove bool
	CanReject  bool
}

// AdminSkillTab is one maturity filter link with its live count.
type AdminSkillTab struct {
	Key    string // "" = all
	Label  string
	Count  int
	Active bool
}

// AdminSkillsData backs the admin_skills.html template.
type AdminSkillsData struct {
	adminCommonData
	Available bool
	Filter    string // the active maturity filter ("" = all)
	Tabs      []AdminSkillTab
	Rows      []AdminSkillRow
	Flash     string
	Error     string
}

// adminSkillMaturities is the tab order (besides "all").
var adminSkillMaturities = []struct{ Key, Label string }{
	{persistence.SkillMaturityDraft, "Pending review"},
	{persistence.SkillMaturityActive, "Active"},
	{persistence.SkillMaturityTrusted, "Trusted"},
	{persistence.SkillMaturityRetired, "Retired"},
}

// AdminSkills renders /ui/admin/skills (GET) and applies an
// approve/reject/set-reach decision (POST form).
func (s *Server) AdminSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.adminSkillDecide(w, r)
		return
	}
	filter := normalizeSkillFilter(r.URL.Query().Get("maturity"))
	data := AdminSkillsData{
		adminCommonData: adminCommonData{Title: "Skills", CurrentPage: "admin-skills", IsAdmin: true},
		Available:       s.skillRepo != nil,
		Filter:          filter,
		Flash:           r.URL.Query().Get("done"),
	}
	if !data.Available {
		s.render(w, "admin_skills.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// One survey of the whole store powers both the per-maturity counts
	// (tabs) and the filtered rows — cheaper than a query per tab.
	all, err := s.skillRepo.ListAcrossProjects(ctx, nil, adminSkillsPageLimit)
	if err != nil {
		data.Error = "failed to load skills"
		s.render(w, "admin_skills.html", data)
		return
	}

	counts := map[string]int{}
	for _, sk := range all {
		counts[sk.Maturity]++
	}
	data.Tabs = append(data.Tabs, AdminSkillTab{Key: "", Label: "Current", Count: len(all) - counts[persistence.SkillMaturityRetired], Active: filter == ""})
	for _, m := range adminSkillMaturities {
		data.Tabs = append(data.Tabs, AdminSkillTab{
			Key: m.Key, Label: m.Label, Count: counts[m.Key], Active: filter == m.Key,
		})
	}

	for _, sk := range all {
		if filter == "" && sk.Maturity == persistence.SkillMaturityRetired {
			continue
		}
		if filter != "" && sk.Maturity != filter {
			continue
		}
		data.Rows = append(data.Rows, AdminSkillRow{
			ID: sk.ID, Name: sk.Name, Description: sk.Description,
			Body:      strings.TrimSpace(sk.Body),
			ProjectID: sk.ProjectID, RepoScope: sk.RepoScope, Domain: sk.Domain,
			OriginTask: sk.OriginTask, Maturity: sk.Maturity, IsGlobal: sk.IsGlobal,
			Roles:      sk.Roles,
			UsageFired: sk.UsageFired, UsageWorked: sk.UsageWorked, UsageCorrected: sk.UsageCorrected,
			// Approve promotes draft/retired → active; Reject/retire is a
			// no-op-to-show on an already-retired skill.
			CanApprove: sk.Maturity != persistence.SkillMaturityActive && sk.Maturity != persistence.SkillMaturityTrusted,
			CanReject:  sk.Maturity != persistence.SkillMaturityRetired,
		})
	}
	s.render(w, "admin_skills.html", data)
}

// normalizeSkillFilter maps a query value to a known maturity or "" (all).
func normalizeSkillFilter(v string) string {
	switch strings.TrimSpace(v) {
	case persistence.SkillMaturityDraft, persistence.SkillMaturityActive,
		persistence.SkillMaturityTrusted, persistence.SkillMaturityRetired:
		return v
	default:
		return ""
	}
}

// adminSkillDecide handles the approve/reject/set-reach form POST.
func (s *Server) adminSkillDecide(w http.ResponseWriter, r *http.Request) {
	if s.skillRepo == nil {
		http.Error(w, "skill store not wired", http.StatusNotImplemented)
		return
	}
	_ = r.ParseForm()
	// Preserve the operator's current tab across the redirect.
	back := "/ui/admin/skills"
	if f := normalizeSkillFilter(r.FormValue("maturity")); f != "" {
		back += "?maturity=" + url.QueryEscape(f)
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// set-global / set-project flip cross-project reach without touching
	// maturity (LLD 2026-07-07-cross-project-global-skills). Routes through
	// the same SkillRepository.SetGlobal the CLI/companion use, so the
	// surfaces can't diverge.
	switch r.FormValue("action") {
	case "set-global", "set-project":
		if err := s.skillRepo.SetGlobal(ctx, id, r.FormValue("action") == "set-global"); err != nil {
			redirect("error")
			return
		}
		redirect("reach-updated")
		return
	}

	decision := skills.Approve
	if r.FormValue("action") == "reject" {
		decision = skills.Reject
	}
	outcome, err := skills.ApplyDecision(ctx, s.skillRepo, id, decision)
	if err != nil {
		redirect("error")
		return
	}
	redirect(outcome)
}

func skillBodyPreview(body string, n int) string {
	body = strings.TrimSpace(body)
	if len(body) <= n {
		return body
	}
	return body[:n] + "…"
}
