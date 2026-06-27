package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/persistence"
)

// enableRequestDTO is the body for POST /api/v1/doctor/features/{id}/enable.
type enableRequestDTO struct {
	Apply bool `json:"apply"`
}

// enablePlanDTO is the wire shape for a dry-run plan response.
type enablePlanDTO struct {
	FeatureID string          `json:"feature_id"`
	Changes   []gateChangeDTO `json:"changes"`
	Apply     string          `json:"apply"`
}

// gateChangeDTO represents one gate key transition in the plan.
type gateChangeDTO struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// enableResultDTO is the wire shape for the apply=true response.
type enableResultDTO struct {
	FeatureID string `json:"feature_id"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
}

// EnableFeature handles POST /api/v1/doctor/features/{id}/enable.
//
// Body: {"apply": bool}
//   - apply=false (dry-run): runs PlanEnable and returns the plan as JSON; no
//     config mutation occurs.
//   - apply=true: runs PlanEnable then ApplyEnable with the real ConfigWriter
//     (over the daemon's config.yaml path) and the real Reloader (the daemon's
//     ConfigReloader). Returns the verify result.
//
// Security: this endpoint MUTATES the daemon config and triggers a reload.
// It is admin-gated via requireAdminGate — the same gate that guards every
// /api/v1/admin/* route (audit, workflow-architect, CPC, etc.). A regular
// project API key is insufficient; the caller must supply an admin key (or
// hold a browser session with role=admin). When auth is disabled every caller
// is implicitly trusted, matching the daemon's other admin endpoints.
func (h *DoctorHandlers) EnableFeature(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}

	// Admin gate (fail-CLOSED) — config mutation is an admin-only action.
	// Three cases:
	//   1. server is wired (production): run the gate normally; return on failure.
	//   2. server is nil AND enableApplierFunc is nil (not a unit-test handler):
	//      reject with 503 — proceeding unauthenticated would be a security hole.
	//   3. server is nil AND enableApplierFunc is non-nil (unit-test injection):
	//      gate intentionally bypassed so tests can run without a wired Server.
	if h.server != nil {
		if !h.server.requireAdminGate(w, r) {
			return
		}
	} else if h.enableApplierFunc == nil {
		respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"server not wired; cannot authenticate")
		return
	}
	// else: test path — enableApplierFunc is set; gate bypassed for unit tests.

	// Extract the feature id from the path:
	// /api/v1/doctor/features/{id}/enable
	id := extractFeatureEnableID(r.URL.Path)
	if id == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "feature id is required")
		return
	}

	var found *featuredoctor.Feature
	for _, f := range featuredoctor.Registry() {
		f := f
		if f.ID == id {
			found = &f
			break
		}
	}
	if found == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "feature not found: "+id)
		return
	}

	// Parse body.
	var req enableRequestDTO
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &req); err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid request body: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	deps := h.buildFeatureDeps()

	plan, err := featuredoctor.PlanEnable(ctx, *found, deps)
	if err != nil {
		respondError(w, http.StatusConflict, "PREREQ_UNMET", err.Error())
		return
	}

	if !req.Apply {
		// Dry-run: return the plan.
		respondJSON(w, http.StatusOK, enablePlanToDTO(found.ID, plan))
		return
	}

	// Apply path.
	var result featuredoctor.PrereqResult
	if h.enableApplierFunc != nil {
		// Test injection: bypass real writer/reloader.
		result, err = h.enableApplierFunc(ctx, *found, deps, plan, nil, nil)
	} else {
		// Production path — need real writer and reloader.
		if h.configPath == "" {
			respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
				"config path not wired; cannot write gate changes")
			return
		}
		if h.configReloader == nil {
			respondError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
				"config reloader not wired; cannot reload after gate changes")
			return
		}

		writer := &featuredoctor.FileConfigWriter{Path: h.configPath}
		reloader := &configReloaderAdapter{r: h.configReloader}
		result, err = featuredoctor.ApplyEnable(ctx, *found, deps, plan, writer, reloader)
	}
	if err != nil {
		// Audit the failed apply too — a mutating admin POST that errored
		// (and rolled back) is still operator-relevant forensics.
		h.auditFeatureEnable(ctx, r, found.ID, plan, false, "apply failed: "+err.Error())
		respondError(w, http.StatusInternalServerError, "APPLY_FAILED", err.Error())
		return
	}

	h.auditFeatureEnable(ctx, r, found.ID, plan, result.OK, result.Detail)
	respondJSON(w, http.StatusOK, enableResultDTO{
		FeatureID: found.ID,
		OK:        result.OK,
		Detail:    result.Detail,
	})
}

// auditFeatureEnable writes one admin-audit row for a feature-enable apply.
// Per the AdminAuditRepository contract every admin POST that mutates state
// records a durable row before returning. Best-effort + nil-safe: an audit
// sink failure must not fail the (already-applied) operation, and the
// test-injection path (server unwired) simply records nothing.
func (h *DoctorHandlers) auditFeatureEnable(ctx context.Context, r *http.Request, featureID string, plan *featuredoctor.EnablePlan, ok bool, detail string) {
	if h.server == nil || h.server.adminAuditRepo == nil {
		return
	}
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	after, _ := json.Marshal(map[string]any{
		"feature_id": featureID,
		"ok":         ok,
		"detail":     detail,
		"apply":      applyMechanismString(plan.Apply),
		"changes":    len(plan.Changes),
	})
	_ = h.server.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "api",
		Action:    "feature.enable",
		Target:    featureID,
		After:     string(after),
		IP:        clientIPFromRequest(r),
		UserAgent: r.UserAgent(),
	})
}

// configReloaderAdapter adapts *config.ConfigReloader to featuredoctor.Reloader.
// ConfigReloader.Reload() takes no context; the adapter ignores ctx (the
// underlying reload is synchronous and bounded internally).
type configReloaderAdapter struct {
	r interface{ Reload() error }
}

func (a *configReloaderAdapter) Reload(_ context.Context) error {
	return a.r.Reload()
}

// enablePlanToDTO converts an *EnablePlan to its wire DTO.
func enablePlanToDTO(featureID string, plan *featuredoctor.EnablePlan) enablePlanDTO {
	dto := enablePlanDTO{
		FeatureID: featureID,
		Apply:     applyMechanismString(plan.Apply),
	}
	for _, ch := range plan.Changes {
		dto.Changes = append(dto.Changes, gateChangeDTO{
			Key:  ch.Key,
			From: ch.From,
			To:   ch.To,
		})
	}
	return dto
}

// applyMechanismString returns the human-readable name for an ApplyMechanism.
func applyMechanismString(m featuredoctor.ApplyMechanism) string {
	switch m {
	case featuredoctor.ReloadHot:
		return "reload-hot"
	case featuredoctor.RestartRequired:
		return "restart-required"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// extractFeatureEnableID extracts {id} from
// /api/v1/doctor/features/{id}/enable.
func extractFeatureEnableID(path string) string {
	const prefix = "/api/v1/doctor/features/"
	const suffix = "/enable"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	mid := strings.TrimPrefix(path, prefix)
	mid = strings.TrimSuffix(mid, "/") // normalize trailing slash on /enable/
	if !strings.HasSuffix(mid, suffix) {
		return ""
	}
	id := strings.TrimSuffix(mid, suffix)
	if strings.Contains(id, "/") {
		return ""
	}
	return id
}
