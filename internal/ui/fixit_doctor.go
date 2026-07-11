package ui

// Fix-It Doctor repair chat panel (task 3.2, fix-it-doctor-design.md
// §5.2/§5.4). Server-rendered Go html/template + HTMX — no SPA: GET
// renders the full page (fresh session or, with ?session=<id>, a
// resumed one); the composer form hx-posts to /message and swaps in
// the re-rendered transcript partial.
//
// Mutating action cards (config_apply_gate / config_apply / retry_task
// / reprobe_integration / set_secret) render an Apply affordance on the
// session's MOST RECENT turn only (task 3.3's deny-by-default
// dispatcher, fix-it-doctor-design.md §5.4) — older turns' actions are
// historical and no longer indexable (Dispatch always resolves against
// the session's LastEnvelope). ActionKindLinkOut is pure client-side
// navigation, no server mutation, so it renders as a real link on any
// turn.
//
// Entry-point buttons deep-link here (task 3.4): the failed-task
// recovery card, the integration tile's "Help me fix this" (hidden
// unless red + the doctor is wired), and the restart banner. The
// feature-doctor drill-down entry point has no host surface yet (no
// admin page renders featuredoctor.Diagnosis today — see the task 3.4
// report) — this panel remains directly reachable at
// /ui/fixit/{kind}/{id} regardless (tests, and any future caller).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// FixItSessionReader is the narrow interface the panel needs to
// resume a session's transcript. Concrete impl is
// persistence.FixItSessionRepository; kept narrow (Get only) so tests
// don't need the full repository surface.
type FixItSessionReader interface {
	Get(ctx context.Context, id string) (*persistence.FixItSession, error)
}

// WithFixItSessionReader wires the transcript-resume source for the
// /ui/fixit panel. Optional — nil means resume (?session=) silently
// falls back to a fresh page rather than 500ing.
func WithFixItSessionReader(src FixItSessionReader) ServerOption {
	return func(s *Server) { s.fixItSessions = src }
}

// fixItTurnView is one rendered transcript row.
type fixItTurnView struct {
	Role     string // "user" | "assistant"
	Content  string
	Actions  []fixItActionView
	Resolved bool
}

// fixItActionView is one proposed-action card. Executable is true ONLY
// for link_out (pure navigation, no mutation). Applyable is true for
// every OTHER kind, but ONLY on the session's most recent turn (see
// decodeFixItTurns) — ActionIndex is this action's position in that
// turn's envelope, the exact index task 3.3's Dispatch resolves
// against. NeedsSecretValue marks set_secret's card so the template
// renders a masked value input instead of a bare Apply button (the
// value must come from the operator, never the model — see
// fixitdoctor.SecretSetter's doc comment).
type fixItActionView struct {
	Kind             string
	Label            string
	Params           map[string]string
	Executable       bool
	LinkURL          string
	Applyable        bool
	ActionIndex      int
	NeedsSecretValue bool
}

// FixItPanelData is the template payload for /ui/fixit/{kind}/{id}.
type FixItPanelData struct {
	Title       string
	CurrentPage string

	Configured bool // false when the doctor isn't wired on this deployment
	Kind       string
	RefID      string
	ProjectID  string
	SessionID  string // "" for a fresh session
	Turns      []fixItTurnView
	StatusPoll *api.FixItStatusPoll
	Notice     string
	// RollbackID is the ControlPlaneProposal id of the most recent
	// successfully-applied config_apply action, if any — the §5.4
	// "Rollback affordance" shown for the session's lifetime. Empty
	// when no config_apply has been applied yet (or it was already
	// rolled back).
	RollbackID string
}

