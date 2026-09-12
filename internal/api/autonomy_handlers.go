package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/autonomy"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ListAutonomyEvaluations handles
// GET /api/v1/projects/{projectId}/autonomy/evaluations.
//
// Returns per-tick evaluation rows newest-first, with optional
// ?outcome= filter. Unlike task lists this only reports the audit
// trail — to surface the final task status (COMPLETED / FAILED /
// CANCELLED), pair with GET /projects/{p}/tasks.
func (s *Server) ListAutonomyEvaluations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
	}
	if s.autonomyEvalRepo == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"evaluations": []persistence.AutonomyEvaluation{},
			"total":       0,
		})
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	filter := persistence.AutonomyEvaluationFilter{
		ProjectID: &projectID,
		PageSize:  limit,
	}
	if outcome := r.URL.Query().Get("outcome"); outcome != "" {
		filter.Outcome = &outcome
	}

	rows, err := s.autonomyEvalRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy evaluations list failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list evaluations")
		return
	}
	if rows == nil {
		rows = []*persistence.AutonomyEvaluation{}
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"evaluations": rows,
		"total":       len(rows),
	})
}

// GetAutonomyEvaluationSummary handles
// GET /api/v1/projects/{projectId}/autonomy/summary.
//
// Aggregates evaluation outcomes over the last N hours (default 24).
// Cheap read (one aggregate, bounded time range). Useful for the
// landing-page autonomy widget and for a vornikctl autonomy summary CLI.
func (s *Server) GetAutonomyEvaluationSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
	}
	if s.autonomyEvalRepo == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"projectId": projectID,
			"windowHrs": 24,
			"counts":    map[string]int64{},
		})
		return
	}

	windowHrs := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowHrs = n
		}
	}
	if windowHrs > 24*30 {
		windowHrs = 24 * 30 // cap at 30d to keep the aggregate cheap
	}

	since := time.Now().UTC().Add(-time.Duration(windowHrs) * time.Hour)
	counts, err := s.autonomyEvalRepo.CountByOutcome(r.Context(), projectID, since, time.Time{})
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy summary failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to aggregate evaluations")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"projectId": projectID,
		"windowHrs": windowHrs,
		"since":     since.Format(time.RFC3339),
		"counts":    counts,
	})
}

// Degradation-detection thresholds (design 2026-09-10-autonomy-degradation-
// detection-design.md §6). Named rather than inlined so a future tuning
// pass changes one number instead of hunting two literals.
const (
	// autonomyHealthMonotonyMinTicks is the smallest window CountByOutcome
	// is treated as evidence of a stuck outcome mix. Below this a single
	// CREATED tick would trivially read as "100% monotone".
	autonomyHealthMonotonyMinTicks = 10
	// autonomyHealthMonotonyThreshold is the share one outcome must hold
	// of the window's total to be flagged. The incident this endpoint
	// answers ran six days at 100% CREATED; 95% catches that shape
	// without flagging a healthy mix that happens to lean one way.
	autonomyHealthMonotonyThreshold = 0.95
	// autonomyHealthMaxDeliveryRows bounds the per-tick delivery/judge
	// resolution (task Get + GetChildren + verdict GetByTask, each a
	// round-trip). An unbounded fan-out over a 30-day window is a slow
	// query nobody asked for; "truncated" tells the caller when the cap
	// bit.
	autonomyHealthMaxDeliveryRows = 200
	// autonomyHealthFeedTaskPageSize mirrors the bound
	// buildStateContext already uses (manager.go) for the same
	// FeedObservations call, so the two call sites don't disagree about
	// how much history "recent" means.
	autonomyHealthFeedTaskPageSize = 50
)

// errAutonomyHealthNoTaskRepo is the guard for a Server wired with an
// autonomy eval repo but no task repository. Every other optional repo in
// this handler family (verdictRepo, executionRepo, stepOutcomeRepo,
// autonomyEvalRepo) is nil-checked; taskRepo was dereferenced bare in
// three places and would panic.
var errAutonomyHealthNoTaskRepo = errors.New("autonomy health: no task repository configured")

