package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/featuredoctor"
)

// featureStatusDTO is the JSON shape for one feature's diagnosis.
type featureStatusDTO struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Summary string            `json:"summary"`
	Status  string            `json:"status"`
	GatesOn bool              `json:"gates_on"`
	Prereqs []prereqResultDTO `json:"prereqs"`
	Verify  *prereqResultDTO  `json:"verify,omitempty"`
}

// prereqResultDTO is the per-prereq wire shape.
type prereqResultDTO struct {
	Name        string `json:"name"`
	OK          bool   `json:"ok"`
	Fixable     bool   `json:"fixable"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// diagnosisToDTO converts an internal Diagnosis to the wire DTO.
func diagnosisToDTO(d featuredoctor.Diagnosis) featureStatusDTO {
	dto := featureStatusDTO{
		ID:      d.Feature.ID,
		Title:   d.Feature.Title,
		Summary: d.Feature.Summary,
		Status:  string(d.Status),
		GatesOn: d.GatesOn,
	}
	for _, p := range d.Prereqs {
		dto.Prereqs = append(dto.Prereqs, prereqResultDTO{
			Name:        p.Name,
			OK:          p.OK,
			Fixable:     p.Fixable,
			Detail:      p.Detail,
			Remediation: p.Remediation,
		})
	}
	if d.Verify != nil {
		v := prereqResultDTO{
			Name:        "verify",
			OK:          d.Verify.OK,
			Fixable:     d.Verify.Fixable,
			Detail:      d.Verify.Detail,
			Remediation: d.Verify.Remediation,
		}
		dto.Verify = &v
	}
	return dto
}

// ListFeatures handles GET /api/v1/doctor/features.
// It runs Diagnose over all registered features and returns the array.
func (h *DoctorHandlers) ListFeatures(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	deps := h.buildFeatureDeps()
	features := featuredoctor.Registry()
	out := make([]featureStatusDTO, 0, len(features))
	for _, f := range features {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		d := featuredoctor.Diagnose(ctx, f, deps)
		cancel()
		out = append(out, diagnosisToDTO(d))
	}
	respondJSON(w, http.StatusOK, out)
}

// GetFeature handles GET /api/v1/doctor/features/{id}.
func (h *DoctorHandlers) GetFeature(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	// Extract id from path: /api/v1/doctor/features/{id}
	id := extractFeaturedoctorID(r.URL.Path)
	if id == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "feature id is required")
		return
	}

	var found *featuredoctor.Feature
	for _, f := range featuredoctor.Registry() {
		f := f // capture loop variable
		if f.ID == id {
			found = &f
			break
		}
	}
	if found == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "feature not found: "+id)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	deps := h.buildFeatureDeps()
	d := featuredoctor.Diagnose(ctx, *found, deps)
	respondJSON(w, http.StatusOK, diagnosisToDTO(d))
}

// buildFeatureDeps returns the Deps for this request. featureDepsFunc is
// consulted first so tests can inject stubs without touching the Server.
func (h *DoctorHandlers) buildFeatureDeps() featuredoctor.Deps {
	if h.featureDepsFunc != nil {
		return h.featureDepsFunc()
	}
	if h.server != nil {
		return h.server.featureDeps()
	}
	return featuredoctor.Deps{}
}

// extractFeaturedoctorID extracts the feature id from a path like
// /api/v1/doctor/features/{id}.
func extractFeaturedoctorID(path string) string {
	const prefix = "/api/v1/doctor/features/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	id := strings.TrimPrefix(path, prefix)
	return strings.TrimSuffix(id, "/")
}

// routeFeaturePath dispatches /api/v1/doctor/features/{id}[/enable] to the
// appropriate sub-handler. This single trailing-slash handler lets ServeMux's
// longest-prefix-match route all feature sub-paths through one entry point.
func (h *DoctorHandlers) routeFeaturePath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if strings.HasSuffix(path, "/enable") {
		h.EnableFeature(w, r)
		return
	}
	h.GetFeature(w, r)
}
