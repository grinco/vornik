package api

// Workflow-healing override admin endpoints — Phase B operator-
// tuning surface backed by migration 81's workflow_healing_overrides
// table.
//
//   GET  /api/v1/admin/workflow-healing/overrides
//   POST /api/v1/admin/workflow-healing/overrides        (upsert)
//   POST /api/v1/admin/workflow-healing/overrides/delete (delete)
//
// Same admin gate matrix as the trigger endpoints; same nil-safe
// 503 when the repo isn't wired (SQLite deploys).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// HealingOverrideJSON is the wire shape mirroring
// persistence.HealingTriggerOverride. Nullable fields use pointers
// so a missing field stays absent in the JSON (omitempty),
// distinguishing "not set" from "set to zero" — important for
// threshold (zero is meaningful) and muted_until.
type HealingOverrideJSON struct {
	ProjectID         string   `json:"project_id"`
	WorkflowID        string   `json:"workflow_id"`
	TriggerClass      string   `json:"trigger_class"`
	ThresholdOverride *float64 `json:"threshold_override,omitempty"`
	MutedUntil        string   `json:"muted_until,omitempty"`
	Notes             string   `json:"notes,omitempty"`
	CreatedBy         string   `json:"created_by,omitempty"`
	CreatedAt         string   `json:"created_at,omitempty"`
	UpdatedAt         string   `json:"updated_at,omitempty"`
}

// HealingOverrideListResponse wraps a list-call result.
type HealingOverrideListResponse struct {
	Entries []HealingOverrideJSON `json:"entries"`
}