// autonomyFeedJSON is the wire shape of one FeedObservation row. Durations
// render as seconds (not Go's default nanosecond int) so the JSON is
// readable by both the vornikctl renderer and a human with curl.
type autonomyFeedJSON struct {
	Slug           string  `json:"slug"`
	CadenceSeconds float64 `json:"cadenceSeconds"`
	LagSeconds     float64 `json:"lagSeconds"`
	NeverRan       bool    `json:"neverRan"`
	// Unmeasured splits NeverRan in two. Both call sites bound history at
	// 50 tasks (~9.7 days at the shipped demand), so "no run found" means
	// EITHER the whole history was examined and this feed has genuinely
	// never run, OR the page came back full and older history was never
	// looked at. Rendering a feed 200 days overdue identically to one
	// that never ran is the "examined and clean" vs "never examined"
	// conflation this endpoint exists to catch, reproduced in its own
	// output — so the horizon travels with the observation.
	Unmeasured bool `json:"unmeasured"`
	// LagAtLeastSeconds is a proven LOWER BOUND, set only when
	// Unmeasured: nothing carrying the slug appears anywhere in the
	// examined window, so the last run is at least this old. Never a
	// measurement; renderers must mark it as a bound.
	LagAtLeastSeconds float64 `json:"lagAtLeastSeconds,omitempty"`
	// HorizonTasks / HorizonOldest publish the scope of the claim: how
	// many tasks were examined and how far back they reach. A number
	// with no scope reads as a guarantee (project rule 4).
	HorizonTasks  int    `json:"horizonTasks"`
	HorizonOldest string `json:"horizonOldest,omitempty"`
	// Breach is "slow", "fast", or "" — "" is a real observed non-breach,
	// distinct from NeverRan (no evidence at all). Never collapse the two.
	Breach string `json:"breach"`
}

