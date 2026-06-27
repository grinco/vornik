package ui

// /ui/admin/blackbox/overrides — operator surface for the Phase B
// workflow_healing_overrides table. Two routes:
//
//   GET  /ui/admin/blackbox/overrides            → list page
//   POST /ui/admin/blackbox/overrides/save       → upsert one row
//   POST /ui/admin/blackbox/overrides/delete     → delete one row
//
// Same admin gate + audit + nil-safe pattern as the rest of
// /admin/*. The detector reads these rows in the next commit; this
// commit ships the surface so operators can pre-populate overrides
// before the detector wiring lands.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// HealingOverrideRow is the pre-formatted shape the list template
// renders. Pre-computes display strings so the template stays
// arithmetic-free.
type HealingOverrideRow struct {
	ProjectID         string
	WorkflowID        string
	Class             string
	ThresholdDisplay  string // e.g. "+50.0%" or "—"
	MutedUntilDisplay string // formatted UTC or "—"
	MutedActive       bool   // true when MutedUntil > now
	Notes             string
	CreatedBy         string
	UpdatedAt         string // formatted UTC
}

// BlackBoxOverridesData backs /ui/admin/blackbox/overrides.
type BlackBoxOverridesData struct {
	adminCommonData
	Available   bool
	Rows        []HealingOverrideRow
	ActionError string
	// PrefilledFor lets the trigger detail page link an operator
	// straight to a populated edit form via
	// ?project=…&workflow=…&class=…. Empty when no prefill.
	PrefilledProject  string
	PrefilledWorkflow string
	PrefilledClass    string
	// ClassOptions powers the class select. Mirrors the
	// HealingTriggerClass constants.
	ClassOptions []string
}

// healingOverrideToRow formats one override row for the template.
// Threshold display is computed as "+%.1f%%" when set; MutedUntil
// shows the absolute timestamp + whether it's still active.
func healingOverrideToRow(o *persistence.HealingTriggerOverride, now time.Time) HealingOverrideRow {
	row := HealingOverrideRow{
		ProjectID:         o.ProjectID,
		WorkflowID:        o.WorkflowID,
		Class:             string(o.TriggerClass),
		Notes:             o.Notes,
		CreatedBy:         o.CreatedBy,
		UpdatedAt:         o.UpdatedAt.UTC().Format("2006-01-02 15:04 UTC"),
		ThresholdDisplay:  "—",
		MutedUntilDisplay: "—",
	}
	if o.ThresholdOverride != nil {
		row.ThresholdDisplay = "+" + strconv.FormatFloat(100.0*(*o.ThresholdOverride), 'f', 1, 64) + "%"
	}
	if o.MutedUntil != nil {
		row.MutedUntilDisplay = o.MutedUntil.UTC().Format("2006-01-02 15:04 UTC")
		row.MutedActive = o.MutedUntil.After(now)
	}
	return row
}

// classOptions returns the trigger-class enum as a string slice
// for the form's select. Kept in one place so adding a new class
// (e.g. latency_regression) only requires updating the persistence
// constants and this helper.
func classOptions() []string {
	return []string{
		string(persistence.HealingTriggerFailureRateSpike),
		string(persistence.HealingTriggerCostRegression),
	}
}

