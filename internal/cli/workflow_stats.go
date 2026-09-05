package cli

// `vornikctl workflow-stats` — per-workflow execution-evidence
// rollup. Slice 1 of the memetic-workflows arc; the architect
// agent (Slice 2) will consume the same JSON shape. Useful for
// terminal operators to sanity-check what the architect sees
// before approving any proposal.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"

	"vornik.io/vornik/internal/stepid"
	"vornik.io/vornik/internal/stepoutcome"

	"github.com/spf13/cobra"
)

var (
	wfStatsWorkflow string
	wfStatsSince    string
	wfStatsJSON     bool
)

var workflowStatsCmd = &cobra.Command{
	Use:   "workflow-stats",
	Short: "Show per-workflow execution-evidence rollup (admin)",
	Long: `Show the per-workflow rollup the architect agent (memetic-
workflows arc, Slice 2) consumes. Pulls execution counts, per-step
outcome distributions, top failure classes, judge verdicts, and
hallucination + operator-intervention rates across every project
that uses the workflow.

Requires an admin-scoped API key.

Examples:
  vornikctl workflow-stats --workflow dev-pipeline
  vornikctl workflow-stats --workflow research --since 24h
  vornikctl workflow-stats --workflow research --since 7d --json`,
	RunE: runWorkflowStats,
}

func init() {
	workflowStatsCmd.Flags().StringVarP(&wfStatsWorkflow, "workflow", "w", "", "Workflow ID (required)")
	workflowStatsCmd.Flags().StringVar(&wfStatsSince, "since", "7d",
		"Lookback window: <N>d / <N>h / <N>m / RFC3339 timestamp")
	workflowStatsCmd.Flags().BoolVar(&wfStatsJSON, "json", false, "Emit JSON instead of a human-readable table")
	_ = workflowStatsCmd.MarkFlagRequired("workflow")
	rootCmd.AddCommand(workflowStatsCmd)
}

// workflowStatsResponse mirrors workflowtelemetry.Rollup on the
// client side. Kept here so the CLI stays self-contained; missing
// fields are JSON-null-tolerated.
type workflowStatsResponse struct {
	WorkflowID               string                      `json:"workflow_id"`
	WindowStart              string                      `json:"window_start"`
	WindowEnd                string                      `json:"window_end"`
	RunCount                 int                         `json:"run_count"`
	SuccessCount             int                         `json:"success_count"`
	FailureCount             int                         `json:"failure_count"`
	CancelledCount           int                         `json:"cancelled_count"`
	Steps                    []workflowStatsStep         `json:"steps"`
	AvgCostUSD               float64                     `json:"avg_cost_usd"`
	AvgDurationSeconds       float64                     `json:"avg_duration_seconds"`
	JudgeVerdictDist         map[string]int              `json:"judge_verdict_dist"`
	HallucinationRate        float64                     `json:"hallucination_rate"`
	OperatorInterventionRate float64                     `json:"operator_intervention_rate"`
	TopFailureClasses        []workflowStatsFailureClass `json:"top_failure_classes"`
	ClassifiedStepFailures   int                         `json:"classified_step_failures"`
	UnclassifiedStepFailures int                         `json:"unclassified_step_failures"`
}

type workflowStatsStep struct {
	StepID             string         `json:"step_id"`
	Role               string         `json:"role"`
	Model              string         `json:"model"`
	OutcomeDist        map[string]int `json:"outcome_dist"`
	AvgCostUSD         float64        `json:"avg_cost_usd"`
	AvgDurationSeconds float64        `json:"avg_duration_seconds"`
	TopErrorClass      string         `json:"top_error_class,omitempty"`
}

type workflowStatsFailureClass struct {
	ErrorClass string `json:"error_class"`
	Count      int    `json:"count"`
}

func runWorkflowStats(_ *cobra.Command, _ []string) error {
	if wfStatsWorkflow == "" {
		return fmt.Errorf("--workflow is required")
	}
	q := url.Values{}
	q.Set("workflow", wfStatsWorkflow)
	if wfStatsSince != "" {
		q.Set("since", wfStatsSince)
	}
	path := "/api/v1/admin/workflow-stats?" + q.Encode()

	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("workflow-stats: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}

	var rollup workflowStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&rollup); err != nil {
		return fmt.Errorf("workflow-stats: decode failed: %w", err)
	}

	if wfStatsJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rollup)
	}
	return renderWorkflowStats(os.Stdout, &rollup)
}

