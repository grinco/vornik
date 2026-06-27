package api

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
)

// rateLimitStatusResponse mirrors the JSON contract for
// GET /api/v1/projects/{id}/ratelimit-status. Operators consume
// this from the UI homepage panel and the vornikctl CLI; field names
// stay snake_case for consistency with the rest of the API surface.
type rateLimitStatusResponse struct {
	ProjectID string `json:"project_id"`

	// TaskCreation summarises the in-process project task-creation
	// limiter (sliding-window). Zero counts when no tasks have been
	// recorded in the trailing minute / hour, regardless of whether
	// the project has a cap configured.
	TaskCreation taskCreationStatus `json:"task_creation"`

	// Keys is one entry per non-revoked, non-expired API key on
	// this project. Empty when the project has no active keys or
	// the apiKeyRepo isn't wired. The order is repo-natural —
	// callers that want stable ordering should sort by Name.
	Keys []apiKeyStatus `json:"keys"`

	// Project summary derived from the metric event ring — counts
	// warn-and-block events on the ScopeProject id over the
	// trailing StatusWindow. Drives the "approaching limit"
	// homepage banner together with each key's RecentWarns.
	ProjectSummary scopeSummary `json:"project_summary"`

	// WindowSeconds is the recent-warn / recent-block lookback in
	// seconds. Surfaced so the UI can render
	// "warned 4 times in last 5 min" without hard-coding the
	// duration on the front end.
	WindowSeconds int `json:"window_seconds"`
}

// taskCreationStatus is the per-project sliding-window snapshot:
// current trailing-minute / trailing-hour counts paired with the
// configured caps (zero meaning "no cap"). Headroom is computed
// here so the UI template stays dumb.
type taskCreationStatus struct {
	MinuteCount    int `json:"minute_count"`
	MinuteCap      int `json:"minute_cap"`
	MinuteHeadroom int `json:"minute_headroom"` // cap - count, clamped at zero; -1 when cap == 0 ("unlimited")
	HourCount      int `json:"hour_count"`
	HourCap        int `json:"hour_cap"`
	HourHeadroom   int `json:"hour_headroom"`
}

// apiKeyStatus mirrors the per-key bucket level + recent warn / block
// activity. Tokens float through as-is (the bucket arithmetic is
// fractional) so the UI can render "7.3 / 10" without re-doing the
// refill math.
type apiKeyStatus struct {
	KeyID           string       `json:"key_id"`
	Name            string       `json:"name"`
	KeyPrefix       string       `json:"key_prefix"`
	RateLimitRPS    int          `json:"rate_limit_rps"`             // 0 == unlimited
	RateLimitBurst  int          `json:"rate_limit_burst"`           // 0 == unlimited
	TokensRemaining *float64     `json:"tokens_remaining,omitempty"` // nil when no bucket has been allocated yet
	Summary         scopeSummary `json:"summary"`
}

// scopeSummary surfaces the trailing-window degradation signal for
// one scope+id pair. Last429At is nullable: zero IsZero ⇒ no 429
// inside the window ⇒ omit from JSON.
type scopeSummary struct {
	RecentWarns  int        `json:"recent_warns"`
	RecentBlocks int        `json:"recent_blocks"`
	Last429At    *time.Time `json:"last_429_at,omitempty"`
}

// GetProjectRateLimitStatus handles
// GET /api/v1/projects/{projectId}/ratelimit-status.
//
// Best-effort read-only surface for the operator UI homepage panel
// and CLI. Never returns 500 on missing collaborators — degrades to
// zero counts so the UI keeps rendering. Returns 404 when the
// project doesn't exist; everything else is 200 with the appropriate
// fields zeroed.
func (s *Server) GetProjectRateLimitStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	var (
		minuteCap int
		hourCap   int
	)
	if s.projectRegistry != nil {
		project := s.projectRegistry.GetProject(projectID)
		if project == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
		minuteCap = project.RateLimit.TasksPerMinute
		hourCap = project.RateLimit.TasksPerHour
	}

	resp := rateLimitStatusResponse{
		ProjectID:     projectID,
		WindowSeconds: int(ratelimit.StatusWindow / time.Second),
	}
	// Surfacing the trailing-window snapshot is an in-process
	// concept (the postgres backend stores aggregate counts in SQL,
	// not the raw timestamp log). Type-assert to the concrete
	// *ratelimit.Limiter for now; postgres-backed deployments lose
	// the snapshot but still see configured caps + recent-event
	// counts from Metrics — the homepage banner stays accurate.
	var inProcessLimiter *ratelimit.Limiter
	if s.rateLimiter != nil {
		inProcessLimiter, _ = s.rateLimiter.(*ratelimit.Limiter)
	}
	resp.TaskCreation = buildTaskCreationStatus(inProcessLimiter, projectID, minuteCap, hourCap, time.Now())
	resp.ProjectSummary = scopeSummaryFor(s.rateLimitMetrics, ratelimit.ScopeProject, projectID)
	resp.Keys = buildAPIKeyStatuses(r.Context(), s.apiKeyRepo, s.apiKeyLimiter, s.rateLimitMetrics, projectID)
	respondJSON(w, http.StatusOK, resp)
}