// FixItDoctorPanel handles GET /ui/fixit/{kind}/{id}. With
// ?session=<id> it resumes one of the requesting operator's own
// sessions for this (kind, id) pair; an unknown/foreign/out-of-scope
// session id is silently ignored (the page renders fresh) — same
// convention as the project wizard's populateWizardResume.
func (s *Server) FixItDoctorPanel(w http.ResponseWriter, r *http.Request, kind, refID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := FixItPanelData{
		Title:       "Fix-It Doctor",
		CurrentPage: "fixit",
		Configured:  s.fixItDoctor != nil,
		Kind:        kind,
		RefID:       refID,
		ProjectID:   strings.TrimSpace(r.URL.Query().Get("project")),
	}

	if sid := strings.TrimSpace(r.URL.Query().Get("session")); sid != "" {
		s.populateFixItResume(r, &data, sid)
	}

	if !s.uiRequireProjectScope(w, r, data.ProjectID) {
		return
	}

	s.render(w, "fixit_doctor.html", data)
}

// populateFixItResume seeds data for a resumed session, but ONLY when
// sid belongs to the requesting operator and matches the (kind, refID)
// the panel was opened on. A mismatched/foreign/unknown id is silently
// ignored — the page renders fresh rather than erroring, so a crafted
// ?session= can't leak another operator's transcript.
func (s *Server) populateFixItResume(r *http.Request, data *FixItPanelData, sid string) {
	if s.fixItSessions == nil {
		return
	}
	operator := s.operatorIDForRequest(r)
	if operator == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	row, err := s.fixItSessions.Get(ctx, sid)
	if err != nil || row == nil || row.OperatorID != operator {
		return
	}
	if row.FailureKind != data.Kind || row.FailureRefID != data.RefID {
		return
	}
	data.SessionID = row.ID
	data.ProjectID = row.ProjectID
	data.Turns = decodeFixItTurns(row.Transcript)
	data.RollbackID = latestRollbackID(row.AppliedActions)
}

// fixItCallerIsAdmin mirrors ui/integrations.go's integrationsCaller
// convention exactly: an UNSCOPED request (admin session, auth-disabled
// install, or an unscoped API key) is treated as admin-class; a
// project-scoped session/key is not. Used to gate daemon-scope fixit
// actions (an empty ProjectID) to admin-class callers only, per
// fix-it-doctor-design.md §7 ("daemon-scope actions admin-only").
func fixItCallerIsAdmin(r *http.Request) bool {
	_, scoped := api.RequestScopedProjects(r)
	return !scoped
}

// fixItAppliedActionRow projects one fixit_sessions.applied_actions
// entry — duplicated here (rather than importing internal/fixitdoctor's
// unexported record shape) for the same "pure JSON projection" reason
// fixItTranscriptTurn duplicates Turn's shape.
type fixItAppliedActionRow struct {
	Kind       string `json:"kind"`
	Result     string `json:"result"`
	RollbackID string `json:"rollback_id,omitempty"`
}

// latestRollbackID returns the RollbackID of the most recent
// successfully-applied action that carries one (a config_apply), or ""
// when none exists yet — the §5.4 Rollback affordance persisted for the
// session's lifetime.
func latestRollbackID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var rows []fixItAppliedActionRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return ""
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Result == "applied" && rows[i].RollbackID != "" {
			return rows[i].RollbackID
		}
		if rows[i].Kind == "config_apply_rollback" && rows[i].Result == "applied" {
			// A rollback already happened after this point — nothing to
			// roll back again.
			return ""
		}
	}
	return ""
}

// fixItDoctorRouter dispatches /fixit/{kind}/{id}[/message |
// /actions/{n}/apply | /actions/rollback].
func (s *Server) fixItDoctorRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/fixit/")
	if strings.HasSuffix(path, "/message") {
		rest := strings.TrimSuffix(path, "/message")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		s.FixItDoctorMessage(w, r, parts[0], parts[1])
		return
	}
	if strings.HasSuffix(path, "/actions/rollback") {
		rest := strings.TrimSuffix(path, "/actions/rollback")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		s.FixItDoctorRollback(w, r, parts[0], parts[1])
		return
	}
	if idx := strings.Index(path, "/actions/"); idx >= 0 && strings.HasSuffix(path, "/apply") {
		prefix := path[:idx]
		mid := strings.TrimSuffix(path[idx+len("/actions/"):], "/apply")
		parts := strings.SplitN(prefix, "/", 2)
		n, numErr := strconv.Atoi(mid)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || numErr != nil || n < 0 {
			http.NotFound(w, r)
			return
		}
		s.FixItDoctorApply(w, r, parts[0], parts[1], n)
		return
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	s.FixItDoctorPanel(w, r, parts[0], parts[1])
}

