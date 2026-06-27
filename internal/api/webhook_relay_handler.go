package api

import (
	"encoding/json"
	"io"
	"net/http"
)

// relayEnvelope is the JSON body a DMZ webhook node POSTs to the job-tier
// relay ingress after it has verified the provider HMAC signature. mTLS at
// the listener authenticates the sending node; this handler trusts the
// envelope and does NOT re-verify the provider signature (LLD §3.2).
//
// JSON wire contract across the DMZ seam — must stay in sync with
// webhookrelay.envelope (internal/enterprise/clustering/webhookrelay/client.go). Both structs
// must have identical json tags.
type relayEnvelope struct {
	// Version identifies the envelope schema. v1 receivers ignore the field
	// for forward-compat; future versions will validate a supported range.
	Version    string `json:"version"`
	ProjectID  string `json:"project_id"`
	Source     string `json:"source"`
	DeliveryID string `json:"delivery_id"`
	Body       []byte `json:"body"` // raw provider payload (base64 in JSON)
}

// RelayWebhook handles POST /internal/v1/webhook-relay (job tier, mTLS-only).
// The mTLS listener (Task 5) authenticates the calling DMZ node; this handler
// trusts an authenticated relay and does NOT re-verify the provider HMAC
// signature — it runs enqueueVerifiedWebhook directly (LLD §3.2).
func (s *Server) RelayWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
		return
	}
	if s.projectRegistry == nil || s.taskRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "WEBHOOK_NOT_CONFIGURED", "relay ingress is not configured")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes+4096)) // envelope overhead headroom
	if err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid relay body")
		return
	}
	var env relayEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid relay envelope")
		return
	}
	if env.ProjectID == "" || env.Source == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "project and source are required")
		return
	}
	project := s.projectRegistry.GetProject(env.ProjectID)
	if project == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
		return
	}
	source, ok := findWebhookSource(project, env.Source)
	if !ok {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Webhook source not found")
		return
	}
	// Positive signal: the always-on relay-receipt counter + an Info line so a
	// relayed delivery arriving at this worker is visible in the logs (the
	// signal that was missing when tracing a delivery end-to-end). The
	// enqueue/filter outcome is recorded separately in the webhook_events audit.
	s.apiMetrics.RecordWebhookRelayReceived()
	s.logger.Info().
		Str("project_id", env.ProjectID).
		Str("source", env.Source).
		Str("delivery_id", env.DeliveryID).
		Msg("relay webhook received from DMZ node")
	s.enqueueVerifiedWebhook(r.Context(), w, project, source, env.Body, env.DeliveryID)
}
