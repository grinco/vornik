// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

type ExecutionDetailData struct {
	Title       string
	CurrentPage string
	Execution   *persistence.Execution
	Task        *persistence.Task
	Workflow    *registry.Workflow
	Outcomes    []StepOutcomeRow
	// TotalDurationMS is the sum of step durations across Outcomes — used
	// by the inline timeline strip to scale each step's width. Zero means
	// the timeline doesn't render (no duration data).
	TotalDurationMS int64
	// OutcomeByStep is a step_id → outcome lookup so the Step Progress
	// rows can find their corresponding outcome row (duration display,
	// outcome class) without nested-ranging the Outcomes slice O(N²).
	OutcomeByStep map[string]StepOutcomeRow
	// MaxStepDurationMS is the longest single-step duration across the
	// outcome list. Used as the divisor for per-step bar widths so
	// the longest step renders at 100% and shorter steps scale
	// linearly. Zero means no duration data — bars don't render.
	MaxStepDurationMS int64
	// StepColorByID maps each unique step_id to a CSS color string. The
	// timeline strip and Step Progress cards both look up here so a
	// segment in the timeline visually matches its progress row by
	// color. Colors are picked deterministically from a curated palette
	// in step-discovery order so the same step gets the same color
	// across page reloads (and across executions of the same workflow,
	// when the step IDs match). Outcome color stays a separate signal
	// — see OutcomeClass on each row.
	StepColorByID map[string]string
	// StepColorOrder is the in-order step list used to render the
	// legend below the timeline. Same data StepColorByID exposes, but
	// preserves insertion order for stable display.
	StepColorOrder []StepColorEntry
	// Routing carries the strict-adaptive workflow router's
	// decision when this execution ran the `adaptive` workflow.
	// Nil for non-adaptive executions or for adaptive runs where
	// the lead didn't emit `selected_workflow` (legacy free-form
	// path or routing failure). Surfaces in a dedicated UI panel
	// so the operator can see WHICH workflow the lead picked
	// without drilling into the child task.
	Routing *RoutingDecision
}

// RoutingDecision is the per-execution snapshot of the strict
// adaptive router's choice. ChildTaskID is empty when the route
// step succeeded but the child-task delegation hasn't completed
// (rare — the executor creates the child synchronously after
// parsing the result), so the UI tolerates it.
type RoutingDecision struct {
	// SelectedWorkflow is the workflow_id the lead requested.
	// Equals the actually-delegated workflow when the choice
	// passed validation; otherwise the executor logs a warn and
	// falls back to project default — that fallback is invisible
	// here because the parent's result.json carries only what
	// the lead said, not what the executor did. The child task's
	// own workflow_id (rendered separately when ChildTaskID is
	// resolved) is the authoritative record.
	SelectedWorkflow string
	// Reason is the lead's one-sentence justification, surfaced
	// to the operator so the routing decision is reviewable.
	// Empty when the lead omitted the field.
	Reason string
	// ChildTaskID and ChildWorkflowID identify the actual
	// child task spawned to run the chosen workflow. Empty
	// when no child has been created yet (parent still in the
	// route step or routing failed before delegation).
	ChildTaskID     string
	ChildWorkflowID string
}

// StepColorEntry pairs a step ID with its assigned color for the
// timeline legend. Color is template.CSS so html/template trusts
// it inside `style="background: ..."` attributes — a plain string
// would be auto-escaped to ZgotmplZ by the CSS-context sanitizer.
type StepColorEntry struct {
	StepID string
	Color  template.CSS
}

// StepOutcomeRow is the per-step outcome entry rendered in the
// execution detail page. A thin projection of ExecutionStepOutcome with
// a few display-ready helpers.
type StepOutcomeRow struct {
	StepID             string
	Role               string
	Model              string
	Outcome            string
	AttributedToStepID string
	ErrorClass         string
	ErrorDetail        string
	DurationMS         int64
	DurationDisplay    string // "123ms" / "1.4s" / "—"
	// OutcomeClass is a CSS-friendly label driving the pill color.
	// ok → good; parse_error/schema_violation/refused/degenerate_loop /
	// downstream_rejected/gate_failed/failed/timeout → bad; pending,
	// cancelled → neutral.
	OutcomeClass string
	// HallucinationSignals is the parsed JSONB blob from the
	// outcome row. Empty when no signals were emitted (or the
	// detector wasn't wired). Rendered in a "claim review" panel
	// next to each step row.
	HallucinationSignals []HallucinationSignalRow
}