// fixItScopeAndAdminGate resolves sessionID's OWN scope (never trusting
// a request-body project id — same convention as resolveFixItScope in
// api/fixit_handlers.go), re-checks it against the caller's allowed
// projects (404 on mismatch — no existence leak), and additionally
// requires an admin-class caller for a daemon-scope session (empty
// project). This is the SCOPE RE-CHECK ON EVERY APPLY the design's §5.4/
// §7 calls for — Converse's own scope gate at session-open time is not
// sufficient by itself because the ref could be tampered (or the
// caller's own grants revoked) between open and apply. Returns
// ("", false) after already writing the 404/not-found response.
func (s *Server) fixItScopeAndAdminGate(w http.ResponseWriter, r *http.Request, sessionID, operator string) (projectID string, ok bool) {
	scopedProject, found, err := s.fixItDoctor.SessionScope(r.Context(), sessionID, operator)
	if err != nil || !found {
		http.NotFound(w, r)
		return "", false
	}
	if !s.uiRequireProjectScope(w, r, scopedProject) {
		return "", false
	}
	if scopedProject == "" && !fixItCallerIsAdmin(r) {
		http.NotFound(w, r)
		return "", false
	}
	return scopedProject, true
}

// FixItDoctorMessage handles POST /ui/fixit/{kind}/{id}/message — one
// repair-chat turn. Always renders the "fixit_transcript" HTMX
// fragment (never a redirect), including the error case, so the
// composer's hx-target div always gets feedback.
func (s *Server) FixItDoctorMessage(w http.ResponseWriter, r *http.Request, kind, refID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.fixItDoctor == nil {
		s.renderFixItTranscript(w, FixItPanelData{Notice: "Fix-It Doctor is not configured on this deployment."})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	operator := s.operatorIDForRequest(r)
	if operator == "" {
		http.Error(w, "operator identity required", http.StatusUnauthorized)
		return
	}

	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	projectID := strings.TrimSpace(r.FormValue("project"))
	message := strings.TrimSpace(r.FormValue("message"))

	ctx := r.Context()

	if sessionID != "" {
		scopedProject, ok, err := s.fixItDoctor.SessionScope(ctx, sessionID, operator)
		if err != nil || !ok {
			http.NotFound(w, r)
			return
		}
		projectID = scopedProject
	}
	if !s.uiRequireProjectScope(w, r, projectID) {
		return
	}

	if message == "" {
		s.renderFixItTranscript(w, FixItPanelData{
			Kind: kind, RefID: refID, SessionID: sessionID, ProjectID: projectID,
			Notice: "Message is required.",
		})
		return
	}

	result, err := s.fixItDoctor.Converse(ctx, sessionID, operator, kind, refID, projectID, message)
	if err != nil {
		s.renderFixItTranscript(w, FixItPanelData{
			Kind: kind, RefID: refID, SessionID: sessionID, ProjectID: projectID,
			Notice: "Turn failed: " + err.Error(),
		})
		return
	}

	data := FixItPanelData{Kind: kind, RefID: refID, ProjectID: projectID}
	if result != nil {
		data.SessionID = result.SessionID
		data.StatusPoll = result.StatusPoll
	}
	// Re-fetch the persisted transcript so the fragment shows the full
	// history (Converse's result only carries this turn's envelope).
	if s.fixItSessions != nil && data.SessionID != "" {
		if row, err := s.fixItSessions.Get(ctx, data.SessionID); err == nil && row != nil {
			data.Turns = decodeFixItTurns(row.Transcript)
			data.RollbackID = latestRollbackID(row.AppliedActions)
		}
	}
	s.renderFixItTranscript(w, data)
}

// FixItDoctorApply handles POST /ui/fixit/{kind}/{id}/actions/{n}/apply
// — the task 3.3 Apply button on one proposed-action card (§5.4).
// actionIndex resolves against the session's LAST envelope, exactly
// what fixitdoctor.Service.Dispatch resolves against; secret_value
// (masked, user-typed) is the ONLY source of a set_secret value —
// never anything the model proposed. Always renders the
// "fixItTranscript" fragment, including refusals, so the card's
// hx-target always gets feedback.
func (s *Server) FixItDoctorApply(w http.ResponseWriter, r *http.Request, kind, refID string, actionIndex int) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.fixItDoctor == nil {
		s.renderFixItTranscript(w, FixItPanelData{Notice: "Fix-It Doctor is not configured on this deployment."})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	operator := s.operatorIDForRequest(r)
	if operator == "" {
		http.Error(w, "operator identity required", http.StatusUnauthorized)
		return
	}
	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	secretValue := r.FormValue("secret_value")
	if sessionID == "" {
		s.renderFixItTranscript(w, FixItPanelData{Kind: kind, RefID: refID, Notice: "No active session to apply an action on."})
		return
	}

	// Scope RE-CHECK on EVERY apply (defense in depth — the ref could be
	// tampered between open and apply, §5.4/§7) — never trust the panel's
	// own hidden "project" field for this decision.
	projectID, ok := s.fixItScopeAndAdminGate(w, r, sessionID, operator)
	if !ok {
		return
	}

	ctx := r.Context()
	result, err := s.fixItDoctor.Apply(ctx, sessionID, operator, actionIndex, secretValue)
	data := FixItPanelData{Kind: kind, RefID: refID, SessionID: sessionID, ProjectID: projectID}
	switch {
	case err != nil:
		data.Notice = "Apply failed: " + err.Error()
	case result != nil:
		data.Notice = fmt.Sprintf("[%s] %s", result.Result, result.Detail)
	}
	if s.fixItSessions != nil {
		if row, gerr := s.fixItSessions.Get(ctx, sessionID); gerr == nil && row != nil {
			data.Turns = decodeFixItTurns(row.Transcript)
			data.RollbackID = latestRollbackID(row.AppliedActions)
		}
	}
	s.renderFixItTranscript(w, data)
}

// FixItDoctorRollback handles POST /ui/fixit/{kind}/{id}/actions/rollback
// — the §5.4 Rollback affordance for a previously-applied config_apply.
// proposal_id comes from the hidden field the template renders from
// FixItPanelData.RollbackID — never from anywhere the model could reach.
func (s *Server) FixItDoctorRollback(w http.ResponseWriter, r *http.Request, kind, refID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.fixItDoctor == nil {
		s.renderFixItTranscript(w, FixItPanelData{Notice: "Fix-It Doctor is not configured on this deployment."})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	operator := s.operatorIDForRequest(r)
	if operator == "" {
		http.Error(w, "operator identity required", http.StatusUnauthorized)
		return
	}
	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	proposalID := strings.TrimSpace(r.FormValue("proposal_id"))
	if sessionID == "" || proposalID == "" {
		s.renderFixItTranscript(w, FixItPanelData{Kind: kind, RefID: refID, Notice: "Nothing to roll back."})
		return
	}

	projectID, ok := s.fixItScopeAndAdminGate(w, r, sessionID, operator)
	if !ok {
		return
	}

	ctx := r.Context()
	result, err := s.fixItDoctor.Rollback(ctx, sessionID, operator, proposalID)
	data := FixItPanelData{Kind: kind, RefID: refID, SessionID: sessionID, ProjectID: projectID}
	switch {
	case err != nil:
		data.Notice = "Rollback failed: " + err.Error()
	case result != nil:
		data.Notice = fmt.Sprintf("[%s] %s", result.Result, result.Detail)
	}
	if s.fixItSessions != nil {
		if row, gerr := s.fixItSessions.Get(ctx, sessionID); gerr == nil && row != nil {
			data.Turns = decodeFixItTurns(row.Transcript)
			data.RollbackID = latestRollbackID(row.AppliedActions)
		}
	}
	s.renderFixItTranscript(w, data)
}

func (s *Server) renderFixItTranscript(w http.ResponseWriter, data FixItPanelData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "fixItTranscript", data); err != nil {
		s.logger.Error().Err(err).Msg("renderFixItTranscript: template failed")
	}
}

// fixItTranscriptTurn is the on-disk shape fixitdoctor.Turn marshals
// to — duplicated here (rather than importing internal/fixitdoctor's
// Turn type) so the UI package's transcript decode stays a pure,
// dependency-free JSON projection, the same pattern
// resumeTranscriptJSON uses for the project wizard.
type fixItTranscriptTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	Envelope  *struct {
		Message  string `json:"message"`
		Resolved bool   `json:"resolved"`
		Actions  []struct {
			Kind   string            `json:"kind"`
			Label  string            `json:"label"`
			Params map[string]string `json:"params"`
		} `json:"actions"`
	} `json:"envelope,omitempty"`
}

