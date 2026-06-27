package api

// CPC admin endpoints. Operator-facing surface for the
// inter-project orchestration ledger (LLD §11 Phase D
// follow-on). Three actions:
//
//   GET  /api/v1/admin/cpc            — list rows (filter by
//                                       status / project / since)
//   GET  /api/v1/admin/cpc/{id}       — show one row
//   POST /api/v1/admin/cpc/{id}/cancel — force-resolve a stuck row
//
// All three sit behind the existing admin auth gate (admin.enabled
// + IsAdminKey). The cancel action writes an audit row with
// action="interproject.cpc.admincancel" + the operator's
// API key as principal so the trail distinguishes
// operator-initiated cancels from the executor's automatic
// resolve / timeout-scanner sweeps.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

// CPCEntryJSON is the wire shape for a single cross_project_calls
// row. RFC3339 timestamps + omitempty optional fields so the
// CLI table renderer doesn't need to handle Go time.Time.
type CPCEntryJSON struct {
	ID             string `json:"id"`
	CallerTaskID   string `json:"caller_task_id"`
	CallerStepID   string `json:"caller_step_id"`
	CallerProject  string `json:"caller_project"`
	CalleeProject  string `json:"callee_project"`
	CalleeWorkflow string `json:"callee_workflow"`
	CalleeTaskID   string `json:"callee_task_id,omitempty"`
	ExpectedSchema string `json:"expected_schema,omitempty"`
	Status         string `json:"status"`
	ErrorMessage   string `json:"error_message,omitempty"`
	TimeoutAt      string `json:"timeout_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	ResolvedAt     string `json:"resolved_at,omitempty"`
}

// CPCListResponse is the wire shape for GET /api/v1/admin/cpc.
type CPCListResponse struct {
	Entries []CPCEntryJSON `json:"entries"`
}

// CPCCancelRequest is the body shape for the cancel POST.
type CPCCancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// CPCCancelResponse is the wire shape returned on success — the
// updated row so the CLI can render the new status.
type CPCCancelResponse struct {
	Entry CPCEntryJSON `json:"entry"`
}

// AdminCPCList handles GET /api/v1/admin/cpc. Optional query
// params: status, caller, callee, since (RFC3339 or YYYY-MM-DD),
// limit (1-1000, default 200).
func (s *Server) AdminCPCList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.cpcRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "CPC_DISABLED",
			"cross_project_calls repository not wired on this deployment")
		return
	}
	q := r.URL.Query()
	filter := persistence.CPCListFilter{
		Status:        persistence.CrossProjectCallStatus(q.Get("status")),
		CallerProject: q.Get("caller"),
		CalleeProject: q.Get("callee"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := parseLimit(v, 1, 1000); err == nil {
			filter.PageSize = n
		}
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.CreatedSince = t
		} else if t, err := time.Parse("2006-01-02", since); err == nil {
			filter.CreatedSince = t
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.cpcRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "cpc list failed: "+err.Error())
		return
	}
	out := CPCListResponse{Entries: make([]CPCEntryJSON, 0, len(rows))}
	for _, e := range rows {
		out.Entries = append(out.Entries, cpcRowToJSON(e))
	}
	respondJSON(w, http.StatusOK, out)
}

// AdminCPCShow handles GET /api/v1/admin/cpc/{id}. 404 when the
// id doesn't match a row.
func (s *Server) AdminCPCShow(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.cpcRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "CPC_DISABLED",
			"cross_project_calls repository not wired on this deployment")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := s.cpcRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "cpc not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "cpc fetch failed")
		return
	}
	respondJSON(w, http.StatusOK, CPCCancelResponse{Entry: cpcRowToJSON(row)})
}

// AdminCPCCancel handles POST /api/v1/admin/cpc/{id}/cancel.
// Body: {"reason": "..."}. Force-resolves a pending/running
// row as rejected so the caller's on_fail branch fires.
// Idempotent — re-issuing on an already-terminal row returns
// the existing row with no state change.
func (s *Server) AdminCPCCancel(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.cpcRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "CPC_DISABLED",
			"cross_project_calls repository not wired on this deployment")
		return
	}

	// Body is optional — empty body means "use the default
	// cancel reason". Cap at 4 KiB so a 1 GB body doesn't OOM
	// the handler.
	var body CPCCancelRequest
	if r.Body != nil {
		if err := decodeJSONBody(w, r, 4*1024, &body); err != nil && !errors.Is(err, io.EOF) {
			respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid body: "+err.Error())
			return
		}
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "operator-cancelled via admin API"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Confirm the row exists first — gives a clean 404 instead
	// of a silent no-op for typos.
	row, err := s.cpcRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "cpc not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "cpc fetch failed")
		return
	}
	if err := s.cpcRepo.AdminCancel(ctx, id, reason); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "cpc cancel failed: "+err.Error())
		return
	}

	// Audit row. Action distinguishes operator cancel from
	// executor auto-resolve so operators can filter the audit
	// log to "operator interventions only".
	if s.adminAuditRepo != nil {
		after, _ := json.Marshal(map[string]any{
			"cpc_id":         id,
			"caller_project": row.CallerProject,
			"callee_project": row.CalleeProject,
			"reason":         reason,
			"prior_status":   string(row.Status),
		})
		principal := apiKeyPrincipalFromContext(r.Context())
		if principal == "" {
			principal = "anonymous-admin"
		}
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "api",
			Action:    "interproject.cpc.admincancel",
			Target:    row.CalleeProject,
			After:     string(after),
			IP:        clientIPFromRequest(r),
			UserAgent: r.UserAgent(),
		})
	}

	// Re-fetch so the response reflects the updated status.
	updated, err := s.cpcRepo.Get(ctx, id)
	if err != nil || updated == nil {
		// The cancel succeeded; re-fetch failing is a weird
		// race. Return the pre-cancel row with a synthesised
		// rejected status so the CLI still shows the operator
		// what happened.
		row.Status = persistence.CPCStatusRejected
		row.ErrorMessage = &reason
		respondJSON(w, http.StatusOK, CPCCancelResponse{Entry: cpcRowToJSON(row)})
		return
	}
	respondJSON(w, http.StatusOK, CPCCancelResponse{Entry: cpcRowToJSON(updated)})
}

// requireAdminGate centralises the "is this an admin caller"
// check shared by all admin-scoped handlers (CPC, operator
// list/show/set/forget, future admin endpoints). Returns true
// when the caller is admitted; emits the appropriate response
// code and returns false when not.
//
// Auth-disabled override (2026-05-25): when the deployment is
// running with config.API.AuthEnabled=false, the concept of
// "admin key vs regular key" doesn't apply — every caller is
// implicitly trusted (the same reasoning the admin.Middleware
// uses for its non-gated path bypass; see
// https://docs.vornik.io §10). Without this
// override, `vornikctl operator list` + every admin REST
// endpoint returned 404 / 403 for every caller on auth-disabled
// deployments, breaking the single-operator path that the rest
// of the daemon explicitly supports.
func (s *Server) requireAdminGate(w http.ResponseWriter, r *http.Request) bool {
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	if !s.adminConfig.Enabled {
		http.NotFound(w, r)
		return false
	}
	// Session-admin path (github-login phase 3): a browser session
	// whose principal resolved to role=admin passes the gate without
	// an api-key admin allowlist entry. Resolves the slice-2 "swap the
	// key-string check for an identity role" intent. The key allowlist
	// below keeps working unchanged for programmatic admin callers.
	if SessionRoleFromContext(r.Context()) == "admin" {
		return true
	}
	key := APIKeyFromContext(r.Context())
	if key == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", "admin authentication required")
		return false
	}
	if !s.adminConfig.IsAdminKey(key) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return false
	}
	return true
}

// cpcRowToJSON converts a persistence.CrossProjectCall to its
// wire shape. Optional fields are omitted when zero-value so
// the CLI's table renderer doesn't show empty columns.
func cpcRowToJSON(c *persistence.CrossProjectCall) CPCEntryJSON {
	if c == nil {
		return CPCEntryJSON{}
	}
	out := CPCEntryJSON{
		ID:             c.ID,
		CallerTaskID:   c.CallerTaskID,
		CallerStepID:   c.CallerStepID,
		CallerProject:  c.CallerProject,
		CalleeProject:  c.CalleeProject,
		CalleeWorkflow: c.CalleeWorkflow,
		ExpectedSchema: c.ExpectedSchema,
		Status:         string(c.Status),
		CreatedAt:      c.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if c.CalleeTaskID != nil {
		out.CalleeTaskID = *c.CalleeTaskID
	}
	if c.ErrorMessage != nil {
		out.ErrorMessage = *c.ErrorMessage
	}
	if c.TimeoutAt != nil {
		out.TimeoutAt = c.TimeoutAt.UTC().Format(time.RFC3339Nano)
	}
	if c.ResolvedAt != nil {
		out.ResolvedAt = c.ResolvedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// parseLimit is a tiny helper used by the list endpoint to
// validate the limit query param. Kept private because the
// audit handler has its own copy with different bounds; the
// shape is the same but the bounds + default differ.
func parseLimit(s string, min, max int) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("non-numeric limit")
		}
		n = n*10 + int(c-'0')
		if n > max {
			n = max
		}
	}
	if n < min {
		return 0, errors.New("limit out of range")
	}
	return n, nil
}

// adminCPCRouter dispatches the /api/v1/admin/cpc/{id} and
// /api/v1/admin/cpc/{id}/cancel paths. HandleFunc only does
// prefix matching; this router peels off the {id} segment +
// optional action.
func (s *Server) adminCPCRouter(w http.ResponseWriter, r *http.Request) {
	remaining := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/cpc/")
	remaining = strings.TrimSuffix(remaining, "/")
	if remaining == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "cpc id required")
		return
	}
	if strings.HasSuffix(remaining, "/cancel") {
		id := strings.TrimSuffix(remaining, "/cancel")
		if r.Method != http.MethodPost {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
			return
		}
		s.AdminCPCCancel(w, r, id)
		return
	}
	if strings.Contains(remaining, "/") {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown cpc subresource")
		return
	}
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET required")
		return
	}
	s.AdminCPCShow(w, r, remaining)
}

// clientIPFromRequest extracts the caller IP for the audit row. It reads
// the centrally-resolved, trusted-proxy-aware client IP that
// realip.Middleware stored in the request context, falling back to
// RemoteAddr's host when unset. It NEVER reads X-Forwarded-For directly —
// behind the Cloudflare tunnel the leftmost hop is attacker-controlled, so
// audit rows would otherwise record a forgeable IP.
// see LLD § https://docs.vornik.io
func clientIPFromRequest(r *http.Request) string {
	if ip := realip.ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return realip.RemoteHost(r)
}
