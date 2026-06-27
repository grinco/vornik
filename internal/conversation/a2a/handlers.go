package a2a

// HTTP handlers for the A2A inbound surface.
//
//   - GET  /.well-known/agent.json                       — card index
//   - GET  /.well-known/agent.json/<project>/<workflow>  — per-agent card
//   - GET  /a2a/v1/agents/<project>/<workflow>/card      — same, mounted
//                                                          under the API
//                                                          path so a
//                                                          public-base
//                                                          URL pointing
//                                                          at /api routes
//                                                          still works
//   - POST /a2a/v1/agents/<project>/<workflow>/tasks     — submit a task
//   - GET  /a2a/v1/agents/<project>/<workflow>/tasks/{id} — SSE stream
//
// Auth model:
//   - Card endpoints are public (per spec). The middleware skip list
//     handles the well-known path; the /a2a/v1/.../card path is also
//     unauthenticated.
//   - Task submission + SSE go through the standard API-key middleware
//     applied to /api/v1 by the surrounding mux.
//
// Concrete handler wiring lives in internal/api/routes.go; this
// package only carries the handler implementations + glue.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

const maxTaskSubmitBodyBytes = 1 << 20

// PublicBaseURLProvider returns the daemon's externally-reachable
// base URL (without trailing slash). Implementations typically
// pull from config.PublicBaseURL or fall back to the request's
// Host header. Defined as an interface so the handlers stay
// testable.
type PublicBaseURLProvider interface {
	PublicBaseURL() string
}

// PublicBaseURLFunc adapts a plain func to the interface — the
// production wiring usually has the URL on a *config.Config so a
// closure over `cfg.Public.BaseURL` is the natural shape.
type PublicBaseURLFunc func() string

func (f PublicBaseURLFunc) PublicBaseURL() string { return f() }

// RegistrySnapshot is the narrow read-only view the handlers need
// from the project registry. Mirrors the existing project /
// workflow read patterns in internal/api so the test fakes are
// trivial.
type RegistrySnapshot interface {
	GetProject(id string) *registry.Project
	GetWorkflow(id string) *registry.Workflow
	ListProjects() []*registry.Project
	ListWorkflows() []*registry.Workflow
}

// TaskCreator is the narrow surface the A2A submit handler calls
// into. Concrete impl is internal/taskcreate.Creator; defined here
// so the package doesn't import taskcreate (which would pull in
// most of the persistence layer + circular import risk).
type TaskCreator interface {
	Create(ctx context.Context, params TaskCreateParams) (*persistence.Task, error)
}

// TaskCreateParams mirrors taskcreate.Params but only the fields
// the A2A handler sets. Concrete types in internal/taskcreate
// translate at the call site.
type TaskCreateParams struct {
	ProjectID      string
	WorkflowID     string
	TaskType       string
	Prompt         string
	Priority       int
	CreationSource persistence.TaskCreationSource
	ExtraContext   map[string]any
}

// LiveSubscriber is the SSE bridge's read surface. Mirrors the
// existing api.LiveSubscriber so the wiring side just passes the
// daemon's publisher through.
type LiveSubscriber interface {
	Subscribe(executionID string, fromSeq int64) (events <-chan livepubsub.LiveEvent, cancel func(), err error)
}

// Handler bundles every dependency the A2A endpoints need.
// One Handler per daemon; methods are safe to call concurrently
// across requests.
type Handler struct {
	BaseURLProvider PublicBaseURLProvider
	Registry        RegistrySnapshot
	TaskCreator     TaskCreator
	LiveSubscriber  LiveSubscriber
	// PushConfigStore persists a caller-supplied pushNotificationConfig so the
	// daemon can POST task-state updates to the webhook even when the caller
	// isn't streaming. Nil disables push config (agent card advertises it off).
	PushConfigStore PushConfigStore
	Logger          zerolog.Logger
}

// PushConfigStore is the persistence surface the submit handler + the
// pushNotificationConfig set/get endpoints need. Satisfied by
// persistence.A2APushConfigRepository.
type PushConfigStore interface {
	Set(ctx context.Context, cfg persistence.A2APushConfig) error
	Get(ctx context.Context, taskID string) (*persistence.A2APushConfig, error)
}