// decodeFixItTurns projects a stored fixit_sessions.transcript blob
// into render-ready rows. Returns nil (not an error) on
// missing/invalid input — a resume that can't decode degrades to an
// empty transcript rather than failing the page.
func decodeFixItTurns(raw []byte) []fixItTurnView {
	if len(raw) == 0 {
		return nil
	}
	var turns []fixItTranscriptTurn
	if err := json.Unmarshal(raw, &turns); err != nil {
		return nil
	}
	// The Apply affordance is only meaningful on the LAST turn that
	// carries an envelope — fixitdoctor.Service.Dispatch always resolves
	// actionIndex against session.LastEnvelope, so an older turn's
	// actions are historical and not indexable (and may since have been
	// superseded by a newer proposal, or a system turn recording a prior
	// apply's result may have been appended after it — see
	// recordAppliedAction in dispatch.go).
	lastEnvelopeTurn := -1
	for i, t := range turns {
		if t.Envelope != nil {
			lastEnvelopeTurn = i
		}
	}

	out := make([]fixItTurnView, 0, len(turns))
	for i, t := range turns {
		view := fixItTurnView{Role: t.Role, Content: t.Content}
		if t.Envelope != nil {
			view.Resolved = t.Envelope.Resolved
			isLast := i == lastEnvelopeTurn
			for ai, a := range t.Envelope.Actions {
				out2 := fixItActionView{Kind: a.Kind, Label: a.Label, Params: a.Params}
				if a.Kind == "link_out" {
					if url := strings.TrimSpace(a.Params["url"]); safeFixItLinkOutURL(url) {
						out2.Executable = true
						out2.LinkURL = url
					}
				} else if isLast {
					out2.Applyable = true
					out2.ActionIndex = ai
					out2.NeedsSecretValue = a.Kind == "set_secret"
				}
				view.Actions = append(view.Actions, out2)
			}
		}
		out = append(out, view)
	}
	return out
}

// safeFixItLinkOutURL is the template-render-time defense-in-depth
// check mirroring fixitdoctor.validLinkOutURL — the server already
// validates this before the action ever reaches the session's
// persisted envelope, but the panel re-checks before turning it into
// a clickable <a href> so a bug in an older persisted row (or a future
// server-side regression) can't resurface as a live redirect out of
// the operator's control.
func safeFixItLinkOutURL(url string) bool {
	if url == "" || !strings.HasPrefix(url, "/") || strings.HasPrefix(url, "//") {
		return false
	}
	if strings.Contains(url, "://") || strings.ContainsAny(url, " \t\n\r") {
		return false
	}
	return true
}
