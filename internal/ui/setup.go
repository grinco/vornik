package ui

import (
	"net/http"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/onboarding"
)

// SetupPageData is the server-rendered payload for /ui/setup.
type SetupPageData struct {
	Title                string
	CurrentPage          string
	Status               onboarding.Status
	ChatConfigured       bool
	MemoryConfigured     bool
	DispatcherConfigured bool
	ProjectOptions       []SetupProjectOption
	// Prefill carries the last proposed chat config from an existing
	// onboarding session, so a resumed flow shows prior input. Empty
	// on a fresh install.
	Prefill onboarding.ChatConfigProposal
}

type SetupProjectOption struct {
	ID          string
	DisplayName string
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
	status := s.setupStatus(r)
	data := SetupPageData{
		Title:       "Setup guide",
		CurrentPage: "setup",
		Status:      status,
		Prefill:     s.setupPrefill(r),
	}
	data.ChatConfigured, data.MemoryConfigured, data.DispatcherConfigured = s.setupStepState()
	data.ProjectOptions = s.setupProjectOptions()
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

func (s *Server) setupStepState() (chatConfigured, memoryConfigured, dispatcherConfigured bool) {
	cfg := s.onboardingDetector.Config
	if cfg == nil {
		return false, false, false
	}
	chatConfigured = strings.TrimSpace(cfg.Chat.Endpoint) != "" && strings.TrimSpace(cfg.Chat.Model) != ""
	memoryConfigured = chatConfigured && (!cfg.Memory.Enabled ||
		(strings.TrimSpace(cfg.Memory.EmbeddingModel) != "" && strings.TrimSpace(cfg.Memory.EmbeddingEndpoint) != ""))
	dispatcherConfigured = strings.TrimSpace(cfg.Telegram.DispatcherProjectID) != ""
	return chatConfigured, memoryConfigured, dispatcherConfigured
}

func (s *Server) setupProjectOptions() []SetupProjectOption {
	if s.projectReg == nil {
		return nil
	}
	projects := s.projectReg.ListProjects()
	out := make([]SetupProjectOption, 0, len(projects))
	for _, p := range projects {
		if p == nil {
			continue
		}
		out = append(out, SetupProjectOption{ID: p.ID, DisplayName: p.DisplayName})
	}
	return out
}
