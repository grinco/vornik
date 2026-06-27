package api

// Admin workflow-stats endpoint — mirrors AdminAuditList's gate
// matrix so terminal-only operators get the same data the architect
// agent will read (memetic-workflows arc, Slice 1). Daemon-wide
// rollup (not project-scoped): one workflow_id maps to N projects;
// the architect's proposed edits affect every project using the
// workflow, so the rollup MUST aggregate across projects.

import (
	"net/http"
	"strconv"
	"time"
)

// AdminWorkflowStats handles GET /api/v1/admin/workflow-stats.
// Query parameters:
//   - workflow=<id>   (required): workflow ID to roll up
//   - since=<RFC3339|<N>d|<N>h>  (optional, default 7d): lower-bound
//     timestamp on executions.created_at
//
// Same gate matrix as AdminAuditList (admin.enabled / admin key
// allowlist). Returns the rollup as JSON; the CLI handles
// human-readable formatting.
func (s *Server) AdminWorkflowStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "only GET is supported")
		return
	}
	if !s.adminConfig.Enabled {
		http.NotFound(w, r)
		return
	}
	if s.workflowTelemetry == nil {
		respondError(w, http.StatusServiceUnavailable, "WORKFLOW_TELEMETRY_DISABLED",
			"workflow-stats endpoint not wired on this deployment")
		return
	}
	// D4 (audit 2026-06-10): route through requireAdminGate so the
	// auth-disabled override admits the trusted local operator instead
	// of 401-ing on the inline IsAdminKey check. Same auth-ON matrix.
	if !s.requireAdminGate(w, r) {
		return
	}

	workflowID := r.URL.Query().Get("workflow")
	if workflowID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "workflow query parameter is required")
		return
	}
	since, err := parseSinceParam(r.URL.Query().Get("since"), 7*24*time.Hour)
	if err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "since: "+err.Error())
		return
	}

	rollup, err := s.workflowTelemetry.ForWorkflow(r.Context(), workflowID, since)
	if err != nil {
		s.logger.Warn().Err(err).
			Str("workflow_id", workflowID).
			Msg("workflow-stats: rollup failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, rollup)
}

// parseSinceParam accepts:
//   - "" → now - defaultLookback
//   - "7d" / "24h" / "30m" → relative duration; rejects negative
//   - RFC3339 timestamp → absolute UTC instant
//
// Returns the absolute UTC time the caller should pass to the
// rollup service. Defends against unbounded queries: the rollup
// SQL filters on `executions.created_at >= $2`, so an empty /
// invalid `since` must NOT mean "all time."
func parseSinceParam(raw string, defaultLookback time.Duration) (time.Time, error) {
	raw = trimSpace(raw)
	if raw == "" {
		return time.Now().Add(-defaultLookback).UTC(), nil
	}
	// Relative-duration shortcut: 7d / 24h / 30m / 90s. We accept
	// both Go's standard suffixes (h, m, s, ms, µs, ns) and our
	// own 'd' for days, which Go doesn't support natively.
	if d, ok := parseRelativeDuration(raw); ok {
		if d < 0 {
			return time.Time{}, errBadSince("negative duration")
		}
		return time.Now().Add(-d).UTC(), nil
	}
	// RFC3339 fallback for absolute timestamps.
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, errBadSince("unrecognised format: must be RFC3339 or <N>d / <N>h / <N>m")
}

// parseRelativeDuration extends time.ParseDuration with a 'd'
// suffix for days. Returns (d, true) on success, (_, false) when
// the value doesn't look like a Go-style duration.
func parseRelativeDuration(raw string) (time.Duration, bool) {
	if raw == "" {
		return 0, false
	}
	if n := len(raw); raw[n-1] == 'd' {
		// "7d" → parse leading int and multiply by 24h.
		days, err := strconv.Atoi(raw[:n-1])
		if err != nil {
			return 0, false
		}
		return time.Duration(days) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return d, true
}

// trimSpace mirrors strings.TrimSpace but avoids the import in
// this very small file. (strings is already imported across the
// api package — kept local to keep this surface self-contained.)
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

type errBadSince string

func (e errBadSince) Error() string { return string(e) }
