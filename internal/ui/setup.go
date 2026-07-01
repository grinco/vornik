package ui

import (
	"net/http"
	"regexp"
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
	// DispatcherProjectID backs the final say-hello step's deep link into
	// the dispatcher project's chat once the dispatcher is pinned.
	DispatcherProjectID string
	ProjectOptions      []SetupProjectOption
	// Prefill carries the current chat config so completed setup stays
	// editable without exposing env-placeholder secrets.
	Prefill       onboarding.ChatConfigProposal
	MemoryPrefill onboarding.MemoryConfigProposal
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
		Title:         "Setup guide",
		CurrentPage:   "setup",
		Status:        status,
		Prefill:       s.setupPrefill(r),
		MemoryPrefill: s.setupMemoryPrefill(),
	}
	data.ChatConfigured, data.MemoryConfigured, data.DispatcherConfigured = s.setupStepState()
	if cfg := s.onboardingDetector.Config; cfg != nil {
		data.DispatcherProjectID = s.sayHelloProjectID(strings.TrimSpace(cfg.Telegram.DispatcherProjectID))
	}
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

// setupPrefill loads current chat config so the form can be edited after a
// completed setup. Env-placeholder secrets are intentionally left blank.
func (s *Server) setupPrefill(_ *http.Request) onboarding.ChatConfigProposal {
	cfg := s.onboardingDetector.Config
	if cfg == nil {
		return onboarding.ChatConfigProposal{}
	}
	apiKey := strings.TrimSpace(cfg.Chat.APIKey)
	if strings.HasPrefix(apiKey, "${") {
		apiKey = ""
	}
	return onboarding.ChatConfigProposal{
		Endpoint: strings.TrimSpace(cfg.Chat.Endpoint),
		APIKey:   apiKey,
		Model:    strings.TrimSpace(cfg.Chat.Model),
	}
}

func (s *Server) setupMemoryPrefill() onboarding.MemoryConfigProposal {
	cfg := s.onboardingDetector.Config
	if cfg == nil {
		return onboarding.MemoryConfigProposal{}
	}
	apiKey := strings.TrimSpace(cfg.Memory.EmbeddingAPIKey)
	if strings.HasPrefix(apiKey, "${") {
		apiKey = ""
	}
	return onboarding.MemoryConfigProposal{
		Enabled:            cfg.Memory.Enabled,
		EmbeddingEndpoint:  strings.TrimSpace(cfg.Memory.EmbeddingEndpoint),
		EmbeddingAPIKey:    apiKey,
		EmbeddingModel:     strings.TrimSpace(cfg.Memory.EmbeddingModel),
		EmbeddingDimension: cfg.Memory.EmbeddingDimension,
	}
}

// setupStepState derives the per-step completion flags the template gates
// on. The dispatcher step (and the create-project CTA) deliberately unlock
// on chatConfigured ALONE: memory is optional — SetupMemoryCommit supports
// committing enabled=false, and the server-side SetupDispatcherCommit never
// required memory either. The old UI-only chat+memory gate permanently
// dead-ended chat-only installs after the restart (regression test:
// TestSetupPage_MemoryOptOutStillUnlocksDispatcher). memoryConfigured is
// still reported so the memory step can show Completed vs Optional.
func (s *Server) setupStepState() (chatConfigured, memoryConfigured, dispatcherConfigured bool) {
	cfg := s.onboardingDetector.Config
	if cfg == nil {
		return false, false, false
	}
	chatConfigured = cfg.Chat.Enabled &&
		strings.TrimSpace(cfg.Chat.Provider) != "" &&
		strings.TrimSpace(cfg.Chat.Endpoint) != "" &&
		strings.TrimSpace(cfg.Chat.Model) != "" &&
		strings.TrimSpace(cfg.Chat.APIKey) != ""
	memoryConfigured = cfg.Memory.Enabled &&
		strings.TrimSpace(cfg.Memory.EmbeddingModel) != "" &&
		strings.TrimSpace(cfg.Memory.EmbeddingEndpoint) != ""
	dispatcherConfigured = strings.TrimSpace(cfg.Telegram.DispatcherProjectID) != ""
	return chatConfigured, memoryConfigured, dispatcherConfigured
}

// sayHelloProjectIDPattern is deliberately tighter than what the registry
// accepts: alphanumeric start, then alphanumerics, dot, dash, underscore.
// It exists so a hand-edited telegram.dispatcher_project_id can never put
// HTML/URL metacharacters into the Step-5 href — defense-in-depth on top
// of html/template's contextual escaping (review-20260701-12e3.md).
var sayHelloProjectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// sayHelloProjectID returns id only when it is shaped like a project ID
// and is currently loaded in the registry. Anything else returns "" so the
// Step-5 say-hello section renders its Waiting fallback instead of a chat
// link that would 404 (deleted or typo'd dispatcher project).
func (s *Server) sayHelloProjectID(id string) string {
	if id == "" || !sayHelloProjectIDPattern.MatchString(id) {
		return ""
	}
	if s.projectReg == nil || s.projectReg.GetProject(id) == nil {
		return ""
	}
	return id
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
