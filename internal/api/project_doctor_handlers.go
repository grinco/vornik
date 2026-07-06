package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ProjectDoctor handles the per-project readiness endpoints under
// /api/v1/projects/{id}/doctor. Admin-gated like other project
// mutation surfaces. Dispatch is suffix-based (the mux registers the
// prefix). See https://docs.vornik.io
// Phase 2.
func (s *Server) ProjectDoctor(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.projectDoctor == nil {
		respondError(w, http.StatusServiceUnavailable, "DOCTOR_NOT_CONFIGURED", "project doctor not wired")
		return
	}
	// Path: /api/v1/projects/{id}/doctor[/checks/{key}/run | /secrets]
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		respondError(w, http.StatusBadRequest, "BAD_PATH", "missing project id")
		return
	}
	projectID := rest[:slash]
	suffix := rest[slash+1:] // "doctor" | "doctor/checks/{key}/run" | "doctor/secrets"

	switch {
	case r.Method == http.MethodGet && suffix == "doctor":
		respondJSON(w, http.StatusOK, s.projectDoctor.Run(r.Context(), projectID))

	case r.Method == http.MethodPost && strings.HasPrefix(suffix, "doctor/checks/") && strings.HasSuffix(suffix, "/run"):
		key := strings.TrimSuffix(strings.TrimPrefix(suffix, "doctor/checks/"), "/run")
		if key == "smoke" {
			if _, err := s.projectDoctor.TriggerSmoke(r.Context(), projectID); err != nil {
				respondError(w, http.StatusBadRequest, "SMOKE_FAILED", err.Error())
				return
			}
		}
		res, err := s.projectDoctor.RunOne(r.Context(), projectID, key)
		if err != nil {
			respondError(w, http.StatusBadRequest, "CHECK_FAILED", err.Error())
			return
		}
		respondJSON(w, http.StatusOK, res)

	case r.Method == http.MethodPost && suffix == "doctor/secrets":
		body, err := readLimitedBody(w, r, 16*1024)
		if err != nil {
			respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
			return
		}
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if uerr := json.Unmarshal(body, &req); uerr != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", uerr.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || req.Value == "" {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "name and value are required")
			return
		}
		if err := s.projectDoctor.SetSecret(projectID, req.Name, req.Value); err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		res, err := s.projectDoctor.RunOne(r.Context(), projectID, "secrets")
		if err != nil {
			respondError(w, http.StatusBadRequest, "CHECK_FAILED", err.Error())
			return
		}
		respondJSON(w, http.StatusOK, res)

	default:
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "unsupported doctor route/method")
	}
}
