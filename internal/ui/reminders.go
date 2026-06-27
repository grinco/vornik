package ui

// UI mutation surface for reminders.
//
//   POST /ui/reminders/{id}/cancel — flips status to cancelled
//                                    (row stays in the list)
//   POST /ui/reminders/{id}/delete — physically removes the row
//                                    (B-12, for stale-row cleanup)
//
// Both redirect to Referer or /ui/projects if the request didn't
// carry one. Listing is rendered inline on /ui/projects/{id} via
// the data loader in project_detail.go; this file owns the
// mutations.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// uiRefererDest returns a safe same-origin redirect target derived from the
// request's Referer, falling back to `fallback` when there's no usable one.
//
// r.Referer() is the browser-sent ABSOLUTE URL (scheme://host/path[?query]),
// so the previous strings.HasPrefix(ref, "/ui/") guard never matched a real
// browser request and every reminder mutation fell through to the fallback
// (the "delete sends me to /ui/projects instead of back to reminders" bug).
// We parse the Referer, keep it only when its PATH is under /ui/, and return
// just the path+query — dropping scheme/host so the redirect is always local
// (a crafted off-site Referer can't bounce the operator off the daemon).
func uiRefererDest(r *http.Request, fallback string) string {
	ref := r.Referer()
	if ref == "" {
		return fallback
	}
	u, err := url.Parse(ref)
	if err != nil || !strings.HasPrefix(u.Path, "/ui/") {
		return fallback
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// RemindersListData backs the /ui/reminders dashboard.
type RemindersListData struct {
	Title         string
	CurrentPage   string
	Rows          []ReminderListRow
	Available     bool   // false when the repo isn't wired (renders an empty state)
	FilterStatus  string // ?status=pending|fired|cancelled|...
	StatusOptions []string
	FilterProject string
	// Counts surfaces a small header summary so operators see
	// at-a-glance "12 pending, 4 fired, 1 cancelled" without
	// having to scroll.
	Counts ReminderListCounts
}

// ReminderListCounts mirrors a small group-by query over the
// rows the handler loaded — keeps the template declarative.
type ReminderListCounts struct {
	Total     int
	Pending   int
	Fired     int
	Cancelled int
	Other     int // firing / expired
}

// ReminderListRow is one card in the dashboard. Pre-formatted
// fire time + countdown + status badge class so the template
// doesn't do time math inline.
type ReminderListRow struct {
	ID              string
	Status          string
	StatusBadge     string // tailwind classes for the pill
	OperatorID      string
	Channel         string
	ProjectID       string
	Content         string
	FireAt          string // "2026-05-24 09:00:00 MST"
	FireAtISO       string // RFC3339 for client-side tickers
	Countdown       string // "in 6h 12m" / "due now" / "fired 3m ago" / "cancelled 1d ago"
	CreatedVia      string
	IsTerminal      bool   // drives whether the Cancel button renders
	CronExpr        string // 5-field POSIX cron; "" for one-shot
	RecurrenceUntil string // human-readable bound; "" for unbounded
	IsRecurring     bool   // drives the cron badge in the row
}

// Reminders renders /ui/reminders — the per-operator dashboard
// listing every reminder across the daemon. Filters: status,
// project.
func (s *Server) Reminders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	data := RemindersListData{
		Title:         "Reminders",
		CurrentPage:   "reminders",
		Available:     s.reminderRepo != nil,
		FilterStatus:  strings.TrimSpace(q.Get("status")),
		FilterProject: strings.TrimSpace(q.Get("project")),
		StatusOptions: []string{"", "pending", "firing", "fired", "cancelled", "expired"},
	}
	if !data.Available {
		s.render(w, "reminders.html", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	filter := persistence.ReminderListFilter{
		Status:    persistence.ReminderStatus(data.FilterStatus),
		ProjectID: data.FilterProject,
		PageSize:  200,
	}
	rows := s.listRemindersForRequest(ctx, r, filter)
	now := time.Now()
	for _, rem := range rows {
		if rem == nil {
			continue
		}
		if !reminderVisibleToUIRequest(r, rem) {
			continue
		}
		row := ReminderListRow{
			ID:          rem.ID,
			Status:      string(rem.Status),
			StatusBadge: reminderStatusBadge(rem.Status),
			OperatorID:  rem.OperatorID,
			Channel:     rem.Channel,
			ProjectID:   rem.ProjectID,
			Content:     rem.Content,
			FireAt:      rem.FireAt.Local().Format("2006-01-02 15:04:05 MST"),
			FireAtISO:   rem.FireAt.UTC().Format(time.RFC3339),
			CreatedVia:  rem.CreatedVia,
			IsTerminal:  rem.Status.IsTerminal(),
			Countdown:   reminderCountdown(rem, now),
			CronExpr:    rem.CronExpr,
			IsRecurring: rem.IsRecurring(),
		}
		if rem.RecurrenceUntil != nil {
			row.RecurrenceUntil = rem.RecurrenceUntil.Local().Format("2006-01-02 15:04 MST")
		}
		switch rem.Status {
		case persistence.ReminderStatusPending, persistence.ReminderStatusFiring:
			data.Counts.Pending++
		case persistence.ReminderStatusFired:
			data.Counts.Fired++
		case persistence.ReminderStatusCancelled:
			data.Counts.Cancelled++
		default:
			data.Counts.Other++
		}
		data.Counts.Total++
		data.Rows = append(data.Rows, row)
	}
	s.render(w, "reminders.html", data)
}

// listRemindersForRequest returns the reminder sample honouring the
// caller's scope. An admin / all-access caller (and any explicit
// ?project filter) gets a single global query — the
// reminderVisibleToUIRequest post-filter remains the authoritative scope
// gate either way. A project-scoped caller with no explicit project gets
// a per-project merge plus their own operator-owned rows, so reminders
// past the global page cap aren't silently dropped (the
// global-page-then-post-filter under-display flagged by the
// cross-project visibility scope audit).
func (s *Server) listRemindersForRequest(ctx context.Context, r *http.Request, base persistence.ReminderListFilter) []*persistence.Reminder {
	scoped, isScoped := api.RequestScopedProjects(r)
	if !isScoped || base.ProjectID != "" {
		rows, err := s.reminderRepo.List(ctx, base)
		if err != nil {
			s.logger.Warn().Err(err).Msg("ui: reminders dashboard list failed")
			return nil
		}
		return rows
	}

	seen := make(map[string]bool)
	var merged []*persistence.Reminder
	collect := func(rows []*persistence.Reminder) {
		for _, rem := range rows {
			if rem == nil || seen[rem.ID] {
				continue
			}
			seen[rem.ID] = true
			merged = append(merged, rem)
		}
	}
	for _, p := range scoped {
		f := base
		f.ProjectID = p
		rows, err := s.reminderRepo.List(ctx, f)
		if err != nil {
			s.logger.Warn().Err(err).Str("project_id", p).Msg("ui: scoped reminder list failed")
			continue
		}
		collect(rows)
	}
	// Project-less, operator-owned reminders (e.g. telegram:… one-shots
	// with no project) are visible to their owner; the per-project sweep
	// above can't reach them, so fetch by the caller's operator id.
	if op := api.RequestOperatorID(r); op != "" {
		f := base
		f.OperatorID = op
		if rows, err := s.reminderRepo.List(ctx, f); err == nil {
			collect(rows)
		}
	}
	if base.PageSize > 0 && len(merged) > base.PageSize {
		merged = merged[:base.PageSize]
	}
	return merged
}

// reminderStatusBadge returns theme-aware semantic pill classes (see the .pill
// primitive in _partials.html). The consuming span adds only font-mono.
func reminderStatusBadge(s persistence.ReminderStatus) string {
	switch s {
	case persistence.ReminderStatusFired:
		return "pill pill-ok"
	case persistence.ReminderStatusCancelled:
		return "pill pill-neutral"
	case persistence.ReminderStatusExpired:
		return "pill pill-danger"
	case persistence.ReminderStatusFiring:
		return "pill pill-info"
	case persistence.ReminderStatusPending:
		return "pill pill-warn"
	default:
		return "pill pill-neutral"
	}
}

// reminderCountdown renders the human-readable status-dependent
// duration label. Pending → "in 6h 12m" / "due now"; fired →
// "fired 3m ago"; cancelled → "cancelled 1d ago".
func reminderCountdown(rem *persistence.Reminder, now time.Time) string {
	switch rem.Status {
	case persistence.ReminderStatusFired:
		if rem.FiredAt != nil {
			return "fired " + humanDuration(now.Sub(*rem.FiredAt)) + " ago"
		}
		return "fired"
	case persistence.ReminderStatusCancelled:
		if rem.CancelledAt != nil {
			return "cancelled " + humanDuration(now.Sub(*rem.CancelledAt)) + " ago"
		}
		return "cancelled"
	case persistence.ReminderStatusExpired:
		return "expired"
	default:
		// Pending / firing: countdown to fire_at.
		remaining := rem.FireAt.Sub(now)
		if remaining <= 0 {
			return "due now"
		}
		return "in " + humanDuration(remaining)
	}
}

// ReminderCancel cancels one reminder and redirects back. POST
// /ui/reminders/{id}/cancel. Mirrors the API endpoint shape so
// future GitOps tooling can hit either surface.
func (s *Server) ReminderCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.reminderRepo == nil {
		http.Error(w, "reminders not configured", http.StatusServiceUnavailable)
		return
	}
	if id == "" || strings.ContainsAny(id, `/\`) {
		http.Error(w, "invalid reminder id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rem, err := s.reminderRepo.Get(ctx, id)
	if err != nil {
		s.logger.Warn().Err(err).Str("reminder_id", id).Msg("ui: load reminder before cancel failed")
		http.Error(w, "reminder not found", http.StatusNotFound)
		return
	}
	if !reminderVisibleToUIRequest(r, rem) {
		http.Error(w, "access denied to reminder", http.StatusForbidden)
		return
	}
	if err := s.reminderRepo.Cancel(ctx, id); err != nil {
		s.logger.Warn().Err(err).Str("reminder_id", id).Msg("ui: cancel reminder failed")
		http.Error(w, "cancel failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Audit so /ui/admin/audit reflects the full reminder
	// lifecycle (set → fire → cancel). Best-effort.
	if s.adminAuditRepo != nil {
		principal := adminPrincipal(r)
		if principal == "" || principal == "unknown" {
			principal = "ui-anonymous"
		}
		afterJSON, _ := json.Marshal(map[string]any{"reminder_id": id})
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "ui",
			Action:    "reminder.cancelled",
			Target:    id,
			After:     string(afterJSON),
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	// Return to the page the operator was on (safe-listed to local /ui/
	// paths), else the projects dashboard.
	http.Redirect(w, r, uiRefererDest(r, "/ui/projects"), http.StatusSeeOther)
}

// ReminderDelete physically removes one reminder and redirects
// back. POST /ui/reminders/{id}/delete. Mirrors the API's
// DELETE /api/v1/reminders/{id} shape, just with a POST form
// because browsers can't issue real DELETE from a <form>.
//
// Distinct from Cancel: cancel flips status to "cancelled" but
// keeps the row visible in the list; delete actually removes it.
// Operators need delete to clean up the stale-cancelled /
// stale-fired rows that accumulate over weeks. Available on
// every row regardless of terminal status — that's the whole
// point of having it.
func (s *Server) ReminderDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.reminderRepo == nil {
		http.Error(w, "reminders not configured", http.StatusServiceUnavailable)
		return
	}
	if id == "" || strings.ContainsAny(id, `/\`) {
		http.Error(w, "invalid reminder id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rem, err := s.reminderRepo.Get(ctx, id)
	if err != nil {
		// Idempotent on 404 — operator likely clicked Delete twice
		// or another tab already removed it. Redirect rather than
		// surfacing an error; the row will be gone from the list
		// on the next render either way.
		if dest := uiRefererDest(r, ""); dest != "" {
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		http.Error(w, "reminder not found", http.StatusNotFound)
		return
	}
	if !reminderVisibleToUIRequest(r, rem) {
		http.Error(w, "access denied to reminder", http.StatusForbidden)
		return
	}
	if err := s.reminderRepo.Delete(ctx, id); err != nil {
		s.logger.Warn().Err(err).Str("reminder_id", id).Msg("ui: delete reminder failed")
		http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Audit row so /ui/admin/audit reflects the deletion (B-12 API
	// handler does the same; mirror here for UI-driven deletes).
	if s.adminAuditRepo != nil {
		principal := adminPrincipal(r)
		if principal == "" || principal == "unknown" {
			principal = "ui-anonymous"
		}
		// Capture pre-delete snapshot so the audit trail keeps the
		// deleted reminder's content even after the row is gone.
		beforeJSON, _ := json.Marshal(map[string]any{
			"reminder_id": id,
			"status":      string(rem.Status),
			"channel":     rem.Channel,
			"operator_id": rem.OperatorID,
			"project_id":  rem.ProjectID,
			"fire_at":     rem.FireAt.UTC().Format(time.RFC3339),
			"content":     rem.Content,
		})
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "ui",
			Action:    "reminder.deleted",
			Target:    id,
			Before:    string(beforeJSON),
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, uiRefererDest(r, "/ui/projects"), http.StatusSeeOther)
}

func reminderVisibleToUIRequest(r *http.Request, rem *persistence.Reminder) bool {
	if rem == nil {
		return false
	}
	// Admin-class requests see every reminder. admin.Middleware stamps
	// IsAdmin on EVERY /ui page (not just /ui/admin) for admin keys
	// AND session-admins, so this is the one check that covers both.
	// Regression 2026-06-06 (post auth-flip): without it, global
	// operator-owned rows (telegram:…) were invisible to the admin —
	// the dashboard rendered "No reminders" over a populated table.
	if admin.IsAdminFromContext(r.Context()) {
		return true
	}
	if rem.ProjectID != "" {
		return api.RequestAllowsProject(r, rem.ProjectID)
	}
	return api.RequestAllowsOperator(r, rem.OperatorID)
}