// HealingOverrideUpsertRequest is the POST .../overrides body.
// MutedUntil is an RFC3339 string OR a relative duration like
// "24h" / "30m"; the latter is convenient for `vornikctl ... --mute-hours 24`.
type HealingOverrideUpsertRequest struct {
	ProjectID         string   `json:"project_id"`
	WorkflowID        string   `json:"workflow_id"`
	TriggerClass      string   `json:"trigger_class"`
	ThresholdOverride *float64 `json:"threshold_override,omitempty"`
	MutedUntil        string   `json:"muted_until,omitempty"`
	MuteDuration      string   `json:"mute_duration,omitempty"` // e.g. "24h", "30m"
	ClearMute         bool     `json:"clear_mute,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

// HealingOverrideDeleteRequest is the POST .../overrides/delete body.
type HealingOverrideDeleteRequest struct {
	ProjectID    string `json:"project_id"`
	WorkflowID   string `json:"workflow_id"`
	TriggerClass string `json:"trigger_class"`
}

// WithHealingOverrideRepository wires the override ledger behind
// the admin endpoints. Nil keeps them at 503.
func WithHealingOverrideRepository(repo persistence.HealingTriggerOverrideRepository) ServerOption {
	return func(s *Server) {
		s.healingOverrideRepo = repo
	}
}

// AdminHealingOverridesList handles
// GET /api/v1/admin/workflow-healing/overrides.
// Optional ?limit=<n> (default 200, max 1000).
func (s *Server) AdminHealingOverridesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.healingOverrideRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing override repository not wired on this deployment")
		return
	}
	limit := 200
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := parseLimit(v, 1, 1000); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.healingOverrideRepo.List(ctx, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "list failed: "+err.Error())
		return
	}
	out := HealingOverrideListResponse{Entries: make([]HealingOverrideJSON, 0, len(rows))}
	for _, o := range rows {
		out.Entries = append(out.Entries, healingOverrideToJSON(o))
	}
	respondJSON(w, http.StatusOK, out)
}

// AdminHealingOverrideUpsert handles
// POST /api/v1/admin/workflow-healing/overrides. Body is a
// HealingOverrideUpsertRequest. At least one of threshold_override
// / muted_until / mute_duration / clear_mute must be set, mirroring
// the UI's "nothing to save" guard.
func (s *Server) AdminHealingOverrideUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingOverrideRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing override repository not wired on this deployment")
		return
	}
	var body HealingOverrideUpsertRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.ProjectID = strings.TrimSpace(body.ProjectID)
	body.WorkflowID = strings.TrimSpace(body.WorkflowID)
	body.TriggerClass = strings.TrimSpace(body.TriggerClass)
	if body.ProjectID == "" || body.WorkflowID == "" || body.TriggerClass == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"project_id, workflow_id, trigger_class are all required")
		return
	}
	if !isValidHealingTriggerClass(body.TriggerClass) {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"unknown trigger_class: "+body.TriggerClass)
		return
	}
	// At least one action field must be set, mirroring the UI's
	// "nothing to save" guard.
	if body.ThresholdOverride == nil && body.MutedUntil == "" && body.MuteDuration == "" && !body.ClearMute {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"nothing to save: set threshold_override, muted_until, mute_duration, or clear_mute")
		return
	}
	if body.ThresholdOverride != nil && *body.ThresholdOverride <= 0 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"threshold_override must be > 0 (stored as relative delta, not percent)")
		return
	}
	principal := APIKeyFromContext(r.Context())
	if principal == "" {
		principal = "api"
	}
	o := &persistence.HealingTriggerOverride{
		ProjectID:         body.ProjectID,
		WorkflowID:        body.WorkflowID,
		TriggerClass:      persistence.HealingTriggerClass(body.TriggerClass),
		ThresholdOverride: body.ThresholdOverride,
		Notes:             strings.TrimSpace(body.Notes),
		CreatedBy:         principal,
	}
	if !body.ClearMute {
		// Either an explicit timestamp OR a duration; explicit wins.
		if body.MutedUntil != "" {
			t, perr := time.Parse(time.RFC3339, body.MutedUntil)
			if perr != nil {
				respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"muted_until must be RFC3339: "+perr.Error())
				return
			}
			tu := t.UTC()
			o.MutedUntil = &tu
		} else if body.MuteDuration != "" {
			d, perr := time.ParseDuration(body.MuteDuration)
			if perr != nil || d <= 0 {
				respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
					"mute_duration must be a positive Go duration (e.g. 24h): "+body.MuteDuration)
				return
			}
			until := time.Now().UTC().Add(d)
			o.MutedUntil = &until
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.healingOverrideRepo.Upsert(ctx, o); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "upsert failed: "+err.Error())
		return
	}
	// Read-back for an authoritative response (CreatedAt /
	// UpdatedAt the repo set).
	saved, err := s.healingOverrideRepo.Get(ctx, o.ProjectID, o.WorkflowID, o.TriggerClass)
	if err != nil {
		// Best-effort: echo the request shape.
		respondJSON(w, http.StatusOK, healingOverrideToJSON(o))
		return
	}
	respondJSON(w, http.StatusOK, healingOverrideToJSON(saved))
}

// AdminHealingOverrideDelete handles
// POST /api/v1/admin/workflow-healing/overrides/delete. Idempotent
// at the repo level: a missing row is not an error.
func (s *Server) AdminHealingOverrideDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.healingOverrideRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "BLACKBOX_DISABLED",
			"workflow-healing override repository not wired on this deployment")
		return
	}
	var body HealingOverrideDeleteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.ProjectID = strings.TrimSpace(body.ProjectID)
	body.WorkflowID = strings.TrimSpace(body.WorkflowID)
	body.TriggerClass = strings.TrimSpace(body.TriggerClass)
	if body.ProjectID == "" || body.WorkflowID == "" || body.TriggerClass == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"project_id, workflow_id, trigger_class are all required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.healingOverrideRepo.Delete(ctx, body.ProjectID, body.WorkflowID,
		persistence.HealingTriggerClass(body.TriggerClass)); err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusInternalServerError, "INTERNAL", "delete failed: "+err.Error())
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// adminHealingOverridesRouter dispatches the bare list/upsert at
// /api/v1/admin/workflow-healing/overrides and the /delete suffix.
// Single ServeMux entry covers both verbs because net/http's mux
// dispatches by method internally.
func (s *Server) adminHealingOverridesRouter(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/admin/workflow-healing/overrides/delete" {
		s.AdminHealingOverrideDelete(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.AdminHealingOverridesList(w, r)
	case http.MethodPost:
		s.AdminHealingOverrideUpsert(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or POST")
	}
}

// healingOverrideToJSON formats the persistence row for the wire.
func healingOverrideToJSON(o *persistence.HealingTriggerOverride) HealingOverrideJSON {
	out := HealingOverrideJSON{
		ProjectID:         o.ProjectID,
		WorkflowID:        o.WorkflowID,
		TriggerClass:      string(o.TriggerClass),
		ThresholdOverride: o.ThresholdOverride,
		Notes:             o.Notes,
		CreatedBy:         o.CreatedBy,
	}
	if !o.CreatedAt.IsZero() {
		out.CreatedAt = o.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !o.UpdatedAt.IsZero() {
		out.UpdatedAt = o.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if o.MutedUntil != nil {
		out.MutedUntil = o.MutedUntil.UTC().Format(time.RFC3339)
	}
	return out
}

// isValidHealingTriggerClass mirrors the persistence enum check.
// Kept here rather than in persistence so the api package can fail
// validation at the wire boundary without a round-trip.
func isValidHealingTriggerClass(s string) bool {
	switch persistence.HealingTriggerClass(s) {
	case persistence.HealingTriggerFailureRateSpike,
		persistence.HealingTriggerCostRegression:
		return true
	}
	return false
}
