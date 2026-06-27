// Instinct admin surface — /ui/admin/instincts. A read-only browser
// over the continuous-learning instinct layer (LLD:
// continuous-learning-instinct-layer-design.md), mirroring the JSON API
// at /api/v1/instincts with a filterable HTML table.
//
// The page reads the same instinctRepo the failed-task playbook panel
// uses; it is browse-only here (the only mutating surface is a per-row
// Retire POST → /ui/admin/instincts/{id}/retire), so nothing about
// agent behaviour changes. The admin gate (mounted on /admin/*) guards
// the page.

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

// AdminInstinctRow is one instinct rendered to the admin table.
// Template-friendly shape: pre-formatted timestamps + status badge.
type AdminInstinctRow struct {
	ID              string
	Scope           string
	ProjectID       string
	Domain          string
	Action          string
	Trigger         string // raw trigger_json string ("" when empty)
	Confidence      string // "0.82"
	SupportCount    int
	ContradictCount int
	Source          string
	Status          string
	StatusBadge     string // tailwind class set for the status pill
	DistillModel    string
	LastSeenAt      string // 2006-01-02 15:04:05
	LastSeenAgo     string // "3m ago"
	IsRetired       bool   // drives the Retire button rendering
	// Application-feedback tally (slice 7 lift column). Succeeded counts
	// 'succeeded' rows; Failed collapses 'failed'+'rejected'; Ignored
	// counts surfaced-but-unresolved rows. 'accepted' is excluded.
	AppSucceeded int
	AppFailed    int
	AppIgnored   int
	// LiftSummary is the compact (<20-char) rendering of the tally,
	// "-" when no applications have been recorded.
	LiftSummary string
}

// AdminInstinctsData backs the /ui/admin/instincts template.
type AdminInstinctsData struct {
	adminCommonData
	Available     bool
	Rows          []AdminInstinctRow
	FilterDomain  string
	FilterScope   string
	FilterProject string
	FilterStatus  string
	FilterMinConf string
	Limit         int
	LimitOptions  []int
	DomainOptions []string
	ScopeOptions  []string
	StatusOptions []string
	// FlashRetired is the one-shot "instinct <id> retired" banner shown
	// after a successful retire POST. Empty on the bare list page.
	FlashRetired string
	// Error carries a render-blocking error (repo failure, bad filter).
	Error string
}

// AdminInstincts renders /ui/admin/instincts. Filter axes: domain,
// scope, project, status, min_confidence, limit. Read-only — the only
// POST surface is /ui/admin/instincts/{id}/retire, dispatched by
// adminRouter.
func (s *Server) AdminInstincts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := adminClampLimit(q.Get("limit"))
	data := AdminInstinctsData{
		adminCommonData: adminCommonData{
			Title:       "Instincts",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:     s.instinctRepo != nil,
		FilterDomain:  strings.TrimSpace(q.Get("domain")),
		FilterScope:   strings.TrimSpace(q.Get("scope")),
		FilterProject: strings.TrimSpace(q.Get("project")),
		FilterStatus:  strings.TrimSpace(q.Get("status")),
		FilterMinConf: strings.TrimSpace(q.Get("min_confidence")),
		Limit:         limit,
		LimitOptions:  adminLimitOptions,
		DomainOptions: []string{"", persistence.InstinctDomainRecovery, persistence.InstinctDomainCost, persistence.InstinctDomainQuality, persistence.InstinctDomainRetrieval, persistence.InstinctDomainWorkflow, persistence.InstinctDomainBudget},
		ScopeOptions:  []string{"", persistence.InstinctScopeProject, persistence.InstinctScopeGlobal},
		StatusOptions: []string{"", persistence.InstinctStatusCandidate, persistence.InstinctStatusActive, persistence.InstinctStatusPromoted, persistence.InstinctStatusRetired},
		FlashRetired:  q.Get("retired"),
	}
	if !data.Available {
		s.render(w, "admin_instincts.html", data)
		return
	}

	filter := persistence.InstinctFilter{PageSize: limit}
	if data.FilterDomain != "" {
		filter.Domain = &data.FilterDomain
	}
	if data.FilterScope != "" {
		filter.Scope = &data.FilterScope
	}
	if data.FilterProject != "" {
		filter.ProjectID = &data.FilterProject
	}
	if data.FilterStatus != "" {
		filter.Status = &data.FilterStatus
	}
	if data.FilterMinConf != "" {
		f, err := strconv.ParseFloat(data.FilterMinConf, 64)
		if err != nil || f < 0 || f > 1 {
			data.Error = "min_confidence: expected a number in [0,1]"
		} else {
			filter.MinConfidence = &f
		}
	}

	if data.Error == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rows, err := s.instinctRepo.List(ctx, filter)
		if err != nil {
			s.logger.Warn().Err(err).Msg("admin instincts list failed")
			data.Error = "Failed to list instincts: " + err.Error()
		} else {
			// Gather IDs and fetch the application-feedback tally in one
			// shot. Fail-soft: a counts error logs a warning and leaves the
			// map nil, so the lift column renders dashes but the page still
			// loads. Reuses the same 5s ctx as the List call above.
			ids := make([]string, 0, len(rows))
			for _, in := range rows {
				if in != nil {
					ids = append(ids, in.ID)
				}
			}
			var counts map[string]*persistence.InstinctApplicationCounts
			if len(ids) > 0 {
				c, cerr := s.instinctRepo.ListApplicationCounts(ctx, ids)
				if cerr != nil {
					s.logger.Warn().Err(cerr).Msg("admin instincts application counts failed; rendering without lift")
				} else {
					counts = c
				}
			}
			data.Rows = make([]AdminInstinctRow, 0, len(rows))
			now := time.Now()
			for _, in := range rows {
				if in == nil {
					continue
				}
				data.Rows = append(data.Rows, instinctRowToAdminRow(in, now, counts[in.ID]))
			}
		}
	}
	s.render(w, "admin_instincts.html", data)
}

