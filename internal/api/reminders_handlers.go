package api

// Reminders read/cancel API surface for the vornikctl reminders CLI
// and the UI tile.
//
//   GET  /api/v1/reminders          — list (filter by status, operator, project)
//   GET  /api/v1/reminders/{id}     — show one
//   POST /api/v1/reminders/{id}/cancel — flip to status='cancelled'
//
// No create endpoint in v1 — reminders are LLM-driven via the
// dispatcher's set_reminder tool. A future v2 may expose POST
// once we figure out the right auth shape for operator-direct
// reminders.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ReminderEntryJSON is the wire shape for one row. Times are
// RFC3339 strings so the CLI table renderer doesn't have to
// import time. Optional timestamps are omitempty for clean JSON.
type ReminderEntryJSON struct {
	ID              string `json:"id"`
	OperatorID      string `json:"operator_id"`
	Channel         string `json:"channel"`
	ChannelRef      string `json:"channel_ref"`
	ProjectID       string `json:"project_id,omitempty"`
	FireAt          string `json:"fire_at"`
	Content         string `json:"content"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	FiredAt         string `json:"fired_at,omitempty"`
	CancelledAt     string `json:"cancelled_at,omitempty"`
	CreatedVia      string `json:"created_via"`
	ErrorCount      int    `json:"error_count,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
	RecurrenceUntil string `json:"recurrence_until,omitempty"`
}

// ReminderListResponse wraps the list response.
type ReminderListResponse struct {
	Entries []ReminderEntryJSON `json:"entries"`
}

// ListReminders handles GET /api/v1/reminders. Query params:
// operator, project, status, limit.
func (s *Server) ListReminders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.reminderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "REMINDERS_DISABLED",
			"reminders repository not wired on this deployment")
		return
	}
	q := r.URL.Query()
	// Scoped non-admin callers don't get to choose `?operator=X`:
	// honouring the query param leaks the existence of arbitrary
	// operator IDs via DB-timing / row-count differences even
	// though `reminderVisibleToRequest` filters mismatching rows
	// out of the response. Admin keys + auth-off deployments keep
	// the override since single-tenant has no operator boundary.
	operatorFilter := q.Get("operator")
	if IsAuthEnabledFromContext(r.Context()) &&
		!s.adminConfig.IsAdminKey(APIKeyFromContext(r.Context())) {
		operatorFilter = requestOperatorID(r)
	}
	filter := persistence.ReminderListFilter{
		OperatorID: operatorFilter,
		ProjectID:  q.Get("project"),
		Status:     persistence.ReminderStatus(q.Get("status")),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := parseLimit(v, 1, 500); err == nil {
			filter.PageSize = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.reminderRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "list failed: "+err.Error())
		return
	}
	out := ReminderListResponse{Entries: make([]ReminderEntryJSON, 0, len(rows))}
	for _, rem := range rows {
		if !s.reminderVisibleToRequest(r, rem) {
			continue
		}
		out.Entries = append(out.Entries, reminderToJSON(rem))
	}
	respondJSON(w, http.StatusOK, out)
}

// ShowReminder handles GET /api/v1/reminders/{id}.
func (s *Server) ShowReminder(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	if s.reminderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "REMINDERS_DISABLED", "reminders repository not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rem, err := s.reminderRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "reminder not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "fetch failed")
		return
	}
	if !s.reminderVisibleToRequest(r, rem) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "access denied to reminder")
		return
	}
	respondJSON(w, http.StatusOK, reminderToJSON(rem))
}

// CancelReminder handles POST /api/v1/reminders/{id}/cancel.
// Idempotent on already-terminal rows (returns the row's current
// state without error).
func (s *Server) CancelReminder(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.reminderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "REMINDERS_DISABLED", "reminders repository not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rem, err := s.reminderRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "reminder not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "fetch failed")
		return
	}
	if !s.reminderVisibleToRequest(r, rem) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "access denied to reminder")
		return
	}
	if err := s.reminderRepo.Cancel(ctx, id); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "cancel failed: "+err.Error())
		return
	}
	// Audit. Best-effort. Same shape the UI emits so
	// /ui/admin/audit shows both surfaces in one timeline.
	if s.adminAuditRepo != nil {
		principal := apiArchivePrincipal(r)
		if principal == "" {
			principal = "api-anonymous"
		}
		afterJSON, _ := json.Marshal(map[string]any{"reminder_id": id})
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "api",
			Action:    "reminder.cancelled",
			Target:    id,
			After:     string(afterJSON),
			IP:        clientIPFromRequest(r),
			UserAgent: r.UserAgent(),
		})
	}
	updated, err := s.reminderRepo.Get(ctx, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "re-fetch failed")
		return
	}
	respondJSON(w, http.StatusOK, reminderToJSON(updated))
}

