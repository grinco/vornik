package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/skills"
)

// Knowledge-skill review inbox (EE UI surface — learning-loop design).
// Lists DRAFT skills awaiting the human gate and lets an authenticated
// operator approve/reject them. This is an Enterprise convenience; the
// Community-Edition approval path is the Telegram review digest. Both
// route through skills.ApplyDecision so they can't diverge.

// AdminSkillRow is one draft rendered to the inbox table.
type AdminSkillRow struct {
	ID          string
	Name        string
	Description string
	BodyPreview string
	ProjectID   string
	RepoScope   string
	Domain      string
	OriginTask  string
}

// AdminSkillsData backs the admin_skills.html template.
type AdminSkillsData struct {
	adminCommonData
	Available bool
	Rows      []AdminSkillRow
	Flash     string
	Error     string
}

// AdminSkills renders /ui/admin/skills (GET) and applies an
// approve/reject decision (POST form).
func (s *Server) AdminSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.adminSkillDecide(w, r)
		return
	}
	data := AdminSkillsData{
		adminCommonData: adminCommonData{Title: "Skills", CurrentPage: "admin", IsAdmin: true},
		Available:       s.skillRepo != nil,
		Flash:           r.URL.Query().Get("done"),
	}
	if !data.Available {
		s.render(w, "admin_skills.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	drafts, err := s.skillRepo.ListDrafts(ctx, 200)
	if err != nil {
		data.Error = "failed to load draft skills"
		s.render(w, "admin_skills.html", data)
		return
	}
	for _, d := range drafts {
		data.Rows = append(data.Rows, AdminSkillRow{
			ID: d.ID, Name: d.Name, Description: d.Description,
			BodyPreview: skillBodyPreview(d.Body, 400),
			ProjectID:   d.ProjectID, RepoScope: d.RepoScope, Domain: d.Domain,
			OriginTask: d.OriginTask,
		})
	}
	s.render(w, "admin_skills.html", data)
}

// adminSkillDecide handles the approve/reject form POST.
func (s *Server) adminSkillDecide(w http.ResponseWriter, r *http.Request) {
	if s.skillRepo == nil {
		http.Error(w, "skill store not wired", http.StatusNotImplemented)
		return
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Redirect(w, r, "/ui/admin/skills", http.StatusSeeOther)
		return
	}
	decision := skills.Approve
	if r.FormValue("action") == "reject" {
		decision = skills.Reject
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	outcome, err := skills.ApplyDecision(ctx, s.skillRepo, id, decision)
	if err != nil {
		http.Redirect(w, r, "/ui/admin/skills?done=error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/admin/skills?done="+outcome, http.StatusSeeOther)
}

func skillBodyPreview(body string, n int) string {
	body = strings.TrimSpace(body)
	if len(body) <= n {
		return body
	}
	return body[:n] + "…"
}
