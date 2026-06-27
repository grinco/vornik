package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Bulk CSV export endpoints. Three resources:
//
//   GET /api/v1/projects/{p}/tasks.csv?status=...&from=...&to=...&limit=...
//   GET /api/v1/projects/{p}/audit.csv?execution_id=...&tool=...&limit=...
//   GET /api/v1/projects/{p}/spend.csv?from=...&to=...&limit=...
//
// Useful for monthly reviews, data-lake ingestion, and "give me
// every task that ran during the rate-limit incident yesterday".
//
// Output format:
//   - Content-Type: text/csv
//   - Content-Disposition: attachment; filename="<resource>-<project>-<YYYYMMDD>.csv"
//   - First row: column headers
//   - All cells CSV-escaped via encoding/csv (handles commas + quotes + newlines)
//   - Hard cap at 10,000 rows per export — operators wanting more
//     run the export in pages or query the DB directly. Prevents
//     a single export from monopolising the daemon.

const csvMaxRows = 10000

// parseCSVTimeRange resolves the optional from/to query params.
// Returns zero times when unset. Invalid values are silently
// ignored (so a copy-pasted half-formed URL doesn't 400).
func parseCSVTimeRange(r *http.Request) (from, to time.Time) {
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		} else if d, err := time.ParseDuration(v); err == nil {
			// Accept "-24h" / "-7d" relative form.
			from = time.Now().Add(d)
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	return from, to
}

func parseCSVLimit(r *http.Request) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return csvMaxRows
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return csvMaxRows
	}
	if n > csvMaxRows {
		return csvMaxRows
	}
	return n
}

// writeCSVHeaders sets the response shape every CSV export
// shares — content-type, attachment filename, no-cache.
func writeCSVHeaders(w http.ResponseWriter, resource, projectID string) {
	filename := fmt.Sprintf("%s-%s-%s.csv", resource, projectID, time.Now().UTC().Format("20060102"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
}

// ExportTasksCSV handles GET /api/v1/projects/{p}/tasks.csv.
func (s *Server) ExportTasksCSV(w http.ResponseWriter, r *http.Request) {
	if s.taskRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "EXPORT_DISABLED", "task repo not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId required")
		return
	}
	limit := parseCSVLimit(r)
	filter := persistence.TaskFilter{
		ProjectID: &projectID,
		PageSize:  limit,
	}
	if v := r.URL.Query().Get("status"); v != "" {
		st := persistence.TaskStatus(v)
		filter.Status = &st
	}
	tasks, err := s.taskRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Msg("ExportTasksCSV: list failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list tasks")
		return
	}

	// Optional client-side time-range trim. We don't push from/to
	// into the filter because TaskFilter doesn't have them; the
	// caller pays a small list+filter cost in exchange for a
	// uniform export shape.
	from, to := parseCSVTimeRange(r)
	rows := make([]*persistence.Task, 0, len(tasks))
	for _, t := range tasks {
		if !from.IsZero() && t.CreatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && t.CreatedAt.After(to) {
			continue
		}
		rows = append(rows, t)
	}

	writeCSVHeaders(w, "tasks", projectID)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"id", "project_id", "workflow_id", "creation_source", "status",
		"priority", "attempt", "max_attempts",
		"current_phase", "expected_by", "closed_at", "closed_by", "message_count",
		"last_error_class", "created_at", "updated_at",
	})
	for _, t := range rows {
		_ = cw.Write([]string{
			t.ID,
			t.ProjectID,
			strPtrOrEmpty(t.WorkflowID),
			string(t.CreationSource),
			string(t.Status),
			strconv.Itoa(t.Priority),
			strconv.Itoa(t.Attempt),
			strconv.Itoa(t.MaxAttempts),
			strPtrOrEmpty(t.CurrentPhase),
			timePtrOrEmpty(t.ExpectedBy),
			timePtrOrEmpty(t.ClosedAt),
			strPtrOrEmpty(t.ClosedBy),
			strconv.Itoa(t.MessageCount),
			strPtrOrEmpty(t.LastErrorClass),
			t.CreatedAt.UTC().Format(time.RFC3339),
			t.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
}

// ExportAuditCSV handles GET /api/v1/projects/{p}/audit.csv.
func (s *Server) ExportAuditCSV(w http.ResponseWriter, r *http.Request) {
	if s.toolAuditRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "EXPORT_DISABLED", "audit repo not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId required")
		return
	}
	limit := parseCSVLimit(r)
	filter := persistence.ToolAuditFilter{
		ProjectID: &projectID,
		PageSize:  limit,
	}
	if v := r.URL.Query().Get("execution_id"); v != "" {
		filter.ExecutionID = &v
	}
	if v := r.URL.Query().Get("tool"); v != "" {
		filter.ToolName = &v
	}
	entries, err := s.toolAuditRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Msg("ExportAuditCSV: list failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list audit")
		return
	}

	from, to := parseCSVTimeRange(r)
	writeCSVHeaders(w, "audit", projectID)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"id", "task_id", "execution_id", "step_id", "tool_name",
		"duration_ms", "tool_input", "tool_output", "created_at",
	})
	for _, e := range entries {
		if !from.IsZero() && e.CreatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && e.CreatedAt.After(to) {
			continue
		}
		_ = cw.Write([]string{
			e.ID,
			e.TaskID,
			e.ExecutionID,
			e.StepID,
			e.ToolName,
			strconv.FormatInt(e.DurationMs, 10),
			truncateForCSV(e.ToolInput),
			truncateForCSV(e.ToolOutput),
			e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
}

// ExportSpendCSV handles GET /api/v1/projects/{p}/spend.csv.
// Pulls from llm_usage repo if wired; otherwise 503.
func (s *Server) ExportSpendCSV(w http.ResponseWriter, r *http.Request) {
	if s.llmUsageRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "EXPORT_DISABLED", "llm usage repo not configured")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId required")
		return
	}
	limit := parseCSVLimit(r)
	from, to := parseCSVTimeRange(r)
	if from.IsZero() {
		from = time.Now().Add(-30 * 24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now()
	}
	filter := persistence.TaskLLMUsageFilter{
		ProjectID: &projectID,
		Since:     &from,
		Until:     &to,
		PageSize:  limit,
	}
	rows, err := s.llmUsageRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Msg("ExportSpendCSV: list failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list spend")
		return
	}

	writeCSVHeaders(w, "spend", projectID)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"task_id", "execution_id", "step_id", "role", "model", "source",
		"prompt_tokens", "completion_tokens", "iterations",
		"cost_usd", "recorded_at",
	})
	for _, u := range rows {
		_ = cw.Write([]string{
			strPtrOrEmpty(u.TaskID),
			strPtrOrEmpty(u.ExecutionID),
			u.StepID,
			u.Role,
			u.Model,
			u.Source,
			strconv.FormatInt(u.PromptTokens, 10),
			strconv.FormatInt(u.CompletionTokens, 10),
			strconv.Itoa(u.Iterations),
			strconv.FormatFloat(u.CostUSD, 'f', 6, 64),
			u.RecordedAt.UTC().Format(time.RFC3339),
		})
	}
}

// strPtrOrEmpty + timePtrOrEmpty + truncateForCSV are tiny
// helpers shared by every CSV row-emit.
func strPtrOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func timePtrOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// truncateForCSV caps tool_input / tool_output at 4 KB per cell so
// a single audit row with a megabyte of stdout can't blow past
// Excel's 32k-per-cell limit + balloon the export.
func truncateForCSV(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "…[truncated]"
}

// _ guards: keep gofmt-imports happy if the strings import gets
// removed by a future edit.
var _ = strings.Builder{}
