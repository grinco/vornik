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
	return nil
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