// AdminInstinctRetire handles POST /ui/admin/instincts/{id}/retire.
// Mirrors the JSON-API retire handler (same Retire repo method) but
// redirects back to /ui/admin/instincts?retired=<id> on success so the
// operator sees a confirmation banner. Advisory only — the row stays
// for audit and nothing about agent behaviour changes.
func (s *Server) AdminInstinctRetire(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.instinctRepo == nil {
		http.Error(w, "instinct repository not wired", http.StatusServiceUnavailable)
		return
	}
	if id == "" {
		http.Error(w, "instinct id required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.instinctRepo.Retire(ctx, id); err != nil {
		if err == persistence.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("instinct_id", id).Msg("admin instinct retire failed")
		http.Error(w, "retire failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if s.adminAuditRepo != nil {
		principal := adminPrincipal(r)
		if principal == "" || principal == "unknown" {
			principal = "ui-admin"
		}
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: principal,
			Source:    "ui",
			Action:    "instinct.retired",
			Target:    id,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}

	// Preserve the operator's filter state on the redirect.
	qq := url.Values{}
	qq.Set("retired", id)
	for _, k := range []string{"domain", "scope", "project", "status", "min_confidence"} {
		if v := r.FormValue("filter_" + k); v != "" {
			qq.Set(k, v)
		}
	}
	http.Redirect(w, r, "/ui/admin/instincts?"+qq.Encode(), http.StatusSeeOther)
}

// instinctRowToAdminRow converts a persistence row to the
// template-friendly admin shape. Pure — no I/O, easy to unit test.
// counts may be nil (no applications recorded for this instinct, or the
// counts query failed) — that yields zero buckets and a "-" LiftSummary.
func instinctRowToAdminRow(in *persistence.Instinct, now time.Time, counts *persistence.InstinctApplicationCounts) AdminInstinctRow {
	row := AdminInstinctRow{
		ID:              in.ID,
		Scope:           in.Scope,
		ProjectID:       in.ProjectID,
		Domain:          in.Domain,
		Action:          in.Action,
		Confidence:      strconv.FormatFloat(in.Confidence, 'f', 2, 64),
		SupportCount:    in.SupportCount,
		ContradictCount: in.ContradictCount,
		Source:          in.Source,
		Status:          in.Status,
		StatusBadge:     instinctStatusBadgeClass(in.Status),
		DistillModel:    in.DistillModel,
		IsRetired:       in.Status == persistence.InstinctStatusRetired,
		LastSeenAt:      in.LastSeenAt.Format("2006-01-02 15:04:05"),
		LastSeenAgo:     humanDuration(now.Sub(in.LastSeenAt)) + " ago",
	}
	if len(in.Trigger) > 0 {
		row.Trigger = string(in.Trigger)
	}
	if counts != nil {
		row.AppSucceeded = counts.Succeeded
		row.AppFailed = counts.Failed
		row.AppIgnored = counts.Ignored
	}
	row.LiftSummary = instinctLiftSummary(row.AppSucceeded, row.AppFailed, row.AppIgnored)
	return row
}

// instinctLiftSummary renders an application-feedback tally as a compact
// (<20-char) string for the admin lift column: "-" when every bucket is
// zero, else space-joined non-zero buckets, e.g. "3↑ 2↓ 1…" (succeeded
// up-arrow, failed down-arrow, ignored ellipsis). Buckets that are zero
// are omitted so the cell stays tight. Pure — no I/O.
func instinctLiftSummary(succeeded, failed, ignored int) string {
	if succeeded == 0 && failed == 0 && ignored == 0 {
		return "-"
	}
	parts := make([]string, 0, 3)
	if succeeded > 0 {
		parts = append(parts, strconv.Itoa(succeeded)+"↑")
	}
	if failed > 0 {
		parts = append(parts, strconv.Itoa(failed)+"↓")
	}
	if ignored > 0 {
		parts = append(parts, strconv.Itoa(ignored)+"…")
	}
	return strings.Join(parts, " ")
}

// instinctStatusBadgeClass picks the tailwind colour for an instinct
// status pill. Reuses the palette the other admin tables use so an
// operator sees consistent colours across pages.
// instinctStatusBadgeClass returns theme-aware semantic pill classes (see the
// .pill primitive in _partials.html). The consuming span adds only font-mono.
func instinctStatusBadgeClass(status string) string {
	switch status {
	case persistence.InstinctStatusPromoted:
		return "pill pill-ok"
	case persistence.InstinctStatusActive:
		return "pill pill-info"
	case persistence.InstinctStatusCandidate:
		return "pill pill-neutral"
	case persistence.InstinctStatusRetired:
		return "pill pill-danger"
	default:
		return "pill pill-neutral"
	}
}
