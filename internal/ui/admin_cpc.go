// CPC admin surface — /ui/admin/cpc. List + force-cancel page for
// the inter-project orchestration ledger (cross_project_calls).
//
// Mirrors the JSON API at /api/v1/admin/cpc[/{id}/cancel] but
// renders a filterable HTML table with one cancel button per
// non-terminal row. Same admin auth gate (mounted on /admin/*)
// guards the page; the cancel POST writes an audit row with
// action="interproject.cpc.admincancel" + principal so the trail
// distinguishes operator-cancels from executor auto-resolves.

package ui

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminCPCRow is one cross_project_calls row rendered to the
// admin table. Template-friendly shape: pre-formatted timestamps,
// status badge class, stable "in 3m / 7s ago" strings.
type AdminCPCRow struct {
	ID             string
	CallerProject  string
	CallerTaskID   string
	CallerStepID   string
	CalleeProject  string
	CalleeWorkflow string
	CalleeTaskID   string
	ExpectedSchema string
	Status         string
	StatusBadge    string // tailwind class set for the status pill
	ErrorMessage   string
	CreatedAt      string // 2006-01-02 15:04:05
	CreatedAgo     string // "3m ago"
	ResolvedAt     string
	TimeoutAt      string
	DurationLabel  string // "completed in 12s" / "running for 4m"
	IsTerminal     bool   // drives the "Cancel" button rendering
}

// AdminCPCData backs the /ui/admin/cpc template.
type AdminCPCData struct {
	adminCommonData
	Available     bool
	Rows          []AdminCPCRow
	FilterStatus  string
	FilterCaller  string
	FilterCallee  string
	FilterSince   string
	Limit         int
	LimitOptions  []int
	StatusOptions []string
	// FlashCancel is the one-shot "row <id> cancelled" banner shown
	// after a successful cancel POST. Empty on the bare list page.
	FlashCancel string
	// Error carries a render-blocking error message (repo failure,
	// invalid filter). Empty on the happy path.
	Error string
}

// AdminCPC renders /ui/admin/cpc. Filter axes: status, caller,
// callee, since, limit. The page is read-mostly — the only POST
// surface is /ui/admin/cpc/{id}/cancel, dispatched by adminRouter.
func (s *Server) AdminCPC(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := adminClampLimit(q.Get("limit"))
	data := AdminCPCData{
		adminCommonData: adminCommonData{
			Title:       "Cross-project calls",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:     s.crossProjectCallRepo != nil,
		FilterStatus:  strings.TrimSpace(q.Get("status")),
		FilterCaller:  strings.TrimSpace(q.Get("caller")),
		FilterCallee:  strings.TrimSpace(q.Get("callee")),
		FilterSince:   strings.TrimSpace(q.Get("since")),
		Limit:         limit,
		LimitOptions:  adminLimitOptions,
		StatusOptions: []string{"", "pending", "running", "completed", "failed", "timed_out", "rejected"},
		FlashCancel:   q.Get("cancelled"),
	}
	if !data.Available {
		s.render(w, "admin_cpc.html", data)
		return
	}

	filter := persistence.CPCListFilter{
		Status:        persistence.CrossProjectCallStatus(data.FilterStatus),
		CallerProject: data.FilterCaller,
		CalleeProject: data.FilterCallee,
		PageSize:      limit,
	}
	if data.FilterSince != "" {
		if t, err := time.Parse(time.RFC3339, data.FilterSince); err == nil {
			filter.CreatedSince = t
		} else if t, err := time.Parse("2006-01-02", data.FilterSince); err == nil {
			filter.CreatedSince = t
		} else {
			data.Error = "since: expected YYYY-MM-DD or RFC3339"
		}
	}

	if data.Error == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rows, err := s.crossProjectCallRepo.List(ctx, filter)
		if err != nil {
			s.logger.Warn().Err(err).Msg("admin cpc list failed")
			data.Error = "Failed to list cross-project calls: " + err.Error()
		} else {
			data.Rows = make([]AdminCPCRow, 0, len(rows))
			now := time.Now()
			for _, c := range rows {
				if c == nil {
					continue
				}
				data.Rows = append(data.Rows, cpcRowToAdminRow(c, now))
			}
		}
	}
	s.render(w, "admin_cpc.html", data)
}

