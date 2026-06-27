package a2a

// pushNotificationConfig set/get endpoints — the A2A spec's
// tasks/pushNotificationConfig surface, REST-shaped to match vornik's A2A
// surface. Lets a caller register / inspect a task's webhook AFTER
// submission (the submit body also accepts it inline).
//
//   POST /a2a/v1/agents/<p>/<wf>/tasks/<id>/pushNotificationConfig  {url, token?}
//   GET  /a2a/v1/agents/<p>/<wf>/tasks/<id>/pushNotificationConfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"vornik.io/vornik/internal/persistence"
)

type pushConfigBody struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// pushConfigResponse intentionally omits the token: it's a caller-supplied
// secret, and echoing it back on GET would turn the endpoint into a token
// oracle. The caller already knows the token it set; we only confirm the
// target url + that a config exists.
type pushConfigResponse struct {
	TaskID     string `json:"taskId"`
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

// handlePushConfig dispatches the set (POST) / get (GET) of a task's webhook
// config. Both verify the task belongs to the agent's project (404 otherwise,
// without leaking out-of-scope existence — mirrors the SSE handler).
func (h *Handler) handlePushConfig(w http.ResponseWriter, r *http.Request, agent *PublishedAgent, taskID string) {
	if h.PushConfigStore == nil {
		writeError(w, http.StatusServiceUnavailable, "PUSH_UNSUPPORTED", "pushNotificationConfig is not configured on this daemon")
		return
	}
	if !h.taskInScope(r.Context(), agent, taskID) {
		// 503 when the lookup surface isn't wired; otherwise 404 (the
		// helper can't distinguish, so it returns false either way — treat
		// as not-found, the safe default that doesn't leak existence).
		if streamDeps == nil || streamDeps.Tasks == nil {
			writeError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED", "task lookup not configured on this daemon")
			return
		}
		writeError(w, http.StatusNotFound, "NOT_FOUND", "task "+taskID+" not found")
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.handlePushConfigSet(w, r, taskID)
	case http.MethodGet:
		h.handlePushConfigGet(w, r, taskID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST to set or GET to read")
	}
}

func (h *Handler) handlePushConfigSet(w http.ResponseWriter, r *http.Request, taskID string) {
	var body pushConfigBody
	rc := http.MaxBytesReader(w, r.Body, 1<<16)
	defer func() { _ = rc.Close() }()
	dec := json.NewDecoder(rc)
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body")
		return
	}
	if err := ValidateWebhookURL(body.URL); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
		return
	}
	if err := h.PushConfigStore.Set(r.Context(), persistence.A2APushConfig{
		TaskID: taskID, URL: body.URL, Token: body.Token,
	}); err != nil {
		h.Logger.Warn().Err(err).Str("task_id", taskID).Msg("a2a: pushNotificationConfig set failed")
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to persist push config")
		return
	}
	writeJSON(w, http.StatusOK, pushConfigResponse{TaskID: taskID, URL: body.URL, Configured: true})
}

func (h *Handler) handlePushConfigGet(w http.ResponseWriter, r *http.Request, taskID string) {
	cfg, err := h.PushConfigStore.Get(r.Context(), taskID)
	if err != nil || cfg == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no pushNotificationConfig set for task "+taskID)
		return
	}
	writeJSON(w, http.StatusOK, pushConfigResponse{TaskID: taskID, URL: cfg.URL, Configured: true})
}

// taskInScope reports whether taskID exists AND belongs to the agent's
// project. Reuses the SSE bridge's task lookup (streamDeps). Returns false
// (caller maps to 404/503) when the lookup isn't wired or the task is
// out-of-scope — never leaks the existence of another project's task.
func (h *Handler) taskInScope(ctx context.Context, agent *PublishedAgent, taskID string) bool {
	if streamDeps == nil || streamDeps.Tasks == nil {
		return false
	}
	task, err := streamDeps.Tasks.Get(ctx, taskID)
	if err != nil || task == nil {
		return false
	}
	return task.ProjectID == agent.ProjectID
}
