package api

// Admin endpoint handlers — admin-ui-design.md slice 1. The
// only data-plane admin endpoint in slice 1 is the audit list;
// the rest of the slice's mutating actions live behind the UI
// surface. This endpoint exists so `vornikctl admin audit`
// (the CLI mirror) has something to call.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminAuditEntryJSON is the on-the-wire shape for admin audit
// rows. Mirrors persistence.AdminAuditEntry but with explicit
// JSON tags + RFC3339-formatted Timestamp so the CLI's table
// renderer doesn't have to round-trip a Go time.Time.
type AdminAuditEntryJSON struct {
	ID        string `json:"id"`
	Timestamp string `json:"ts"`
	Principal string `json:"principal"`
	Source    string `json:"source"`
	Action    string `json:"action"`
	Target    string `json:"target,omitempty"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
	IP        string `json:"ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// AdminAuditListResponse is the wire shape for GET /api/v1/admin/audit.
type AdminAuditListResponse struct {
	Entries []AdminAuditEntryJSON `json:"entries"`
}

// AdminAuditList handles GET /api/v1/admin/audit. Returns 404
// when admin.enabled is false (hide the surface from probes);
// 401/403 are not emitted by this handler directly — the surrounding
// AuthMiddleware enforces presence of a valid API key, and this
// handler checks the admin-key allowlist.
func (s *Server) AdminAuditList(w http.ResponseWriter, r *http.Request) {
	// Disabled: 404. NOT 403 — same rationale as the UI admin gate.
	if !s.adminConfig.Enabled {
		http.NotFound(w, r)
		return
	}
	if s.adminAuditRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "ADMIN_AUDIT_DISABLED", "admin audit repository not wired")
		return
	}
	// D4 (audit 2026-06-10): route through requireAdminGate instead of
	// an inline IsAdminKey check so the auth-disabled override applies —
	// the trusted local operator on an auth-OFF deployment (with
	// admin.enabled) is admitted rather than 401'd, and session-admin
	// callers pass too. Same auth-ON matrix as before (no key → 401,
	// non-admin key → 403, admin key → admitted).
	if !s.requireAdminGate(w, r) {
		return
	}

	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	filter := persistence.AdminAuditFilter{
		Action:       q.Get("action"),
		Principal:    q.Get("principal"),
		TargetPrefix: q.Get("target"),
		PageSize:     limit,
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		} else if t, err := time.Parse("2006-01-02", since); err == nil {
			filter.Since = t
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	entries, err := s.adminAuditRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "admin audit query failed")
		return
	}

	out := AdminAuditListResponse{Entries: make([]AdminAuditEntryJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, AdminAuditEntryJSON{
			ID:        e.ID,
			Timestamp: e.Timestamp.UTC().Format(time.RFC3339Nano),
			Principal: e.Principal,
			Source:    e.Source,
			Action:    e.Action,
			Target:    e.Target,
			Before:    e.Before,
			After:     e.After,
			IP:        e.IP,
			UserAgent: e.UserAgent,
		})
	}
	respondJSON(w, http.StatusOK, out)
}
