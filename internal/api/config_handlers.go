// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"vornik.io/vornik/internal/config"
)

// ConfigHandlers provides HTTP handlers for configuration management.
// It exposes endpoints for reload status and manual reload triggers.
type ConfigHandlers struct {
	reloader *config.ConfigReloader
}

// NewConfigHandlers creates a new ConfigHandlers instance.
func NewConfigHandlers(reloader *config.ConfigReloader) *ConfigHandlers {
	return &ConfigHandlers{
		reloader: reloader,
	}
}

// ReloadStatusResponse represents the response for GET /api/v1/config/reload-status.
type ReloadStatusResponse struct {
	LastReload  string   `json:"last_reload,omitempty"`
	LastAttempt string   `json:"last_attempt,omitempty"`
	Errors      []string `json:"errors,omitempty"`
	HasErrors   bool     `json:"has_errors"`
	// Warnings surfaces non-fatal conditions from the last reload
	// — most notably, projects stripped from the staged set because
	// their referenced workflows/swarms didn't resolve. Echoes the
	// strings the daemon already logged at WARN so operators can
	// programmatically detect "reload succeeded but my project is
	// missing" without grepping journald.
	Warnings    []string `json:"warnings,omitempty"`
	HasWarnings bool     `json:"has_warnings"`

	PendingActivation bool   `json:"pending_activation"`
	Blocked           bool   `json:"blocked"`
	BlockedReason     string `json:"blocked_reason,omitempty"`
}

// GetReloadStatus handles GET /api/v1/config/reload-status.
// It returns the current status of the config reload system.
func (h *ConfigHandlers) GetReloadStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only GET method is allowed")
		return
	}

	if h.reloader == nil {
		respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Config reloader not initialized")
		return
	}

	status := h.reloader.Status()

	resp := ReloadStatusResponse{
		HasErrors:         status.HasErrors,
		Errors:            status.Errors,
		Warnings:          status.Warnings,
		HasWarnings:       status.HasWarnings,
		PendingActivation: status.PendingActivation,
		Blocked:           status.Blocked,
		BlockedReason:     status.BlockedReason,
	}

	if !status.LastReload.IsZero() {
		resp.LastReload = status.LastReload.Format(time.RFC3339)
	}
	if !status.LastAttempt.IsZero() {
		resp.LastAttempt = status.LastAttempt.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// ReloadRequest represents the request body for POST /api/v1/config/reload.
type ReloadRequest struct {
	// Force triggers a reload even if there are validation errors
	Force bool `json:"force,omitempty"`
}

// ReloadResponse represents the response for POST /api/v1/config/reload.
type ReloadResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	Timestamp string `json:"timestamp"`
	// Warnings echoes any non-fatal conditions from this reload
	// cycle (e.g. projects stripped from the staged set because
	// their workflow/swarm refs didn't resolve). A reload with
	// warnings still has Success=true — strip is recoverable, the
	// active config just doesn't reflect every file on disk.
	// Operators can re-check the offending refs and reload again.
	Warnings []string `json:"warnings,omitempty"`
}

// Reload handles POST /api/v1/config/reload.
// It triggers a manual configuration reload.
func (h *ConfigHandlers) Reload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Only POST method is allowed")
		return
	}

	if h.reloader == nil {
		respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Config reloader not initialized")
		return
	}

	// Parse optional request body. An unparseable body is a client error.
	var req ReloadRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &req); err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body: "+err.Error())
			return
		}
	}

	// Attempt reload
	err := h.reloader.Reload()

	resp := ReloadResponse{
		Timestamp: time.Now().Format(time.RFC3339),
	}

	if err != nil {
		status := h.reloader.Status()
		resp.Success = false
		resp.Message = err.Error()

		// Return appropriate status code based on error type
		if status.Blocked {
			// Config is valid but activation blocked
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted) // 202 - accepted but pending
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				return
			}
			return
		}

		// Validation or load failure
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			return
		}
		return
	}

	resp.Success = true
	resp.Message = "Configuration reloaded successfully"
	// Surface any non-fatal warnings from this cycle's strip pass.
	// A non-empty list means the active config doesn't reflect
	// every file on disk; the operator can read each entry to find
	// out which project/workflow ref didn't resolve.
	if status := h.reloader.Status(); status.HasWarnings {
		resp.Warnings = status.Warnings
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}