// renderWorkflowStats prints the rollup in operator-friendly form.
// renderWorkflowRates prints the two rates a reader is likely to conflate, each
// named for what it actually measures.
//
// THE MOTIVATING CONFUSION, 2026-08-26. An operator asked why
// `companion-doc-review` sat at 42% and `plan-and-write` at 67%, and was about
// to prioritise work on `plan-and-write` — which had failed ZERO of fourteen
// runs. Its "67%" was passing-steps over total-steps across three steps (27/40);
// doc-review's "42%" was one step's first-attempt rate (ok=5 of 12). Both
// numbers are real and both are useful. Neither is a run success rate, and
// nothing said so.
//
// A metric that misroutes attention is worse than no metric, which is why this
// prints BOTH rather than picking one: the gap between them is largest exactly
// where the retry ladder is working best, so seeing only the step rate makes a
// healthy workflow look broken.
//
// Named "run success" and "step pass (first attempt)" rather than "success rate"
// — the bare phrase is what let one number stand in for the other.
func renderWorkflowRates(out *os.File, r *workflowStatsResponse) {
	if r.RunCount <= 0 {
		return
	}
	runSuccess := float64(r.SuccessCount) / float64(r.RunCount) * 100

	// The step rate is computed HERE from the outcome distribution the rollup
	// already carries, rather than read from the A1 gauge: the gauge is windowed
	// and swarm-scoped, so a figure taken from it would not be over the same
	// runs as the line above it. Two numbers side by side must share a
	// denominator's worth of provenance or the comparison is the bug again.
	// Both come from ONE rollup response, built over one window by
	// workflowtelemetry.ForWorkflow: RunCount counts the executions in it and
	// Steps aggregates the step-outcome rows of those same executions. If the
	// backend ever windows the two differently, this pairing is the bug.
	//
	// "First attempt" means base-step rows ONLY. The executor persists every
	// retry rung as its own row under a suffixed id (_shape_retry,
	// _model_fallback, _infra_retryN, …), and a rung's own `ok` is the ladder
	// rescuing a step, not the step passing first time. Folding the rungs in
	// inflated the rate most where the ladder works best — the misdirection
	// this rendering exists to prevent, one level down (review 2026-09-03).
	// `orphaned` leaves the denominator too, and for a stronger reason than
	// the rungs: a rung at least tells you the ladder ran. An orphaned row is
	// an attempt whose execution was terminalised — the task started a new
	// run, or its parent went terminal — before anything learned the step's
	// outcome. Counting it as a non-pass reports an absence as a failure.
	//
	// This is not a rounding correction. On `adaptive`, 200 of `route`'s 294
	// recorded attempts are orphaned, and including them printed 6% where the
	// attempts that actually concluded give 18% — the number a backlog item
	// was built on for two weeks (design 2026-09-04-orphaned-step-outcomes §1).
	// The count is PRINTED rather than quietly dropped: a figure that rose
	// from 6% to 18% without saying what left the denominator would be the
	// same misdirection pointed the other way.
	var stepOK, stepTotal, stepOrphaned int
	for _, s := range r.Steps {
		if stepid.IsRetryAttempt(s.StepID) {
			continue
		}
		for outcome, n := range s.OutcomeDist {
			if outcome == string(stepoutcome.Orphaned) {
				stepOrphaned += n
				continue
			}
			stepTotal += n
			if outcome == "ok" {
				stepOK += n
			}
		}
	}

	_, _ = fmt.Fprintf(out, "  run success: %.0f%% (%d/%d runs)",
		runSuccess, r.SuccessCount, r.RunCount)
	if stepTotal > 0 {
		_, _ = fmt.Fprintf(out, "  •  step pass (first attempt): %.0f%% (%d/%d step attempts",
			float64(stepOK)/float64(stepTotal)*100, stepOK, stepTotal)
		if stepOrphaned > 0 {
			_, _ = fmt.Fprintf(out, "; %d orphaned attempts excluded", stepOrphaned)
		}
		_, _ = fmt.Fprint(out, ")")
	}
	_, _ = fmt.Fprintln(out)
	if stepTotal > 0 && stepOK < stepTotal && r.SuccessCount == r.RunCount {
		// Say it outright in the case that caused the confusion, rather than
		// leaving the reader to notice two numbers disagree.
		_, _ = fmt.Fprintln(out, "  (every run completed; the step rate below 100% is the retry "+
			"ladder doing its job, not runs failing)")
	}
}