// buildTaskCreationStatus packages the per-project sliding-window
// snapshot + configured caps into the JSON shape. Pure function so
// it's trivially unit-testable; the handler injects the limiter
// + caps and gets a fully-rendered struct back.
func buildTaskCreationStatus(l *ratelimit.Limiter, projectID string, minuteCap, hourCap int, now time.Time) taskCreationStatus {
	out := taskCreationStatus{MinuteCap: minuteCap, HourCap: hourCap}
	if l != nil && projectID != "" {
		if snap, ok := l.SnapshotFor(projectID, now); ok {
			out.MinuteCount = snap.MinuteCount
			out.HourCount = snap.HourCount
		}
	}
	out.MinuteHeadroom = headroom(out.MinuteCount, out.MinuteCap)
	out.HourHeadroom = headroom(out.HourCount, out.HourCap)
	return out
}

// headroom returns cap - count clamped at zero. A zero cap means
// "no limit configured" and surfaces as -1 so the UI can render
// "unlimited" rather than "0 remaining".
func headroom(count, cap int) int {
	if cap == 0 {
		return -1
	}
	h := cap - count
	if h < 0 {
		return 0
	}
	return h
}

// buildAPIKeyStatuses walks every non-revoked, non-expired key on
// the project, joining the persisted nominal limit (rate_limit_rps
// / rate_limit_burst) with the live bucket level and recent
// warn/block counts. Repo errors degrade to an empty slice so the
// rest of the response stays consistent.
func buildAPIKeyStatuses(
	ctx context.Context,
	repo persistence.APIKeyRepository,
	limiter *ratelimit.APIKeyLimiter,
	metrics *ratelimit.Metrics,
	projectID string,
) []apiKeyStatus {
	if repo == nil || projectID == "" {
		return nil
	}
	keys, err := repo.ListByProject(ctx, projectID)
	if err != nil {
		return nil
	}
	now := time.Now()
	out := make([]apiKeyStatus, 0, len(keys))
	for _, k := range keys {
		if k == nil {
			continue
		}
		if k.RevokedAt != nil {
			continue
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
			continue
		}
		row := apiKeyStatus{
			KeyID:     k.ID,
			Name:      k.Name,
			KeyPrefix: k.KeyPrefix,
		}
		if k.RateLimitRPS != nil {
			row.RateLimitRPS = *k.RateLimitRPS
		}
		if k.RateLimitBurst != nil {
			row.RateLimitBurst = *k.RateLimitBurst
		}
		if limiter != nil {
			if snap, ok := limiter.SnapshotFor(k.ID); ok {
				v := snap.Tokens
				row.TokensRemaining = &v
			}
		}
		row.Summary = scopeSummaryFor(metrics, ratelimit.ScopeAPIKey, k.ID)
		out = append(out, row)
	}
	return out
}

// scopeSummaryFor packages the metric ring's per-scope snapshot
// into the JSON shape. nil metrics is the "ratelimit metrics not
// wired" path — degrades to the zero summary.
func scopeSummaryFor(m *ratelimit.Metrics, scope, id string) scopeSummary {
	if m == nil || scope == "" || id == "" {
		return scopeSummary{}
	}
	s := m.StatusFor(scope, id)
	out := scopeSummary{
		RecentWarns:  s.RecentWarns,
		RecentBlocks: s.RecentBlocks,
	}
	if !s.LastBlockAt.IsZero() {
		t := s.LastBlockAt
		out.Last429At = &t
	}
	return out
}
