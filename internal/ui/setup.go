package ui

import (
	"net/http"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/onboarding"
)

// SetupPageData is the server-rendered payload for /ui/setup.
type SetupPageData struct {
	Title       string
	CurrentPage string
	Status      onboarding.Status
	// Prefill carries the last proposed chat config from an existing
	// onboarding session, so a resumed flow shows prior input. Empty
	// on a fresh install.
	Prefill onboarding.ChatConfigProposal
}

// Setup renders the installation onboarding landing page.
func (s *Server) Setup(w http.ResponseWriter, r *http.Request) {
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := SetupPageData{
		Title:       "Setup guide",
		CurrentPage: "setup",
		Status:      s.setupStatus(r),
		Prefill:     s.setupPrefill(r),
	}
	s.render(w, "setup.html", data)
}

// setupStatus resolves the installation's onboarding state. When the
// detector's Sessions repo is nil or returns an error, Detect falls back
// to a conservative heuristic based on config fields. When Config is nil
// (should not happen in production — container_http.go always wires it),
// Detect reports FreshInstall=true with reason "config unavailable".
func (s *Server) setupStatus(r *http.Request) onboarding.Status {
	return s.onboardingDetector.Detect(r.Context())
}

// setupPrefill loads the most recent uncommitted onboarding session's
// proposed chat config, if any, so a resumed flow pre-fills the form.
// Returns the zero value when no session repo is wired or no session
// exists. The UI server does not own the session repo in this slice
// (the API does), so this returns zero until the UI is wired to read
// sessions in a follow-on; the form renders empty for now.
func (s *Server) setupPrefill(_ *http.Request) onboarding.ChatConfigProposal {
	return onboarding.ChatConfigProposal{}
}