// Sections: header (workflow + window + counts), per-step table,
// top failure classes, quality signals. Empty sections collapse so
// a low-traffic workflow doesn't show a wall of zeros.
func renderWorkflowStats(out *os.File, r *workflowStatsResponse) error {
	if r.RunCount == 0 {
		_, _ = fmt.Fprintf(out, "Workflow %q: no runs in window %s → %s.\n",
			r.WorkflowID, truncate(r.WindowStart, 19), truncate(r.WindowEnd, 19))
		return nil
	}

	_, _ = fmt.Fprintf(out, "Workflow %q  •  %d runs in window %s → %s\n",
		r.WorkflowID, r.RunCount, truncate(r.WindowStart, 19), truncate(r.WindowEnd, 19))
	_, _ = fmt.Fprintf(out, "  %d completed  •  %d failed  •  %d cancelled\n",
		r.SuccessCount, r.FailureCount, r.CancelledCount)
	renderWorkflowRates(out, r)
	if r.AvgCostUSD > 0 {
		_, _ = fmt.Fprintf(out, "  avg cost: $%.4f / run  •  avg duration: %.1fs\n",
			r.AvgCostUSD, r.AvgDurationSeconds)
	}
	if r.HallucinationRate > 0 || r.OperatorInterventionRate > 0 {
		_, _ = fmt.Fprintf(out, "  hallucination rate: %.1f%%  •  operator intervention rate: %.1f%%\n",
			r.HallucinationRate*100, r.OperatorInterventionRate*100)
	}
	if len(r.JudgeVerdictDist) > 0 {
		_, _ = fmt.Fprint(out, "  judge verdicts:")
		verdicts := sortedKeys(r.JudgeVerdictDist)
		for _, v := range verdicts {
			_, _ = fmt.Fprintf(out, " %s=%d", v, r.JudgeVerdictDist[v])
		}
		_, _ = fmt.Fprintln(out)
	}

	if len(r.Steps) > 0 {
		_, _ = fmt.Fprintln(out, "\nSteps:")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "STEP\tROLE\tMODEL\tOUTCOMES\tAVG_DUR\tTOP_ERR")
		for _, s := range r.Steps {
			outcomes := renderOutcomeDist(s.OutcomeDist)
			topErr := s.TopErrorClass
			if topErr == "" {
				topErr = "—"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.1fs\t%s\n",
				truncate(s.StepID, 20),
				truncate(s.Role, 14),
				truncate(s.Model, 24),
				outcomes,
				s.AvgDurationSeconds,
				topErr)
		}
		_ = tw.Flush()
	}

	if len(r.TopFailureClasses) > 0 {
		_, _ = fmt.Fprintln(out, "\nTop failure classes:")
		for _, fc := range r.TopFailureClasses {
			_, _ = fmt.Fprintf(out, "  %d  %s\n", fc.Count, fc.ErrorClass)
		}
	}
	if line := renderUnclassifiedShare(r); line != "" {
		_, _ = fmt.Fprint(out, line)
	}
	return nil
}

// renderUnclassifiedShare reports the residual bucket as a SHARE of classified
// step failures rather than a bare count.
//
// A bare count misleads in exactly the way that started this work: "104
// container_non_zero_exit" was read as a fleet total when it was one workflow
// over 30 days, and the real figure was 3,027 — 52% of every classified
// failure. The denominator is the fact that makes the numerator mean anything.
//
// Returns "" when the window holds no classified failures: that is an absence
// of evidence, and printing "0.0% unclassified" would assert coverage that was
// never measured.
func renderUnclassifiedShare(r *workflowStatsResponse) string {
	if r.ClassifiedStepFailures <= 0 {
		return ""
	}
	share := float64(r.UnclassifiedStepFailures) / float64(r.ClassifiedStepFailures) * 100
	return fmt.Sprintf("\nUnclassified: %d of %d classified step failures (%.1f%%)\n",
		r.UnclassifiedStepFailures, r.ClassifiedStepFailures, share)
}

// renderOutcomeDist formats a {outcome: count} map as
// "ok=5 failed=2 timeout=1" with stable key ordering (highest
// count first; ties broken alphabetically) so the same input
// renders identically across runs.
func renderOutcomeDist(m map[string]int) string {
	if len(m) == 0 {
		return "—"
	}
	type pair struct {
		k string
		v int
	}
	pairs := make([]pair, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, pair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var b []byte
	for i, p := range pairs {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, []byte(fmt.Sprintf("%s=%d", p.k, p.v))...)
	}
	return string(b)
}

// sortedKeys returns the keys of m sorted alphabetically. Used so
// the human-readable output is stable.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