func renderFeedObservations(obs []autonomy.FeedObservation) []autonomyFeedJSON {
	out := make([]autonomyFeedJSON, 0, len(obs))
	for _, o := range obs {
		row := autonomyFeedJSON{
			Slug:              o.Slug,
			CadenceSeconds:    o.Cadence.Seconds(),
			LagSeconds:        o.Lag.Seconds(),
			NeverRan:          o.NeverRan,
			Unmeasured:        o.Unmeasured(),
			LagAtLeastSeconds: o.LagAtLeast.Seconds(),
			HorizonTasks:      o.Horizon.Tasks,
			Breach:            o.Breach,
		}
		if !o.Horizon.Oldest.IsZero() {
			row.HorizonOldest = o.Horizon.Oldest.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// autonomyDeliveryRow is one CREATED tick's resolved delivery: what the
// tick actually produced, following the delegation edge to the child that
// did the work rather than reporting the router's own terminal status.
type autonomyDeliveryRow struct {
	TaskID string `json:"taskId"`
	// ChildTaskID is set only when the task delegated (a child with a
	// non-nil DelegationMode exists). Omitted, not null, when it did not
	// — a non-delegating tick has no child to name.
	ChildTaskID *string `json:"childTaskId,omitempty"`
	// Status is the CHILD's terminal status when delegated, otherwise the
	// parent's own terminal status.
	Status string `json:"status"`
	// NoDelegation marks a tick whose task concluded without ever
	// creating a delegated child — a routing guard or malformed plan.
	// Without this flag such a row hides inside the same CREATED->
	// COMPLETED shape as a healthy delegating tick.
	NoDelegation bool `json:"noDelegation"`
}

// buildAutonomyOutcomesBlock aggregates CountByOutcome's per-outcome map
// into the outcomes block, adding the monotony flag only when it fires —
// absence, not false, is the "not evidence of a problem" state.
func buildAutonomyOutcomesBlock(counts map[string]int64) map[string]any {
	var total int64
	for _, n := range counts {
		total += n
	}
	block := map[string]any{"counts": counts, "total": total}
	if total >= autonomyHealthMonotonyMinTicks {
		for _, n := range counts {
			if float64(n) >= autonomyHealthMonotonyThreshold*float64(total) {
				block["monotony"] = true
				break
			}
		}
	}
	return block
}

// fetchCreatedTicksInWindow loads the up-to-(cap+1) most recent CREATED
// evaluation rows and filters them to [since, now). List returns newest
// first, so the newest (cap+1) CREATED rows overall necessarily include
// every in-window row up to that count — whether the (cap+1)th survives
// the window filter is exactly what "truncated" reports.
func (s *Server) fetchCreatedTicksInWindow(
	ctx context.Context, projectID string, since time.Time,
) ([]*persistence.AutonomyEvaluation, bool, error) {
	createdOutcome := persistence.AutonomyOutcomeCreated
	evalRows, err := s.autonomyEvalRepo.List(ctx, persistence.AutonomyEvaluationFilter{
		ProjectID: &projectID,
		Outcome:   &createdOutcome,
		PageSize:  autonomyHealthMaxDeliveryRows + 1,
	})
	if err != nil {
		return nil, false, err
	}
	inWindow := make([]*persistence.AutonomyEvaluation, 0, len(evalRows))
	for _, e := range evalRows {
		if e == nil || e.TaskID == nil || e.CreatedAt.Before(since) {
			continue
		}
		inWindow = append(inWindow, e)
	}
	truncated := len(inWindow) > autonomyHealthMaxDeliveryRows
	if truncated {
		inWindow = inWindow[:autonomyHealthMaxDeliveryRows]
	}
	return inWindow, truncated, nil
}

// resolveDeliveryRow follows the delegation edge for one CREATED tick's
// task: when a delegated child exists (DelegationMode != nil), the row and
// the resolved task id used for judge/churn lookups become the CHILD's;
// otherwise the parent's own status stands and the row is marked
// noDelegation. Returns an error when GetChildren itself fails — the
// caller skips the row rather than defaulting to noDelegation:true, which
// would assert "this tick never delegated" when the true answer is
// "unknown, the children query failed".
func (s *Server) resolveDeliveryRow(ctx context.Context, task *persistence.Task) (autonomyDeliveryRow, string, error) {
	row := autonomyDeliveryRow{TaskID: task.ID, Status: string(task.Status), NoDelegation: true}
	resolvedTaskID := task.ID

	// Guarded like every other optional repo in this file. Returning the
	// error (rather than the row) keeps the caller's "unresolved" path:
	// with no task repo we cannot say whether this tick delegated, and
	// noDelegation:true would assert that it did not.
	if s.taskRepo == nil {
		return row, resolvedTaskID, errAutonomyHealthNoTaskRepo
	}
	children, childErr := s.taskRepo.GetChildren(ctx, task.ID)
	if childErr != nil {
		return row, resolvedTaskID, childErr
	}
	for _, c := range children {
		if c == nil || c.DelegationMode == nil {
			continue
		}
		childID := c.ID
		row.ChildTaskID = &childID
		row.Status = string(c.Status)
		row.NoDelegation = false
		resolvedTaskID = c.ID
		// One row represents one tick. A FAN_OUT parent can have several
		// delegated children; the first stands in for the tick rather
		// than exploding one tick into N rows, which would need a schema
		// change beyond this task's scope.
		break
	}
	return row, resolvedTaskID, nil
}

// executionChurnAccumulator tallies the per-task execution counts
// resolveAutonomyDelivery folds into the routeChurn block.
type executionChurnAccumulator struct {
	totalExecutions      int64
	tasksWithExecution   int64
	maxExecutionsForTask int64
}

func (a *executionChurnAccumulator) observe(n int64) {
	a.totalExecutions += n
	if n > 0 {
		a.tasksWithExecution++
	}
	if n > a.maxExecutionsForTask {
		a.maxExecutionsForTask = n
	}
}

// tallyDelivery records the judge verdict and execution count for whichever
// task actually delivered (resolvedTaskID — the child's id when delegated).
//
// A verdict lookup returning persistence.ErrNotFound is the legitimate "not
// judged yet" case (TaskJudgeVerdictRepository's own contract) and is not
// counted as unresolved. Any OTHER error from either lookup — a backend
// timeout, say — is logged and counted: silently folding it into "no
// verdict"/"no executions" is indistinguishable from the genuine case, which
// is exactly the "examined and clean" vs "never examined" conflation this
// endpoint exists to catch, reproduced inside the detector itself.
func (s *Server) tallyDelivery(
	ctx context.Context, projectID, resolvedTaskID string, judgeCounts map[string]int64, acc *executionChurnAccumulator, unresolved *int64,
) {
	if s.verdictRepo != nil {
		v, vErr := s.verdictRepo.GetByTask(ctx, resolvedTaskID)
		switch {
		case vErr == nil && v != nil:
			judgeCounts[v.Verdict]++
		case errors.Is(vErr, persistence.ErrNotFound):
			// Not judged (yet). Not a failure.
		default:
			*unresolved++
			s.logger.Warn().Err(vErr).Str("project_id", projectID).Str("task_id", resolvedTaskID).
				Msg("autonomy health: judge verdict lookup failed")
		}
	}
	if s.executionRepo != nil {
		execs, execErr := s.executionRepo.List(ctx, persistence.ExecutionFilter{TaskID: &resolvedTaskID})
		if execErr != nil {
			*unresolved++
			s.logger.Warn().Err(execErr).Str("project_id", projectID).Str("task_id", resolvedTaskID).
				Msg("autonomy health: execution list failed")
			return
		}
		acc.observe(int64(len(execs)))
	}
}

// buildAutonomyJudgeBlock is the honesty rule for the judge column: no
// verdict repo wired, or zero verdicts resolved in the window, both render
// declared:false — never a fabricated 0% failure rate.
func buildAutonomyJudgeBlock(wired bool, counts map[string]int64) map[string]any {
	if !wired {
		return map[string]any{"declared": false}
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return map[string]any{"declared": false}
	}
	return map[string]any{
		"declared": true,
		"counts":   counts,
		"total":    total,
		"failRate": float64(counts[persistence.JudgeVerdictFail]) / float64(total),
	}
}

// buildAutonomyRouteChurnBlock reports executions-per-task and orphaned
// step outcomes for the window (design §6). orphanedStepOutcomes is one
// aggregate query (CountByRoleModelOutcome, outcome="orphaned" — see
// https://docs.vornik.io), not a
// per-task fan-out. maxExecutionsForTask stands in for "corrective
// re-runs": the worst single task in this window, absent a dedicated
// re-run classifier.
func (s *Server) buildAutonomyRouteChurnBlock(
	ctx context.Context, projectID string, since time.Time, tasksObserved int, acc executionChurnAccumulator,
) map[string]any {
	var executionsPerTask float64
	if tasksObserved > 0 {
		executionsPerTask = float64(acc.totalExecutions) / float64(tasksObserved)
	}
	block := map[string]any{
		"tasksObserved":        int64(tasksObserved),
		"tasksWithExecution":   acc.tasksWithExecution,
		"executionsPerTask":    executionsPerTask,
		"maxExecutionsForTask": acc.maxExecutionsForTask,
	}
	// orphanedStepOutcomes is PRESENT only when it was actually counted.
	// Its four siblings in this function family were fixed to feed
	// `unresolved` rather than swallow a lookup failure; this one used to
	// swallow the error and render 0, i.e. "examined and clean" meaning
	// "never examined" — the exact conflation this endpoint exists to
	// catch. An absent key renders "not measured" in the CLI (see
	// renderHealthRouteChurn, which checks presence, not value), so the
	// no-repo and failed-query cases can never read as a clean zero.
	if s.stepOutcomeRepo == nil {
		return block
	}
	counts, err := s.stepOutcomeRepo.CountByRoleModelOutcome(ctx, "orphaned", since, time.Time{}, projectID)
	if err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).
			Msg("autonomy health: orphaned step-outcome count failed, reporting it as not measured")
		return block
	}
	var orphanedStepOutcomes int64
	for _, c := range counts {
		orphanedStepOutcomes += c.Count
	}
	block["orphanedStepOutcomes"] = orphanedStepOutcomes
	return block
}

// resolveAutonomyDelivery loads the up-to-N most recent CREATED evaluation
// rows in [since, now), resolves each to its delivering task across the
// delegation edge, and tallies the judge verdict and execution-count churn
// for whichever task delivered. One function because the three blocks
// (delivery, judge, routeChurn) all key off the same bounded task
// resolution — computing them separately would mean walking the same
// capped task set three times over three more round-trips each.
//
// unresolved counts every CREATED tick this pass could NOT examine (a task
// lookup, children lookup, verdict lookup, or execution lookup that errored
// — never "no data found", which is the honest zero, only "the lookup
// itself failed"). It is surfaced in the response as delivery.unresolved so
// an operator can tell "N ticks exist but could not be resolved" from "the
// window legitimately had fewer" — the distinction this endpoint exists to
// draw, which a silent `continue` would erase right back into.
func (s *Server) resolveAutonomyDelivery(
	ctx context.Context, projectID string, since time.Time,
) (rows []autonomyDeliveryRow, truncated bool, judge map[string]any, churn map[string]any, unresolved int64, err error) {
	inWindow, truncated, err := s.fetchCreatedTicksInWindow(ctx, projectID, since)
	if err != nil {
		return nil, false, nil, nil, 0, err
	}

	judgeCounts := map[string]int64{}
	var acc executionChurnAccumulator

	// No task repo: nothing in the window can be examined at all. Each
	// in-window tick counts as unresolved rather than silently producing
	// an empty delivery table that reads as "the window was quiet".
	if s.taskRepo == nil {
		unresolved = int64(len(inWindow))
		s.logger.Warn().Str("project_id", projectID).Int("ticks", len(inWindow)).
			Msg("autonomy health: no task repository wired, delivery not examined")
		return nil, truncated, buildAutonomyJudgeBlock(false, nil),
			s.buildAutonomyRouteChurnBlock(ctx, projectID, since, 0, acc), unresolved, nil
	}

	for _, e := range inWindow {
		task, getErr := s.taskRepo.Get(ctx, *e.TaskID)
		if getErr != nil || task == nil {
			unresolved++
			s.logger.Warn().Err(getErr).Str("project_id", projectID).Str("task_id", derefStrOrEmpty(e.TaskID)).
				Msg("autonomy health: could not load CREATED tick's task")
			continue
		}
		row, resolvedTaskID, rowErr := s.resolveDeliveryRow(ctx, task)
		if rowErr != nil {
			unresolved++
			s.logger.Warn().Err(rowErr).Str("project_id", projectID).Str("task_id", task.ID).
				Msg("autonomy health: could not load task's children")
			continue
		}
		rows = append(rows, row)
		s.tallyDelivery(ctx, projectID, resolvedTaskID, judgeCounts, &acc, &unresolved)
	}

	judge = buildAutonomyJudgeBlock(s.verdictRepo != nil, judgeCounts)
	churn = s.buildAutonomyRouteChurnBlock(ctx, projectID, since, len(rows), acc)
	return rows, truncated, judge, churn, unresolved, nil
}

// derefStrOrEmpty is a small nil-safe dereference for log fields — the
// AutonomyEvaluation rows this function walks always carry a non-nil TaskID
// (fetchCreatedTicksInWindow already filters out the nil ones), but a log
// helper should not itself risk a nil-pointer panic on a future caller.
func derefStrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// respondAutonomyHealthNotEvaluated is GetAutonomyHealth's nil-repo early
// return: with no autonomyEvalRepo wired, nothing has been evaluated at
// all, so every block is "never examined" rather than "examined and
// clean" — reported as such instead of mixing in a partial computation
// from whatever other repos happen to be wired.
func respondAutonomyHealthNotEvaluated(w http.ResponseWriter, projectID string) {
	respondJSON(w, http.StatusOK, map[string]any{
		"projectId":     projectID,
		"windowHrs":     24,
		"outcomes":      map[string]any{"counts": map[string]int64{}, "total": int64(0)},
		"feedsDeclared": false,
		"feeds":         nil,
		"delivery":      map[string]any{"rows": []autonomyDeliveryRow{}, "truncated": false, "unresolved": int64(0)},
		"judge":         map[string]any{"declared": false},
		"routeChurn":    map[string]any{},
	})
}

// resolveAutonomyFeeds computes GetAutonomyHealth's feeds block. A project
// resolving no feeds (nil project, or Autonomy.Feeds empty) renders
// declared=false / feeds=nil — the honesty rule: never an empty-but-
// present list that a caller could mistake for "declared and healthy".
func (s *Server) resolveAutonomyFeeds(ctx context.Context, project *registry.Project, projectID string) (bool, any, error) {
	resolvedFeeds := project.ResolveFeeds() // nil-safe: nil project or empty Autonomy.Feeds both resolve to nil
	if len(resolvedFeeds) == 0 {
		return false, nil, nil
	}
	// Guarded like verdictRepo/executionRepo/stepOutcomeRepo below: a
	// Server wired with an eval repo and no task repo would otherwise
	// panic here. There is no honest partial answer for this block —
	// every cadence number comes from the task list — so this errors
	// rather than returning an empty observation set that would render
	// as "declared, nothing observed".
	if s.taskRepo == nil {
		return false, nil, errAutonomyHealthNoTaskRepo
	}
	tasks, err := s.taskRepo.List(ctx, persistence.TaskFilter{
		ProjectID: &projectID,
		PageSize:  autonomyHealthFeedTaskPageSize,
	})
	if err != nil {
		return false, nil, err
	}
	obs := autonomy.FeedObservations(resolvedFeeds, tasks, autonomyHealthFeedTaskPageSize, time.Now().UTC())
	return true, renderFeedObservations(obs), nil
}

// GetAutonomyHealth handles GET /api/v1/projects/{projectId}/autonomy/health.
//
// Renders the degradation table an operator would otherwise assemble by
// hand from blackbox traces (design 2026-09-10-autonomy-degradation-
// detection-design.md §6): outcome-mix monotony, feed cadence adherence
// against DECLARED feeds only, delivery resolved across the delegation
// edge (a CREATED tick's own status is the router's, not the work's), the
// judge's verdict on whichever task actually delivered, and route churn
// (executions per task, orphaned step outcomes) for the same window.
//
// Honesty rule, load-bearing: a project that declares no feeds reports
// feedsDeclared:false and feeds:null — never a blank that reads as OK. A
// window with no judge verdicts reports judge.declared:false — never a 0%
// failure rate. Both are the "examined and clean" vs "never examined"
// distinction (project rule 4) this endpoint exists to stop conflating.
func (s *Server) GetAutonomyHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	var project *registry.Project
	if s.projectRegistry != nil {
		p := s.projectRegistry.GetProject(projectID)
		if p == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
		project = p
	}
	if s.autonomyEvalRepo == nil {
		respondAutonomyHealthNotEvaluated(w, projectID)
		return
	}

	windowHrs := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			windowHrs = n
		}
	}
	if windowHrs > 24*30 {
		windowHrs = 24 * 30 // cap at 30d to keep the aggregate cheap
	}

	ctx := r.Context()
	since := time.Now().UTC().Add(-time.Duration(windowHrs) * time.Hour)

	rawCounts, err := s.autonomyEvalRepo.CountByOutcome(ctx, projectID, since, time.Time{})
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy health: outcome count failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to aggregate evaluations")
		return
	}
	outcomes := buildAutonomyOutcomesBlock(rawCounts)

	feedsDeclared, feedsJSON, err := s.resolveAutonomyFeeds(ctx, project, projectID)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy health: task list for feed lag failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load tasks for feed lag")
		return
	}

	deliveryRows, truncated, judge, churn, unresolved, err := s.resolveAutonomyDelivery(ctx, projectID, since)
	if err != nil {
		s.logger.Error().Err(err).Str("project_id", projectID).Msg("autonomy health: delivery resolution failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to resolve delivery")
		return
	}
	if deliveryRows == nil {
		deliveryRows = []autonomyDeliveryRow{}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"projectId":     projectID,
		"windowHrs":     windowHrs,
		"since":         since.Format(time.RFC3339),
		"outcomes":      outcomes,
		"feedsDeclared": feedsDeclared,
		"feeds":         feedsJSON,
		"delivery": map[string]any{
			"rows":      deliveryRows,
			"truncated": truncated,
			// unresolved: CREATED ticks a lookup failure kept this pass from
			// examining -- distinct from a legitimately smaller window. See
			// resolveAutonomyDelivery's doc comment.
			"unresolved": unresolved,
		},
		"judge":      judge,
		"routeChurn": churn,
	})
}