// HallucinationSignalRow projects one detector finding for the
// template. Strings are pre-formatted (no template-side string
// concatenation) so the template stays simple.
type HallucinationSignalRow struct {
	Detector         string
	Severity         string
	ClaimType        string
	ClaimValue       string
	Sentence         string
	EvidenceSearched string
	Detail           string
	// SeverityClass drives the row's CSS pill colour. info →
	// neutral, warn → yellow, high → red.
	SeverityClass string
	// RecordedAt is the pre-formatted detection time (UTC,
	// "YYYY-MM-DD HH:MM:SS"). Empty when the source row didn't
	// carry a timestamp — for the executor render path that's the
	// common case since signals are captured in-flight without
	// timestamping; for the chat-audit drill-down it's always
	// populated.
	RecordedAt string
}

// ExecutionDetail renders a single execution detail page.
func (s *Server) ExecutionDetail(w http.ResponseWriter, r *http.Request) {
	// Extract execution ID from path
	executionID := r.URL.Path[len("/executions/"):]
	s.logger.Debug().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("execution_id", executionID).
		Msg("rendering execution detail")
	if executionID == "" {
		s.logger.Warn().Msg("execution detail requested without execution id")
		http.NotFound(w, r)
		return
	}

	data := ExecutionDetailData{CurrentPage: "tasks"}

	// Get execution
	if s.execRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		exec, err := s.execRepo.Get(ctx, executionID)
		if err != nil {
			s.logger.Warn().Err(err).Str("execution_id", executionID).Msg("execution not found for UI")
			http.NotFound(w, r)
			return
		}
		// Project-scope check — a scoped key for project A must
		// not read project B's execution, which carries workflow
		// results, step outcomes, errors, and linked task data.
		if exec.ProjectID != "" && !api.RequestAllowsProject(r, exec.ProjectID) {
			http.NotFound(w, r)
			return
		}
		data.Execution = exec
		data.Title = "Execution: " + executionID

		// Get associated task
		if s.taskRepo != nil && exec.TaskID != "" {
			task, err := s.taskRepo.Get(ctx, exec.TaskID)
			if err == nil {
				data.Task = task
			} else {
				s.logger.Warn().
					Err(err).
					Str("execution_id", executionID).
					Str("task_id", exec.TaskID).
					Msg("failed to load execution task for UI")
			}
		}

		// Get workflow definition for step progress display
		if s.projectReg != nil && exec.WorkflowID != "" {
			data.Workflow = s.projectReg.GetWorkflow(exec.WorkflowID)
		}

		// Strict-adaptive routing decision: surface the lead's
		// workflow choice so the operator can see WHICH workflow
		// ran without drilling into the child task. Only fires
		// when this execution ran the adaptive workflow and the
		// result.json carries selected_workflow — non-adaptive
		// executions and legacy free-form runs leave Routing nil
		// and the panel doesn't render.
		if exec.WorkflowID == "adaptive" && len(exec.Result) > 0 {
			data.Routing = parseRoutingDecision(exec.Result)
			// Look up the child task to render a link from the
			// parent's page. GetChildren returns []Task; for
			// strict-adaptive we expect exactly one child (the
			// router only delegates once), but tolerate >1 by
			// preferring the first child whose workflow matches
			// the lead's selection.
			if data.Routing != nil && s.taskRepo != nil && data.Task != nil {
				children, err := s.taskRepo.GetChildren(ctx, data.Task.ID)
				if err != nil {
					s.logger.Debug().Err(err).
						Str("task_id", data.Task.ID).
						Msg("routing: failed to list children for adaptive parent — link will be omitted")
				} else {
					for _, c := range children {
						if c == nil || c.WorkflowID == nil {
							continue
						}
						if data.Routing.ChildTaskID == "" || *c.WorkflowID == data.Routing.SelectedWorkflow {
							data.Routing.ChildTaskID = c.ID
							data.Routing.ChildWorkflowID = *c.WorkflowID
						}
					}
				}
			}
		}

		// Load per-step outcomes for the quality panel. Silent degrade
		// to empty list when the repo isn't wired or the execution
		// predates the outcome table.
		if s.outcomeRepo != nil {
			rows, err := s.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{
				ExecutionID: &executionID,
				PageSize:    200,
			})
			if err != nil {
				s.logger.Warn().Err(err).Str("execution_id", executionID).Msg("failed to load step outcomes for UI")
			} else if len(rows) > 0 {
				// Repo returns newest first; reverse so the UI shows step
				// order as the workflow ran it. RecordedAt is the primary
				// key — within a single workflow that's monotonic, so a
				// plain ascending sort gives execution order.
				//
				// Tie-break on ID is what saves us when two outcomes land
				// in the same Postgres microsecond (gate + agent step
				// finalize within the same workflow loop tick, plan
				// sub-step batches that emit several outcomes back-to-
				// back, or two Record() calls under heavy I/O contention
				// where Go-side time.Now() resolves identically). Without
				// the tie-break, sort.Slice was unstable and operators
				// reported step rows arriving in scrambled order across
				// page reloads. SliceStable + secondary key on ID gives
				// deterministic display, and the ID embeds a second-
				// precision timestamp so it correlates with insert order
				// even across the rare same-microsecond case.
				sort.SliceStable(rows, func(i, j int) bool {
					if !rows[i].RecordedAt.Equal(rows[j].RecordedAt) {
						return rows[i].RecordedAt.Before(rows[j].RecordedAt)
					}
					return rows[i].ID < rows[j].ID
				})
				data.Outcomes = make([]StepOutcomeRow, 0, len(rows))
				data.OutcomeByStep = make(map[string]StepOutcomeRow, len(rows))
				data.StepColorByID = make(map[string]string, len(rows))
				for _, o := range rows {
					row := StepOutcomeRow{
						StepID:      o.StepID,
						Role:        o.Role,
						Model:       o.Model,
						Outcome:     o.Outcome,
						ErrorClass:  o.ErrorClass,
						ErrorDetail: o.ErrorDetail,
					}
					if o.AttributedToStepID != nil {
						row.AttributedToStepID = *o.AttributedToStepID
					}
					if o.DurationMS != nil {
						row.DurationMS = *o.DurationMS
						row.DurationDisplay = formatOutcomeDuration(*o.DurationMS)
					} else {
						row.DurationDisplay = "—"
					}
					row.OutcomeClass = outcomeCSSClass(o.Outcome)
					row.HallucinationSignals = parseHallucinationSignalsForUI(o.HallucinationSignals)
					data.Outcomes = append(data.Outcomes, row)
					data.TotalDurationMS += row.DurationMS
					if row.DurationMS > data.MaxStepDurationMS {
						data.MaxStepDurationMS = row.DurationMS
					}
					data.OutcomeByStep[row.StepID] = row
					if _, seen := data.StepColorByID[row.StepID]; !seen {
						color := stepIdentityColor(len(data.StepColorOrder))
						data.StepColorByID[row.StepID] = color
						data.StepColorOrder = append(data.StepColorOrder, StepColorEntry{
							StepID: row.StepID,
							Color:  template.CSS(color),
						})
					}
				}
			}
		}
	} else {
		s.logger.Warn().Msg("execution repository is not configured for UI")
		http.NotFound(w, r)
		return
	}

	s.render(w, "execution.html", data)
}