// DeleteReminder handles DELETE /api/v1/reminders/{id}. Physically
// removes the row — distinct from Cancel which preserves the row
// for audit. Intended for operator cleanup of stale rows that
// survived a project deletion or a recurring rule gone awry.
// Returns 404 if the row doesn't exist (allows idempotent cleanup
// scripts to ignore the "already gone" case).
func (s *Server) DeleteReminder(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "DELETE only")
		return
	}
	if s.reminderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "REMINDERS_DISABLED", "reminders repository not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rem, err := s.reminderRepo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "reminder not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "fetch failed")
		return
	}
	if !s.reminderVisibleToRequest(r, rem) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "access denied to reminder")
		return
	}
	if err := s.reminderRepo.Delete(ctx, id); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "reminder not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "INTERNAL", "delete failed: "+err.Error())
		return
	}
	if s.adminAuditRepo != nil {
		principal := apiArchivePrincipal(r)
		if principal == "" {
			principal = "api-anonymous"
		}
		beforeJSON, _ := json.Marshal(reminderToJSON(rem))
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "api",
			Action:    "reminder.deleted",
			Target:    id,
			Before:    string(beforeJSON),
			IP:        clientIPFromRequest(r),
			UserAgent: r.UserAgent(),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// remindersRouter dispatches /api/v1/reminders/{id} +
// /api/v1/reminders/{id}/cancel + /api/v1/reminders/from-text.
// The collection root is mounted directly on ListReminders.
func (s *Server) remindersRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/reminders/")
	if path == "" {
		s.ListReminders(w, r)
		return
	}
	// /from-text — natural-language creation endpoint. Matched
	// before the {id} fall-through so an operator can't squat
	// the literal id "from-text".
	if path == "from-text" {
		s.CreateReminderFromText(w, r)
		return
	}
	// {id}/cancel
	if strings.HasSuffix(path, "/cancel") {
		id := strings.TrimSuffix(path, "/cancel")
		if strings.Contains(id, "/") {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown reminder path")
			return
		}
		s.CancelReminder(w, r, id)
		return
	}
	// Plain {id}
	if strings.Contains(path, "/") {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown reminder path")
		return
	}
	// DELETE /api/v1/reminders/{id} is the manual-cleanup path; GET
	// is the show path. Other methods get a 405 from ShowReminder /
	// DeleteReminder respectively.
	if r.Method == http.MethodDelete {
		s.DeleteReminder(w, r, path)
		return
	}
	s.ShowReminder(w, r, path)
}

// reminderToJSON converts a persistence row to the wire shape.
func reminderToJSON(r *persistence.Reminder) ReminderEntryJSON {
	out := ReminderEntryJSON{
		ID:         r.ID,
		OperatorID: r.OperatorID,
		Channel:    r.Channel,
		ChannelRef: r.ChannelRef,
		ProjectID:  r.ProjectID,
		FireAt:     r.FireAt.UTC().Format(time.RFC3339),
		Content:    r.Content,
		Status:     string(r.Status),
		CreatedAt:  r.CreatedAt.UTC().Format(time.RFC3339),
		CreatedVia: r.CreatedVia,
		ErrorCount: r.ErrorCount,
		LastError:  r.LastError,
	}
	if r.FiredAt != nil {
		out.FiredAt = r.FiredAt.UTC().Format(time.RFC3339)
	}
	if r.CancelledAt != nil {
		out.CancelledAt = r.CancelledAt.UTC().Format(time.RFC3339)
	}
	if r.CronExpr != "" {
		out.CronExpr = r.CronExpr
	}
	if r.RecurrenceUntil != nil {
		out.RecurrenceUntil = r.RecurrenceUntil.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) reminderVisibleToRequest(r *http.Request, rem *persistence.Reminder) bool {
	if rem == nil {
		return false
	}
	if s.adminConfig.IsAdminKey(APIKeyFromContext(r.Context())) {
		return true
	}
	// Session-admin parity (2026-06-06 post-auth-flip regression): a
	// browser session whose principal resolved to role=admin is
	// admin-class exactly like an allowlisted key — without this, the
	// flip scoped admins down to "rows owned by my principal", which
	// for operator-owned (telegram:…) reminders is none.
	if SessionRoleFromContext(r.Context()) == "admin" {
		return true
	}
	if rem.ProjectID != "" {
		return requestAllowsProject(r, rem.ProjectID)
	}
	// Global rows (no project) — match by operator. requestAllowsOperator
	// handles the auth-off bypass internally.
	return requestAllowsOperator(r, rem.OperatorID)
}
