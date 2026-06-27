package api

// Instinct REST surface — continuous-learning instinct layer (LLD:
// continuous-learning-instinct-layer-design.md). Read / inspect /
// retire only; nothing here mutates agent behaviour.
//
//   GET  /api/v1/instincts                     — list + filter
//   GET  /api/v1/instincts/{id}                — show one
//   POST /api/v1/instincts/{id}/retire         — operator retire
//   POST /api/v1/admin/instincts/recompute     — admin: recompute confidence
//
// The list / show / retire endpoints go through the normal auth chain
// (reading advisory evidence is low-privilege). The recompute endpoint
// is admin-gated because it re-derives confidence over a matched set.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstinctJSON is the on-the-wire shape for one instinct row. Mirrors
// persistence.Instinct but with RFC3339-formatted timestamps so the CLI
// / UI renderers don't round-trip a Go time.Time. Trigger is carried as
// the raw trigger_json bytes (the LLD frontmatter `trigger` field maps
// to the trigger_json column at this boundary).
type InstinctJSON struct {
	ID              string  `json:"id"`
	Scope           string  `json:"scope"`
	ProjectID       string  `json:"project_id,omitempty"`
	Domain          string  `json:"domain"`
	TriggerKey      string  `json:"trigger_key"`
	Trigger         string  `json:"trigger,omitempty"` // raw JSON object string
	Action          string  `json:"action"`
	Confidence      float64 `json:"confidence"`
	SupportCount    int     `json:"support_count"`
	ContradictCount int     `json:"contradict_count"`
	Source          string  `json:"source"`
	Status          string  `json:"status"`
	DistillModel    string  `json:"distill_model,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	LastSeenAt      string  `json:"last_seen_at"`
}

// InstinctListResponse is the wire shape for GET /api/v1/instincts.
type InstinctListResponse struct {
	Instincts []InstinctJSON `json:"instincts"`
}

// InstinctShowResponse is the single-row body for GET /instincts/{id}.
type InstinctShowResponse struct {
	Instinct InstinctJSON `json:"instinct"`
}

// InstinctRecomputeResponse reports how many instincts were recomputed.
type InstinctRecomputeResponse struct {
	Recomputed int `json:"recomputed"`
}

// ListInstincts handles GET /api/v1/instincts. Filters: domain, scope,
// project, status, min_confidence, limit. All optional; empty means no
// constraint. Highest confidence first (repository ordering). Reading
// instincts is a low-privilege operation — no admin scope required.
func (s *Server) ListInstincts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.instinctRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "INSTINCTS_DISABLED",
			"instinct repository not wired on this deployment")
		return
	}
	q := r.URL.Query()
	filter := persistence.InstinctFilter{PageSize: 100}
	if v := strings.TrimSpace(q.Get("domain")); v != "" {
		filter.Domain = &v
	}
	if v := strings.TrimSpace(q.Get("scope")); v != "" {
		filter.Scope = &v
	}
	if v := strings.TrimSpace(q.Get("project")); v != "" {
		filter.ProjectID = &v
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		filter.Status = &v
	}
	if v := strings.TrimSpace(q.Get("min_confidence")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST",
				"min_confidence must be a number in [0,1]")
			return
		}
		filter.MinConfidence = &f
	}
	if v := q.Get("limit"); v != "" {
		if n, err := parseLimit(v, 1, 1000); err == nil {
			filter.PageSize = n
		}
	}

	// Project-scope enforcement. A scoped (per-project) API key must not
	// read another project's project-scoped instincts. Mirrors the
	// canonical idiom in ListProjects (registry_handlers.go). When auth
	// is off (single-tenant), scoped is false and nothing changes.
	allowed, scoped := requestScopedProjectSet(r)
	if scoped && filter.ProjectID != nil && !allowed[*filter.ProjectID] {
		respondError(w, http.StatusForbidden, "FORBIDDEN",
			"project not in caller scope")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.instinctRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "instinct list failed: "+err.Error())
		return
	}
	out := InstinctListResponse{Instincts: make([]InstinctJSON, 0, len(rows))}
	for _, row := range rows {
		if row == nil {
			continue
		}
		// Drop project-scoped rows owned by a project outside the
		// caller's scope. Global-scope rows are cross-project advisory
		// evidence and stay visible to every authenticated caller.
		if scoped && row.Scope == persistence.InstinctScopeProject && !allowed[row.ProjectID] {
			continue
		}
		out.Instincts = append(out.Instincts, instinctToJSON(row))
	}
	respondJSON(w, http.StatusOK, out)
}

// instinctsRouter dispatches /api/v1/instincts/{id} and the /retire
// sub-path. HandleFunc only does prefix matching; this peels the {id}
// segment and the optional suffix off the path.
func (s *Server) instinctsRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/instincts/")
	if rest == "" {
		s.ListInstincts(w, r)
		return
	}
	id, suffix := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id = rest[:i]
		suffix = rest[i+1:]
	}
	switch suffix {
	case "":
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
			return
		}
		s.ShowInstinct(w, r, id)
	case "retire":
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				"retire endpoint accepts POST only")
			return
		}
		s.RetireInstinct(w, r, id)
	default:
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown instinct path")
	}
}

// ShowInstinct handles GET /api/v1/instincts/{id}.
func (s *Server) ShowInstinct(w http.ResponseWriter, r *http.Request, id string) {
	if s.instinctRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "INSTINCTS_DISABLED",
			"instinct repository not wired on this deployment")
		return
	}
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "instinct id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := s.instinctRepo.Get(ctx, id)
	if err != nil {
		if err == persistence.ErrNotFound {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "instinct not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "lookup failed: "+err.Error())
		return
	}
	// A scoped caller may only read a project-scoped instinct belonging to
	// a project in its scope. 404 (not 403) so the endpoint doesn't
	// confirm the existence of out-of-scope rows. Global-scope rows are
	// cross-project advisory evidence and remain readable.
	if row.Scope == persistence.InstinctScopeProject && !requestAllowsProject(r, row.ProjectID) {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "instinct not found")
		return
	}
	respondJSON(w, http.StatusOK, InstinctShowResponse{Instinct: instinctToJSON(row)})
}

// RetireInstinct handles POST /api/v1/instincts/{id}/retire. Flips the
// instinct to status='retired' — advisory only (the row stays for
// audit; nothing about agent behaviour changes). Idempotent: retiring
// an already-retired row is a no-op success.
func (s *Server) RetireInstinct(w http.ResponseWriter, r *http.Request, id string) {
	if s.instinctRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "INSTINCTS_DISABLED",
			"instinct repository not wired on this deployment")
		return
	}
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "instinct id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Fetch before mutating so a scoped caller can't retire another
	// project's project-scoped instinct. 404 on out-of-scope rows so the
	// endpoint doesn't confirm their existence. Global-scope rows are
	// cross-project advisory and retirable by any authenticated caller.
	existing, err := s.instinctRepo.Get(ctx, id)
	if err != nil {
		if err == persistence.ErrNotFound {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "instinct not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "lookup failed: "+err.Error())
		return
	}
	if existing.Scope == persistence.InstinctScopeProject && !requestAllowsProject(r, existing.ProjectID) {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "instinct not found")
		return
	}
	if err := s.instinctRepo.Retire(ctx, id); err != nil {
		if err == persistence.ErrNotFound {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "instinct not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "retire failed: "+err.Error())
		return
	}
	// Best-effort audit row so retirements are traceable, mirroring the
	// operator surfaces. source="api" distinguishes a CLI/REST retire
	// from a confidence-model auto-retire.
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Source: "api",
			Action: "instinct.retired",
			Target: id,
		})
	}
	respondJSON(w, http.StatusOK, map[string]string{"id": id, "status": persistence.InstinctStatusRetired})
}

// AdminInstinctsRecompute handles POST /api/v1/admin/instincts/recompute.
// Admin-gated. Recomputes the materialised confidence for every instinct
// matching the (optional) filter, using the injected scorer. Read-mostly:
// it only rewrites support/contradict/confidence/status columns from the
// existing evidence rows — it never touches the audit spine and never
// changes behaviour.
func (s *Server) AdminInstinctsRecompute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	// Admin gate: 404 when disabled, 401 without a key, 403 without
	// admin scope (bypassed on auth-disabled single-operator
	// deployments). Same matrix the operator/cpc admin surfaces use.
	if !s.requireAdminGate(w, r) {
		return
	}
	principal := apiKeyPrincipalFromContext(r.Context())
	if principal == "" {
		principal = "anonymous-admin"
	}
	if s.instinctRepo == nil || s.instinctScorer == nil {
		respondError(w, http.StatusServiceUnavailable, "INSTINCTS_DISABLED",
			"instinct repository or scorer not wired on this deployment")
		return
	}

	q := r.URL.Query()
	filter := persistence.InstinctFilter{PageSize: 1000}
	if v := strings.TrimSpace(q.Get("domain")); v != "" {
		filter.Domain = &v
	}
	if v := strings.TrimSpace(q.Get("scope")); v != "" {
		filter.Scope = &v
	}
	if v := strings.TrimSpace(q.Get("project")); v != "" {
		filter.ProjectID = &v
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		filter.Status = &v
	}
	if v := q.Get("limit"); v != "" {
		if n, err := parseLimit(v, 1, 5000); err == nil {
			filter.PageSize = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, err := s.instinctRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "instinct list failed: "+err.Error())
		return
	}
	recomputed := 0
	for _, row := range rows {
		if row == nil {
			continue
		}
		if err := s.instinctRepo.RecomputeConfidence(ctx, row.ID, s.instinctScorer); err != nil {
			respondError(w, http.StatusInternalServerError, "INTERNAL",
				"recompute failed for "+row.ID+": "+err.Error())
			return
		}
		recomputed++
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "api",
			Action:    "instinct.recomputed",
			After:     `{"recomputed":` + strconv.Itoa(recomputed) + `}`,
		})
	}
	respondJSON(w, http.StatusOK, InstinctRecomputeResponse{Recomputed: recomputed})
}

// instinctToJSON converts a persistence row to the wire shape.
func instinctToJSON(row *persistence.Instinct) InstinctJSON {
	out := InstinctJSON{
		ID:              row.ID,
		Scope:           row.Scope,
		ProjectID:       row.ProjectID,
		Domain:          row.Domain,
		TriggerKey:      row.TriggerKey,
		Action:          row.Action,
		Confidence:      row.Confidence,
		SupportCount:    row.SupportCount,
		ContradictCount: row.ContradictCount,
		Source:          row.Source,
		Status:          row.Status,
		DistillModel:    row.DistillModel,
		CreatedAt:       row.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       row.UpdatedAt.UTC().Format(time.RFC3339Nano),
		LastSeenAt:      row.LastSeenAt.UTC().Format(time.RFC3339Nano),
	}
	if len(row.Trigger) > 0 {
		out.Trigger = string(row.Trigger)
	}
	return out
}