// listPublishedAgents walks the registry, picks every workflow
// with A2A.Publish=true, and pairs it with each project that
// references it. Deterministic ordering: (projectID, workflowID).
// Returns an empty slice (not nil) so callers can range without
// nil checks.
func (h *Handler) listPublishedAgents() []PublishedAgent {
	if h.Registry == nil {
		return []PublishedAgent{}
	}
	base := ""
	if h.BaseURLProvider != nil {
		base = h.BaseURLProvider.PublicBaseURL()
	}
	out := make([]PublishedAgent, 0)
	projects := h.Registry.ListProjects()
	for _, p := range projects {
		if p == nil {
			continue
		}
		// Walk workflows: today every project has a default
		// workflow; future projects with explicit workflow
		// allowlists would walk that instead. Use the
		// workflow registry as source of truth — the workflow
		// itself declares publish opt-in, not the project.
		wfs := h.Registry.ListWorkflows()
		for _, wf := range wfs {
			if wf == nil || !wf.A2A.Publish {
				continue
			}
			// Only publish when the project actually allows
			// this workflow. The simplest "allows" check is
			// "it's the project's default workflow" — refine
			// when per-project workflow allowlists land.
			if p.DefaultWorkflowID != wf.ID {
				continue
			}
			card, err := BuildAgentCard(base, p.ID, wf, h.PushConfigStore != nil)
			if err != nil {
				continue
			}
			out = append(out, PublishedAgent{
				ProjectID:  p.ID,
				WorkflowID: wf.ID,
				Card:       card,
			})
		}
	}
	return out
}

// findPublishedAgent looks up one (project, workflow) pair.
// Returns nil when the pair isn't published — handlers translate
// to a 404.
func (h *Handler) findPublishedAgent(projectID, workflowID string) *PublishedAgent {
	for _, a := range h.listPublishedAgents() {
		if a.ProjectID == projectID && a.WorkflowID == workflowID {
			out := a
			return &out
		}
	}
	return nil
}

// HandleWellKnown serves GET /.well-known/agent.json. Returns
// the agent-card index for the daemon. Public endpoint.
func (h *Handler) HandleWellKnown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	// `/.well-known/agent.json/<project>/<workflow>` returns the
	// per-agent card. Strip the prefix and dispatch.
	rest := strings.TrimPrefix(r.URL.Path, "/.well-known/agent.json")
	if rest == "" || rest == "/" {
		writeJSON(w, http.StatusOK, BuildAgentCardIndex(h.listPublishedAgents()))
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "expected /.well-known/agent.json or /.well-known/agent.json/<project>/<workflow>")
		return
	}
	agent := h.findPublishedAgent(parts[0], parts[1])
	if agent == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no published A2A agent for "+parts[0]+"/"+parts[1])
		return
	}
	writeJSON(w, http.StatusOK, agent.Card)
}

// HandleAgentRoute is the authenticated counterpart router under
// /a2a/v1/agents/. It dispatches to the per-agent card, task
// submit, and SSE handlers based on the URL suffix.
func (h *Handler) HandleAgentRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/a2a/v1/agents/")
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "expected /a2a/v1/agents/<project>/<workflow>/...")
		return
	}
	projectID, workflowID := parts[0], parts[1]
	suffix := ""
	if len(parts) > 2 {
		suffix = strings.Join(parts[2:], "/")
	}
	agent := h.findPublishedAgent(projectID, workflowID)
	if agent == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no published A2A agent for "+projectID+"/"+workflowID)
		return
	}
	switch {
	case suffix == "card":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
			return
		}
		writeJSON(w, http.StatusOK, agent.Card)
	case suffix == "tasks":
		h.handleTaskSubmit(w, r, agent)
	case strings.HasPrefix(suffix, "tasks/") && strings.HasSuffix(suffix, "/pushNotificationConfig"):
		taskID := strings.TrimSuffix(strings.TrimPrefix(suffix, "tasks/"), "/pushNotificationConfig")
		h.handlePushConfig(w, r, agent, taskID)
	case strings.HasPrefix(suffix, "tasks/"):
		taskID := strings.TrimPrefix(suffix, "tasks/")
		h.handleTaskStream(w, r, agent, taskID)
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown A2A endpoint")
	}
}