// parseHallucinationSignalsForUI decodes the JSONB blob carried
// on each outcome row into render-ready signal projections.
// Returns nil when the blob is absent / unparseable — a malformed
// blob shouldn't kill the page, just hide that step's signal
// panel and log.
func parseHallucinationSignalsForUI(blob []byte) []HallucinationSignalRow {
	if len(blob) == 0 {
		return nil
	}
	var signals []hallucination.Signal
	if err := json.Unmarshal(blob, &signals); err != nil {
		return nil
	}
	out := make([]HallucinationSignalRow, 0, len(signals))
	for _, s := range signals {
		out = append(out, HallucinationSignalRow{
			Detector:      s.Detector,
			Severity:      string(s.Severity),
			ClaimType:     s.ClaimType,
			ClaimValue:    s.ClaimValue,
			Sentence:      s.Sentence,
			Detail:        s.Detail,
			SeverityClass: hallucinationSeverityCSSClass(s.Severity),
		})
	}
	return out
}

// judgeVerdictCSSClass maps a verdict literal to a pill class.
// Mirrors the outcome scheme: pass→ok, fail→bad, abstain→pending.
func judgeVerdictCSSClass(verdict string) string {
	switch verdict {
	case persistence.JudgeVerdictPass:
		return "outcome-ok"
	case persistence.JudgeVerdictFail:
		return "outcome-bad"
	case persistence.JudgeVerdictAbstain:
		return "outcome-pending"
	default:
		return "outcome-neutral"
	}
}

