package ui

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
)

// WizardPageData is the template payload for /ui/projects/new/wizard.
// The page is otherwise static — per-turn state lives client-side in JS
// that calls /api/v1/projects/wizard/converse. When the operator
// resumes an existing draft (banner "Resume" link → ?session=<id>),
// ResumeSessionID seeds the client's sessionID so the page continues
// that session (Converse appends to it; Cancel targets it) instead of
// silently starting a fresh one — and ResumeTranscriptJSON replays the
// prior turns into the chat pane.
type WizardPageData struct {
	Title       string
	CurrentPage string

	// ResumeSessionID is the draft being resumed ("" for a new wizard).
	ResumeSessionID string
	// ResumeTranscriptJSON is a JS array of {role,content} for the
	// prior turns, safe to embed in <script> (json.Marshal escapes
	// <,>,&). Always a valid literal — "[]" when there's nothing to
	// replay.
	ResumeTranscriptJSON template.JS
}

// ProjectsNewWizard renders the conversational project-setup wizard.
// With ?session=<id> it resumes one of the requesting operator's own
// uncommitted drafts.
func (s *Server) ProjectsNewWizard(w http.ResponseWriter, r *http.Request) {
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := WizardPageData{
		Title:                "New project — wizard",
		CurrentPage:          "projects",
		ResumeTranscriptJSON: template.JS("[]"),
	}
	if sid := strings.TrimSpace(r.URL.Query().Get("session")); sid != "" {
		s.populateWizardResume(r, &data, sid)
	}
	s.render(w, "projects_new_wizard.html", data)
}

// populateWizardResume seeds data for a resumed draft, but ONLY when
// sid is one of the requesting operator's own uncommitted, un-cancelled
// sessions. An unknown / foreign / committed / cancelled id is silently
// ignored (the page renders fresh) so a crafted ?session= can neither
// resume another operator's draft nor revive a closed one.
func (s *Server) populateWizardResume(r *http.Request, data *WizardPageData, sid string) {
	if s.wizardSessions == nil {
		return
	}
	operator := s.operatorIDForRequest(r)
	if operator == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rows, err := s.wizardSessions.ListByOperator(ctx, operator, 100)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row == nil || row.ID != sid {
			continue
		}
		if row.CommittedProjectID != nil || row.CancelledAt != nil {
			return // closed draft — nothing to resume
		}
		data.ResumeSessionID = row.ID
		data.ResumeTranscriptJSON = resumeTranscriptJSON(row.Transcript)
		return
	}
}

// resumeTranscriptJSON projects a stored wizard transcript down to a
// {role,content} JS array for replay. Returns "[]" on empty/invalid
// input rather than failing the page render.
func resumeTranscriptJSON(raw []byte) template.JS {
	if len(raw) == 0 {
		return template.JS("[]")
	}
	var turns []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &turns); err != nil {
		return template.JS("[]")
	}
	out := make([]map[string]string, 0, len(turns))
	for _, t := range turns {
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		out = append(out, map[string]string{"role": t.Role, "content": t.Content})
	}
	// json.Marshal escapes <,>,& (HTML escaping on by default), so the
	// result is safe to inline inside a <script> block.
	b, err := json.Marshal(out)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}
