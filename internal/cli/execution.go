package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// Execution responses from the API
type executionResponse struct {
	ExecutionID    string   `json:"executionId"`
	TaskID         string   `json:"taskId"`
	ProjectID      string   `json:"projectId"`
	WorkflowID     string   `json:"workflowId"`
	Status         string   `json:"status"`
	CurrentStepID  string   `json:"currentStepId,omitempty"`
	CompletedSteps []string `json:"completedSteps,omitempty"`
	ErrorMessage   string   `json:"errorMessage,omitempty"`
	ErrorCode      string   `json:"errorCode,omitempty"`
	StartedAt      string   `json:"startedAt,omitempty"`
	CompletedAt    string   `json:"completedAt,omitempty"`
	Duration       string   `json:"duration,omitempty"`
}

// aggregatedExecution collapses N executions for one task into a
// single display row. Behaviour matches what operators want when
// retries fire: see ONE outcome per task with a count, not N rows
// where the FAILED is just noise from a successful retry chain.
type aggregatedExecution struct {
	TaskID         string
	LatestExecID   string
	LatestStatus   string
	LatestWorkflow string
	LatestDuration string
	Attempts       int
}

// aggregateExecutionsByTask groups a flat list of executions by
// task id and, per task, returns the LATEST execution (largest
// startedAt, falling back to original order for ties / missing
// timestamps) plus the total attempt count. Insertion order of
// the returned slice is the order each task FIRST appears in the
// input — the API already returns newest-first, so the latest
// per task ends up at index 0 for that task, and subsequent rows
// from the same task only contribute to Attempts.
//
// Pulled out as a pure function so the unit test can exercise it
// without the cobra/http harness.
func aggregateExecutionsByTask(execs []executionResponse) []aggregatedExecution {
	type bucket struct {
		first   int // insertion-order index, used for stable output
		row     aggregatedExecution
		latestT string
	}
	buckets := make(map[string]*bucket, len(execs))
	order := make([]string, 0, len(execs))

	for _, e := range execs {
		b, ok := buckets[e.TaskID]
		if !ok {
			order = append(order, e.TaskID)
			buckets[e.TaskID] = &bucket{
				first: len(order) - 1,
				row: aggregatedExecution{
					TaskID:         e.TaskID,
					LatestExecID:   e.ExecutionID,
					LatestStatus:   e.Status,
					LatestWorkflow: e.WorkflowID,
					LatestDuration: e.Duration,
					Attempts:       1,
				},
				latestT: e.StartedAt,
			}
			continue
		}
		b.row.Attempts++
		// "Latest" = the largest StartedAt; lexicographic compare
		// is correct for RFC3339 timestamps. Missing-timestamp
		// entries lose to any populated one. If both are missing
		// or equal, the FIRST occurrence wins, which matches the
		// API's newest-first ordering (so the first row IS the
		// freshest one when the API behaves).
		if e.StartedAt != "" && e.StartedAt > b.latestT {
			b.row.LatestExecID = e.ExecutionID
			b.row.LatestStatus = e.Status
			b.row.LatestWorkflow = e.WorkflowID
			b.row.LatestDuration = e.Duration
			b.latestT = e.StartedAt
		}
	}

	// Stable output ordering: first appearance in the input. Map
	// iteration is non-deterministic; relying on `order` keeps
	// repeated invocations consistent.
	out := make([]aggregatedExecution, 0, len(order))
	for _, tid := range order {
		out = append(out, buckets[tid].row)
	}
	// Deterministic secondary sort when the caller doesn't care
	// about source ordering — but the default keeps first-seen.
	// No-op here; documented for the reader.
	_ = sort.SliceStable
	return out
}

type listExecutionsResponse struct {
	Executions []executionResponse `json:"executions"`
	Total      int                 `json:"total"`
}

var executionCmd = &cobra.Command{
	Use:   "execution",
	Short: "Manage executions",
	Long:  "Inspect and list workflow executions in the vornik control plane.",
}

var executionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List executions",
	Long:  "List executions for a project, optionally filtered by task or status.",
	RunE:  runExecutionList,
}

var executionInspectCmd = &cobra.Command{
	Use:   "inspect <executionId>",
	Short: "Inspect an execution",
	Long:  "Get detailed information about an execution by ID.",
	Args:  cobra.ExactArgs(1),
	RunE:  runExecutionInspect,
}

var (
	executionListProject string
	executionListTask    string
	executionListStatus  string
	executionListJSON    bool
	// executionListAll opts back into the verbose per-execution
	// view. Default behaviour aggregates by taskId so retried
	// tasks show ONE row + an attempts count instead of N rows.
	executionListAll     bool
	executionInspectJSON bool
)