// taskSubmitRequest is the body shape POST /tasks accepts. We
// implement a deliberately small subset of the A2A spec — `text`
// parts only — and reject everything else with a 422 explaining
// the gap. Multi-part / file uploads land in Phase B.
type taskSubmitRequest struct {
	Message struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"message"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Configuration carries optional A2A send-configuration. We honour
	// pushNotificationConfig so a caller can register a webhook for
	// task-state updates without holding an SSE stream open.
	Configuration struct {
		PushNotificationConfig *struct {
			URL   string `json:"url"`
			Token string `json:"token,omitempty"`
		} `json:"pushNotificationConfig,omitempty"`
	} `json:"configuration,omitempty"`
}

// taskSubmitResponse mirrors the A2A spec's task creation
// response: a task identifier plus the URL the client polls /
// streams from.
type taskSubmitResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	StreamURL string `json:"streamUrl"`
}

func (h *Handler) handleTaskSubmit(w http.ResponseWriter, r *http.Request, agent *PublishedAgent) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if h.TaskCreator == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "task creation is not configured on this daemon")
		return
	}
	var req taskSubmitRequest
	body := http.MaxBytesReader(w, r.Body, maxTaskSubmitBodyBytes)
	defer func() { _ = body.Close() }()
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	prompt := extractTextPrompt(req.Message.Parts)
	if prompt == "" {
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"only text parts are supported in this slice; submit message.parts[].text")
		return
	}
	// Validate any push-notification webhook BEFORE creating the task, so a
	// bad url is a clean 422 instead of a created-but-unnotifiable task.
	pushCfg := req.Configuration.PushNotificationConfig
	if pushCfg != nil {
		if h.PushConfigStore == nil {
			writeError(w, http.StatusUnprocessableEntity, "PUSH_UNSUPPORTED",
				"pushNotificationConfig is not configured on this daemon")
			return
		}
		if err := ValidateWebhookURL(pushCfg.URL); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			return
		}
	}
	task, err := h.TaskCreator.Create(r.Context(), TaskCreateParams{
		ProjectID:      agent.ProjectID,
		WorkflowID:     agent.WorkflowID,
		TaskType:       agent.WorkflowID,
		Prompt:         prompt,
		CreationSource: persistence.TaskCreationSourceA2A,
		ExtraContext: map[string]any{
			"a2a": map[string]any{
				"submitted_at": time.Now().UTC().Format(time.RFC3339),
				"metadata":     req.Metadata,
			},
		},
	})
	if err != nil {
		h.Logger.Warn().Err(err).Str("project", agent.ProjectID).Str("workflow", agent.WorkflowID).Msg("a2a: task creation failed")
		writeError(w, http.StatusBadRequest, "TASK_CREATE_FAILED", err.Error())
		return
	}
	// Persist the webhook config now that we have the task ID. Best-effort:
	// a store failure shouldn't fail the (already-created) task — log + carry
	// on; the caller can still stream via SSE.
	if pushCfg != nil && h.PushConfigStore != nil {
		if err := h.PushConfigStore.Set(r.Context(), persistence.A2APushConfig{
			TaskID: task.ID, URL: pushCfg.URL, Token: pushCfg.Token,
		}); err != nil {
			h.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("a2a: failed to persist pushNotificationConfig")
		}
	}
	base := ""
	if h.BaseURLProvider != nil {
		base = strings.TrimRight(h.BaseURLProvider.PublicBaseURL(), "/")
	}
	streamPath := agentEndpointPath(agent.ProjectID, agent.WorkflowID) + "/tasks/" + task.ID
	resp := taskSubmitResponse{
		TaskID:    task.ID,
		Status:    "submitted",
		StreamURL: base + streamPath,
	}
	if base == "" {
		resp.StreamURL = streamPath
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// extractTextPrompt concatenates every `text` part's content with
// blank-line separators. Non-text parts are silently dropped —
// the caller already saw the "only text" 422 if everything was
// non-text.
func extractTextPrompt(parts []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" || strings.TrimSpace(p.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

// --- shared helpers -------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		// We've already written headers; best we can do is log via
		// the canonical "operator never sees a 500 but the bytes are
		// truncated" path. Logger may be nil during tests.
		_ = err
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// ErrNotPublished is returned by registry helpers when callers
// reference a project/workflow pair that isn't opted into A2A.
// Exported so test code can assert on the sentinel.
var ErrNotPublished = errors.New("a2a: agent not published")