// AdminBlackBoxOverrides renders the list page and the edit form
// for a single (project, workflow, class) override.
func (s *Server) AdminBlackBoxOverrides(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := BlackBoxOverridesData{
		adminCommonData: adminCommonData{
			Title:       "Trigger overrides",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
		Available:         s.healingOverrideRepo != nil,
		PrefilledProject:  strings.TrimSpace(q.Get("project")),
		PrefilledWorkflow: strings.TrimSpace(q.Get("workflow")),
		PrefilledClass:    strings.TrimSpace(q.Get("class")),
		ActionError:       strings.TrimSpace(q.Get("action_error")),
		ClassOptions:      classOptions(),
	}
	if !data.Available {
		s.render(w, "admin_blackbox_overrides.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.healingOverrideRepo.List(ctx, 200)
	if err != nil {
		s.logger.Warn().Err(err).Msg("override list failed")
	} else {
		now := time.Now().UTC()
		for _, o := range rows {
			data.Rows = append(data.Rows, healingOverrideToRow(o, now))
		}
	}
	s.render(w, "admin_blackbox_overrides.html", data)
}

// AdminBlackBoxOverrideSave handles
// POST /ui/admin/blackbox/overrides/save. Parses the form, upserts,
// audits, redirects.
//
// Form fields:
//   - project, workflow, class (required)
//   - threshold_pct (optional, percent string like "50" or "12.5")
//   - mute_hours (optional, integer; resolved as now + N hours)
//   - clear_mute (optional, "1" wipes any existing mute)
//   - notes (optional)
//
// Behaviour: at least one of threshold_pct OR mute_hours OR
// clear_mute must be set (otherwise nothing to save — redirect with
// banner). An empty threshold field is interpreted as "unset"
// (NULL); the operator wipes a threshold by submitting blank.
func (s *Server) AdminBlackBoxOverrideSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingOverrideRepo == nil {
		http.Error(w, "override repo not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	workflow := strings.TrimSpace(r.FormValue("workflow"))
	class := strings.TrimSpace(r.FormValue("class"))
	if project == "" || workflow == "" || class == "" {
		redirectOverrides(w, r, "project, workflow, and class are all required",
			project, workflow, class)
		return
	}
	if !isValidClass(class) {
		redirectOverrides(w, r, "unknown trigger class: "+class, project, workflow, class)
		return
	}

	o := &persistence.HealingTriggerOverride{
		ProjectID:    project,
		WorkflowID:   workflow,
		TriggerClass: persistence.HealingTriggerClass(class),
		Notes:        strings.TrimSpace(r.FormValue("notes")),
		CreatedBy:    adminPrincipal(r),
	}
	if raw := strings.TrimSpace(r.FormValue("threshold_pct")); raw != "" {
		v, perr := strconv.ParseFloat(raw, 64)
		if perr != nil {
			redirectOverrides(w, r, "threshold must be numeric (got "+raw+")",
				project, workflow, class)
			return
		}
		if v <= 0 {
			redirectOverrides(w, r, "threshold must be > 0",
				project, workflow, class)
			return
		}
		// Stored as relative delta, not percentage.
		rel := v / 100.0
		o.ThresholdOverride = &rel
	}
	clearMute := r.FormValue("clear_mute") == "1"
	if raw := strings.TrimSpace(r.FormValue("mute_hours")); raw != "" && !clearMute {
		h, perr := strconv.Atoi(raw)
		if perr != nil || h <= 0 {
			redirectOverrides(w, r, "mute_hours must be a positive integer",
				project, workflow, class)
			return
		}
		until := time.Now().UTC().Add(time.Duration(h) * time.Hour)
		o.MutedUntil = &until
	}
	if o.ThresholdOverride == nil && o.MutedUntil == nil && !clearMute {
		redirectOverrides(w, r, "nothing to save: set a threshold, a mute window, or clear_mute",
			project, workflow, class)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.healingOverrideRepo.Upsert(ctx, o); err != nil {
		s.logger.Warn().Err(err).
			Str("project", project).Str("workflow", workflow).Str("class", class).
			Msg("override save failed")
		redirectOverrides(w, r, err.Error(), project, workflow, class)
		return
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: adminPrincipal(r),
			Source:    "ui",
			Action:    "blackbox-override.saved",
			Target:    project + "/" + workflow + "/" + class,
			After:     o.Notes,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, "/ui/admin/blackbox/overrides", http.StatusSeeOther)
}

// AdminBlackBoxOverrideDelete handles
// POST /ui/admin/blackbox/overrides/delete. Body shape mirrors save:
// project + workflow + class. Idempotent at the repo level.
func (s *Server) AdminBlackBoxOverrideDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.healingOverrideRepo == nil {
		http.Error(w, "override repo not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	workflow := strings.TrimSpace(r.FormValue("workflow"))
	class := strings.TrimSpace(r.FormValue("class"))
	if project == "" || workflow == "" || class == "" {
		redirectOverrides(w, r, "project, workflow, and class are all required",
			project, workflow, class)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.healingOverrideRepo.Delete(ctx, project, workflow, persistence.HealingTriggerClass(class)); err != nil {
		if !errors.Is(err, persistence.ErrNotFound) {
			s.logger.Warn().Err(err).Msg("override delete failed")
			redirectOverrides(w, r, err.Error(), project, workflow, class)
			return
		}
	}
	if s.adminAuditRepo != nil {
		_ = s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Timestamp: time.Now().UTC(),
			Principal: adminPrincipal(r),
			Source:    "ui",
			Action:    "blackbox-override.deleted",
			Target:    project + "/" + workflow + "/" + class,
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}
	http.Redirect(w, r, "/ui/admin/blackbox/overrides", http.StatusSeeOther)
}

// redirectOverrides sends the operator back to the list page with
// the form values preserved and an action_error banner. Keeps the
// form-error UX consistent across save + delete.
func redirectOverrides(w http.ResponseWriter, r *http.Request, msg, project, workflow, class string) {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	q := "/ui/admin/blackbox/overrides?action_error=" + strings.ReplaceAll(msg, " ", "+")
	if project != "" {
		q += "&project=" + project
	}
	if workflow != "" {
		q += "&workflow=" + workflow
	}
	if class != "" {
		q += "&class=" + class
	}
	http.Redirect(w, r, q, http.StatusSeeOther)
}

// isValidClass mirrors the persistence enum check without forcing
// callers to import the constants. Kept in this file because the
// validation happens at the form boundary.
func isValidClass(s string) bool {
	for _, c := range classOptions() {
		if s == c {
			return true
		}
	}
	return false
}