func init() {
	// execution list flags
	executionListCmd.Flags().StringVarP(&executionListProject, "project", "p", "", "Project ID (required)")
	executionListCmd.Flags().StringVarP(&executionListTask, "task", "t", "", "Filter by task ID")
	executionListCmd.Flags().StringVarP(&executionListStatus, "status", "s", "", "Filter by status (PENDING, RUNNING, COMPLETED, FAILED, CANCELLED)")
	executionListCmd.Flags().BoolVar(&executionListJSON, "json", false, "Output in JSON format")
	executionListCmd.Flags().BoolVar(&executionListAll, "all", false, "Show every execution instead of aggregating retries by task")
	_ = executionListCmd.MarkFlagRequired("project")

	// execution inspect flags
	executionInspectCmd.Flags().BoolVar(&executionInspectJSON, "json", false, "Output in JSON format")

	// Add subcommands
	executionCmd.AddCommand(executionListCmd)
	executionCmd.AddCommand(executionInspectCmd)

	// Add to root
	rootCmd.AddCommand(executionCmd)
}

func runExecutionList(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()

	// Build URL with query parameters
	path := fmt.Sprintf("/api/v1/projects/%s/executions", executionListProject)
	params := []string{}
	if executionListTask != "" {
		params = append(params, fmt.Sprintf("taskId=%s", executionListTask))
	}
	if executionListStatus != "" {
		params = append(params, fmt.Sprintf("status=%s", executionListStatus))
	}
	if len(params) > 0 {
		path += "?" + params[0]
		for i := 1; i < len(params); i++ {
			path += "&" + params[i]
		}
	}

	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("failed to list executions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var result listExecutionsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if executionListJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	// Aggregation default — collapse retried executions into one
	// row per task. Skip when the operator explicitly asked for
	// the verbose view (--all) OR when they filtered by --task
	// (in which case the operator clearly wants every attempt
	// visible for that single task).
	aggregate := !executionListAll && executionListTask == ""
	if aggregate {
		rows := aggregateExecutionsByTask(result.Executions)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "TASK ID\tLATEST STATUS\tATTEMPTS\tEXECUTION ID\tWORKFLOW\tDURATION")
		for _, r := range rows {
			attempts := fmt.Sprintf("%d", r.Attempts)
			if r.Attempts > 1 {
				// Suffix on the count to draw the eye to rows
				// where retries happened — operators scanning
				// for "what failed and recovered" want this row
				// to stand out without becoming noisy.
				attempts = fmt.Sprintf("%d ↻", r.Attempts)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.TaskID,
				r.LatestStatus,
				attempts,
				r.LatestExecID,
				r.LatestWorkflow,
				r.LatestDuration,
			)
		}
		_ = w.Flush()
		fmt.Printf("\n%d task(s) across %d execution(s); use --all to see every attempt.\n", len(rows), result.Total)
		return nil
	}

	// Verbose per-execution view: --all or --task filter.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "EXECUTION ID\tSTATUS\tTASK ID\tWORKFLOW\tDURATION")
	for _, exec := range result.Executions {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			exec.ExecutionID,
			exec.Status,
			exec.TaskID,
			exec.WorkflowID,
			exec.Duration,
		)
	}
	_ = w.Flush()

	fmt.Printf("\nTotal: %d\n", result.Total)
	return nil
}

func runExecutionInspect(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	executionID := args[0]

	path := fmt.Sprintf("/api/v1/executions/%s", executionID)

	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("failed to get execution: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var result executionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if executionInspectJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	// Human-readable output
	fmt.Printf("Execution ID:   %s\n", result.ExecutionID)
	fmt.Printf("Task ID:        %s\n", result.TaskID)
	fmt.Printf("Project ID:     %s\n", result.ProjectID)
	fmt.Printf("Workflow ID:    %s\n", result.WorkflowID)
	fmt.Printf("Status:         %s\n", result.Status)

	if result.CurrentStepID != "" {
		fmt.Printf("Current Step:   %s\n", result.CurrentStepID)
	}
	if len(result.CompletedSteps) > 0 {
		fmt.Printf("Completed Steps: %v\n", result.CompletedSteps)
	}
	if result.StartedAt != "" {
		fmt.Printf("Started At:     %s\n", result.StartedAt)
	}
	if result.CompletedAt != "" {
		fmt.Printf("Completed At:   %s\n", result.CompletedAt)
	}
	if result.Duration != "" {
		fmt.Printf("Duration:       %s\n", result.Duration)
	}
	if result.ErrorMessage != "" {
		fmt.Printf("Error:          %s\n", result.ErrorMessage)
	}
	if result.ErrorCode != "" {
		fmt.Printf("Error Code:     %s\n", result.ErrorCode)
	}

	return nil
}