// AdminCPCCancel handles POST /ui/admin/cpc/{id}/cancel. Mirrors
// the JSON-API cancel handler (writes the same audit action and
// goes through the same AdminCancel repo method) but redirects
// back to /ui/admin/cpc?cancelled=<id> on success so the operator
// sees a confirmation banner.
func (s *Server) AdminCPCCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.crossProjectCallRepo == nil {
		http.Error(w, "cross-project call ledger not wired", http.StatusServiceUnavailable)
		return
	}
	if id == "" {
		http.Error(w, "cpc id required", http.StatusBadRequest)
		return
	}

	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "operator-cancelled via /ui/admin/cpc"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Confirm the row exists first so a typo returns a clean 404
	// rather than a silent no-op.
	row, err := s.crossProjectCallRepo.Get(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if row == nil {
		http.NotFound(w, r)
		return
	}

	if err := s.crossProjectCallRepo.AdminCancel(ctx, id, reason); err != nil {
		s.logger.Warn().Err(err).Str("cpc_id", id).Msg("admin cpc cancel failed")
		http.Error(w, "cancel failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Audit. Mirror the API handler's action label so dashboards
	// filtering on `interproject.cpc.admincancel` pick up both
	// surfaces. Principal comes off the admin gate context if set;
	// fall back to "ui-admin" so the row never has a blank field.
	if s.adminAuditRepo != nil {
		principal := adminPrincipal(r)
		if principal == "" || principal == "unknown" {
			principal = "ui-admin"
		}
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "ui",
			Action:    "interproject.cpc.admincancel",
			Target:    row.CalleeProject,
			After:     `{"cpc_id":"` + id + `","reason":` + strconv.Quote(reason) + `,"prior_status":"` + string(row.Status) + `"}`,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}

	// Preserve the operator's filter state on the redirect — they're
	// usually mid-triage and re-typing the filter to get back is
	// friction.
	q := url.Values{}
	q.Set("cancelled", id)
	if v := r.FormValue("filter_status"); v != "" {
		q.Set("status", v)
	}
	if v := r.FormValue("filter_caller"); v != "" {
		q.Set("caller", v)
	}
	if v := r.FormValue("filter_callee"); v != "" {
		q.Set("callee", v)
	}
	http.Redirect(w, r, "/ui/admin/cpc?"+q.Encode(), http.StatusSeeOther)
}

// cpcRowToAdminRow converts a persistence row to the
// template-friendly admin shape. Pure — no I/O, easy to unit test.
func cpcRowToAdminRow(c *persistence.CrossProjectCall, now time.Time) AdminCPCRow {
	row := AdminCPCRow{
		ID:             c.ID,
		CallerProject:  c.CallerProject,
		CallerTaskID:   c.CallerTaskID,
		CallerStepID:   c.CallerStepID,
		CalleeProject:  c.CalleeProject,
		CalleeWorkflow: c.CalleeWorkflow,
		ExpectedSchema: c.ExpectedSchema,
		Status:         string(c.Status),
		StatusBadge:    cpcStatusBadgeClass(c.Status),
		IsTerminal:     c.Status.IsTerminal(),
		CreatedAt:      c.CreatedAt.Format("2006-01-02 15:04:05"),
		CreatedAgo:     humanDuration(now.Sub(c.CreatedAt)) + " ago",
	}
	if c.CalleeTaskID != nil {
		row.CalleeTaskID = *c.CalleeTaskID
	}
	if c.ErrorMessage != nil {
		row.ErrorMessage = *c.ErrorMessage
	}
	if c.TimeoutAt != nil {
		row.TimeoutAt = c.TimeoutAt.Format("2006-01-02 15:04:05")
	}
	if c.ResolvedAt != nil {
		row.ResolvedAt = c.ResolvedAt.Format("2006-01-02 15:04:05")
		row.DurationLabel = "resolved in " + humanDuration(c.ResolvedAt.Sub(c.CreatedAt))
	} else {
		row.DurationLabel = "running for " + humanDuration(now.Sub(c.CreatedAt))
	}
	return row
}

// cpcStatusBadgeClass picks the tailwind colour for a CPC status
// pill. Mirrors the replay-tree palette so an operator sees the
// same colour for "completed" / "failed" across pages.
// cpcStatusBadgeClass returns theme-aware semantic pill classes (see the .pill
// primitive in _partials.html). The consuming span adds only font-mono.
func cpcStatusBadgeClass(s persistence.CrossProjectCallStatus) string {
	switch s {
	case persistence.CPCStatusCompleted:
		return "pill pill-ok"
	case persistence.CPCStatusFailed, persistence.CPCStatusRejected:
		return "pill pill-danger"
	case persistence.CPCStatusTimedOut:
		return "pill pill-warn"
	case persistence.CPCStatusRunning:
		return "pill pill-info"
	case persistence.CPCStatusPending:
		return "pill pill-neutral"
	default:
		return "pill pill-neutral"
	}
}

// humanDuration renders a coarse "3m 12s" / "1h 24m" / "2d 4h"
// label. Sub-second values collapse to "0s" so the admin table
// doesn't show "734ms" type strings. Negative inputs render the
// absolute value (the timestamp drift case).
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(m) + "m " + strconv.Itoa(s) + "s"
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return strconv.Itoa(days) + "d"
	}
	return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
}