// hallucinationSeverityCSSClass maps a severity to the pill class
// the template uses. Mirrors outcomeCSSClass — "outcome-bad" for
// High, "outcome-pending" for Warn, "outcome-neutral" for Info —
// so the existing CSS palette covers it without new rules.
func hallucinationSeverityCSSClass(s hallucination.Severity) string {
	switch s {
	case hallucination.SeverityHigh:
		return "outcome-bad"
	case hallucination.SeverityWarn:
		return "outcome-pending"
	default:
		return "outcome-neutral"
	}
}

// formatOutcomeDuration formats a ms duration as "123ms" or "1.4s" for
// compact display in the outcomes panel.
func formatOutcomeDuration(ms int64) string {
	if ms < 0 {
		return "—"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}

// outcomeCSSClass maps an outcome string to a CSS class name used by
// the template to colour the pill. Keep aligned with the list in
// internal/stepoutcome so new outcomes show up gracefully (they fall
// through to "neutral" until styled).
func outcomeCSSClass(outcome string) string {
	switch outcome {
	case "ok":
		return "outcome-ok"
	case "pending_validation":
		return "outcome-pending"
	case "cancelled", "superseded":
		// "superseded" outcomes are the result of an operator
		// retrying from an earlier step — neutral, not bad. They
		// shouldn't drag the dashboard's quality stats down.
		return "outcome-neutral"
	case "parse_error", "schema_violation", "refused",
		"iteration_exhausted", "degenerate_loop",
		"downstream_rejected", "gate_failed",
		"failed", "timeout", "budget_tripwire":
		return "outcome-bad"
	default:
		return "outcome-neutral"
	}
}

// stepIdentityColor picks a stable color for the n-th unique step
// in an execution using golden-angle hue spacing. The golden angle
// (≈ 137.508°) is the irrational rotation that maximises minimum
// distance between consecutive samples on a circle, so adjacent
// steps land far apart in hue regardless of how many steps there
// are. This is the same trick d3-scheme-category and matplotlib's
// "hsv" cycler use.
//
// Earlier rev used a hand-picked 12-entry palette but several pairs
// (190/210, 265/280, 5/355, 30/50) were only 10-20° apart and read
// as identical against the dark background — operators reported
// "all steps have the same color".
//
// Saturation and lightness are pinned to values that read well on
// bg-dark-800 (#1f2937-ish): mid-saturation so neighbouring colors
// don't clash with the emerald/amber/rose outcome palette, mid-
// lightness so every hue including yellow remains visible.
func stepIdentityColor(n int) string {
	if n < 0 {
		n = 0
	}
	hue := math.Mod(float64(n)*137.508, 360.0)
	return fmt.Sprintf("hsl(%.0f, 65%%, 60%%)", hue)
}

// parseRoutingDecision extracts the strict-adaptive lead's
// `selected_workflow` and `reason` fields from an execution's
// result.json. Returns nil when the bytes don't parse as a JSON
// object or when no selection is present — so the caller can
// branch on `Routing == nil` to skip rendering the panel.
//
// Lives in the UI handler rather than a shared helper because
// only the dashboard reads this for display purposes; the
// executor's runtime path uses its own inline parser tied to the
// delegation flow. Duplicating the field names is the cost of
// keeping the executor free of a UI-shaped result struct.
func parseRoutingDecision(result []byte) *RoutingDecision {
	if len(result) == 0 {
		return nil
	}
	var parsed struct {
		SelectedWorkflow string `json:"selected_workflow"`
		Reason           string `json:"reason"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil
	}
	if parsed.SelectedWorkflow == "" {
		return nil
	}
	return &RoutingDecision{
		SelectedWorkflow: parsed.SelectedWorkflow,
		Reason:           parsed.Reason,
	}
}
