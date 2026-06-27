package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Autonomy CLI — surfaces the per-tick evaluation audit trail added
// by S1. Before this command existed, the only way to see why a
// project wasn't scheduling tasks was grepping journald.

var (
	autonomyCmd = &cobra.Command{
		Use:   "autonomy",
		Short: "Inspect autonomy evaluation audit trail",
	}
	autonomyEvaluationsCmd = &cobra.Command{
		Use:     "evaluations",
		Aliases: []string{"evals"},
		Short:   "List recent autonomy evaluation outcomes for a project",
		Long: `Show per-tick autonomy outcomes. Each autonomy tick writes one row
regardless of whether a task was created; the outcome tells you why.

Common outcomes:
  CREATED           task successfully enqueued
  NO_ACTION         lead decided no work needed
  ACTIVE_TASKS      tick skipped because tasks were still running
  RATE_LIMITED      project or config per-minute / per-hour cap hit
  BUDGET_BLOCKED    project over its daily / monthly hard cap
  LLM_ERROR         chat provider failed
  PARSE_ERROR       LLM response unparseable as a task
  WORKFLOW_INVALID  workflow or role mismatch (common n8n-agents bug class)
  TYPE_REJECTED     LLM picked a task type outside allowedTaskTypes
  CIRCUIT_OPEN      too many recent failures — breaker tripped
  DUPLICATE         same prompt as a recent task
  COOLDOWN          same prompt failed multiple times recently
  IDEMPOTENCY_HIT   identical idempotency key already present
  DB_ERROR          task persistence layer failed
  ABORTED           eval cancelled by loop reload/shutdown (benign teardown)`,
		RunE: runAutonomyEvaluations,
	}
	autonomySummaryCmd = &cobra.Command{
		Use:   "summary",
		Short: "Aggregate autonomy outcomes by count",
		RunE:  runAutonomySummary,
	}

	autonomyProject string
	autonomyOutcome string
	autonomyLimit   int
	autonomyJSON    bool
	autonomyHours   int
)

func init() {
	autonomyEvaluationsCmd.Flags().StringVarP(&autonomyProject, "project", "p", "", "Project ID (required)")
	autonomyEvaluationsCmd.Flags().StringVar(&autonomyOutcome, "outcome", "", "Filter by outcome (e.g. CREATED, NO_ACTION, RATE_LIMITED)")
	autonomyEvaluationsCmd.Flags().IntVarP(&autonomyLimit, "limit", "n", 50, "Max rows to return (1-500)")
	autonomyEvaluationsCmd.Flags().BoolVar(&autonomyJSON, "json", false, "Output JSON instead of table")
	_ = autonomyEvaluationsCmd.MarkFlagRequired("project")

	autonomySummaryCmd.Flags().StringVarP(&autonomyProject, "project", "p", "", "Project ID (required)")
	autonomySummaryCmd.Flags().IntVar(&autonomyHours, "hours", 24, "Window length in hours (max 720)")
	autonomySummaryCmd.Flags().BoolVar(&autonomyJSON, "json", false, "Output JSON instead of table")
	_ = autonomySummaryCmd.MarkFlagRequired("project")

	autonomyCmd.AddCommand(autonomyEvaluationsCmd, autonomySummaryCmd)
	rootCmd.AddCommand(autonomyCmd)
}

type autonomyEvalRow struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason"`
	TaskID     *string   `json:"task_id,omitempty"`
	TaskType   string    `json:"task_type"`
	WorkflowID string    `json:"workflow_id"`
	PromptHash string    `json:"prompt_hash"`
	DurationMs int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

func runAutonomyEvaluations(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	if autonomyOutcome != "" {
		q.Set("outcome", autonomyOutcome)
	}
	if autonomyLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", autonomyLimit))
	}
	path := fmt.Sprintf("/api/v1/projects/%s/autonomy/evaluations", autonomyProject)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	if autonomyJSON {
		return prettyPrintJSON(raw)
	}
	var parsed struct {
		Evaluations []autonomyEvalRow `json:"evaluations"`
		Total       int               `json:"total"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Evaluations) == 0 {
		fmt.Println("(no evaluations)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tOUTCOME\tDUR\tTYPE\tWORKFLOW\tTASK\tREASON")
	for _, e := range parsed.Evaluations {
		taskID := "—"
		if e.TaskID != nil && *e.TaskID != "" {
			taskID = shortenID(*e.TaskID)
		}
		typ := e.TaskType
		if typ == "" {
			typ = "—"
		}
		wf := e.WorkflowID
		if wf == "" {
			wf = "—"
		}
		reason := e.Reason
		if len(reason) > 80 {
			reason = reason[:77] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%dms\t%s\t%s\t%s\t%s\n",
			e.CreatedAt.Local().Format("15:04:05"),
			e.Outcome,
			e.DurationMs,
			typ,
			wf,
			taskID,
			reason,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", parsed.Total)
	return nil
}

func runAutonomySummary(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	if autonomyHours > 0 {
		q.Set("hours", fmt.Sprintf("%d", autonomyHours))
	}
	path := fmt.Sprintf("/api/v1/projects/%s/autonomy/summary", autonomyProject)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	if autonomyJSON {
		return prettyPrintJSON(raw)
	}
	var parsed struct {
		ProjectID string           `json:"projectId"`
		WindowHrs int              `json:"windowHrs"`
		Since     string           `json:"since"`
		Counts    map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Printf("Project:  %s\n", parsed.ProjectID)
	fmt.Printf("Window:   last %dh (since %s)\n", parsed.WindowHrs, parsed.Since)
	fmt.Println()

	if len(parsed.Counts) == 0 {
		fmt.Println("(no evaluations in window)")
		return nil
	}

	// Sort outcomes by count desc so the dominant outcome is first.
	type kv struct {
		name  string
		count int64
	}
	rows := make([]kv, 0, len(parsed.Counts))
	var total int64
	for k, v := range parsed.Counts {
		rows = append(rows, kv{k, v})
		total += v
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].name < rows[j].name
	})
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "OUTCOME\tCOUNT\tSHARE")
	for _, r := range rows {
		share := "—"
		if total > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(r.count)/float64(total))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\n", r.name, r.count, share)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", total)
	return nil
}

// shortenID returns the tail-hex chunk of a vornik ID so table rows
// stay readable (task_20260423233059_abcd1234 → …abcd1234).
func shortenID(id string) string {
	// Vornik IDs are {prefix}_{timestamp}_{hex} — keep just the hex.
	// Fall back to the last 8 chars when the shape is unexpected.
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '_' {
			return id[i+1:]
		}
	}
	if len(id) > 12 {
		return id[len(id)-12:]
	}
	return id
}
