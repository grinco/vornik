package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
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
	executionPromptCmd.Flags().StringVar(&executionPromptPart, "part", "", "Print one part only: system, user or tools")
	executionCmd.AddCommand(executionPromptCmd)
	executionExchangesCmd.Flags().StringVar(&executionExchangesExport, "export", "", "Write the step's exchanges as a JSONL recording to this path instead of printing the table")
	executionCmd.AddCommand(executionExchangesCmd)
	executionInputCmd.Flags().StringVar(&executionStepFileExport, "export", "", "Write the file to this path (0600) instead of printing it")
	executionResultCmd.Flags().StringVar(&executionStepFileExport, "export", "", "Write the file to this path (0600) instead of printing it")
	executionCmd.AddCommand(executionInputCmd)
	executionCmd.AddCommand(executionResultCmd)

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

var executionPromptPart string

var executionPromptCmd = &cobra.Command{
	Use:   "prompt <executionId> <stepId>",
	Short: "Print what a step's model was told at its first request",
	Long: `Print the step's first model request as the daemon stored it — the system
prompt, the user content and the tools array — content-addressed and redacted at
write. Model-visible means persisted: a step that failed in prompt assembly
leaves this behind, so read it before blaming the tool. Empty for a step run by
an agent image that predates step-prompt persistence.`,
	Args: cobra.ExactArgs(2),
	RunE: runExecutionPrompt,
}

func runExecutionPrompt(_ *cobra.Command, args []string) error {
	raw, err := fetchJSON(fmt.Sprintf("/api/v1/executions/%s/steps/%s/prompt", args[0], args[1]))
	if err != nil {
		return err
	}
	var resp struct {
		ExecutionID string            `json:"execution_id"`
		StepID      string            `json:"step_id"`
		RecordedAt  string            `json:"recorded_at"`
		Hashes      map[string]string `json:"hashes"`
		Parts       map[string]string `json:"parts"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode prompt: %w", err)
	}
	return renderExecutionPrompt(os.Stdout, resp.ExecutionID, resp.StepID, resp.RecordedAt, resp.Hashes, resp.Parts, executionPromptPart)
}

// renderExecutionPrompt prints the parts in the order the model saw them —
// system, then user, then the tools array — each under a header carrying its
// hash, or one part bare when --part is given (so it can be piped).
func renderExecutionPrompt(out io.Writer, executionID, stepID, recordedAt string, hashes, parts map[string]string, only string) error {
	order := []string{"system", "user", "tools"}
	if only != "" {
		body, ok := parts[only]
		if !ok {
			return fmt.Errorf("unknown or unrecorded part %q (system, user, tools)", only)
		}
		_, err := fmt.Fprintln(out, body)
		return err
	}
	if _, err := fmt.Fprintf(out, "execution %s  step %s  recorded %s\n", executionID, stepID, recordedAt); err != nil {
		return err
	}
	for _, part := range order {
		body, ok := parts[part]
		if !ok {
			continue
		}
		if _, err := fmt.Fprintf(out, "\n=== %s  (sha256 %s)\n", part, hashes[part]); err != nil {
			return err
		}
		if body == "" {
			body = "(body no longer stored — pruned by retention)"
		}
		if _, err := fmt.Fprintln(out, body); err != nil {
			return err
		}
	}
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

var executionExchangesExport string

var executionExchangesCmd = &cobra.Command{
	Use:   "exchanges <executionId> <stepId>",
	Short: "List a step's recorded model exchanges, or export them as a replay recording",
	Long: `List every model request/response pair the chat proxy recorded for the step —
seq, iteration, model, tokens, duration, how many secrets were redacted, and
the finish reason. Recorded only for projects with recording.llm_exchanges
set; a step of any other project prints nothing. With --export the same rows
are written as the JSONL recording internal/llmreplay serves, so a test can
replay the step's model without the model.`,
	Args: cobra.ExactArgs(2),
	RunE: runExecutionExchanges,
}

func runExecutionExchanges(_ *cobra.Command, args []string) error {
	base := fmt.Sprintf("/api/v1/executions/%s/steps/%s/exchanges", args[0], args[1])
	if executionExchangesExport != "" {
		raw, err := fetchJSON(base + "?format=jsonl")
		if err != nil {
			return err
		}
		if err := os.WriteFile(executionExchangesExport, raw, 0o600); err != nil {
			return fmt.Errorf("write recording: %w", err)
		}
		lines := strings.Count(strings.TrimSpace(string(raw)), "\n")
		if len(strings.TrimSpace(string(raw))) > 0 {
			lines++
		}
		fmt.Printf("wrote %d exchange(s) to %s\n", lines, executionExchangesExport)
		return nil
	}
	raw, err := fetchJSON(base)
	if err != nil {
		return err
	}
	var resp struct {
		ExecutionID string `json:"execution_id"`
		StepID      string `json:"step_id"`
		Exchanges   []struct {
			Seq              int    `json:"seq"`
			Iteration        *int   `json:"iteration"`
			Model            string `json:"model"`
			PromptTokens     int    `json:"prompt_tokens"`
			CompletionTokens int    `json:"completion_tokens"`
			DurationMs       int    `json:"duration_ms"`
			Redactions       int    `json:"redactions"`
			FinishReason     string `json:"finish_reason"`
		} `json:"exchanges"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode exchanges: %w", err)
	}
	return renderExecutionExchanges(os.Stdout, resp.ExecutionID, resp.StepID, func(yield func(exchangeRow)) {
		for _, x := range resp.Exchanges {
			yield(exchangeRow{Seq: x.Seq, Iteration: x.Iteration, Model: x.Model, PromptTokens: x.PromptTokens,
				CompletionTokens: x.CompletionTokens, DurationMs: x.DurationMs, Redactions: x.Redactions, Finish: x.FinishReason})
		}
	})
}

type exchangeRow struct {
	Seq              int
	Iteration        *int
	Model            string
	PromptTokens     int
	CompletionTokens int
	DurationMs       int
	Redactions       int
	Finish           string
}

func renderExecutionExchanges(out io.Writer, executionID, stepID string, rows func(func(exchangeRow))) error {
	if _, err := fmt.Fprintf(out, "execution %s  step %s\n", executionID, stepID); err != nil {
		return err
	}
	n := 0
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SEQ\tITER\tMODEL\tPROMPT\tCOMPLETION\tDURATION\tREDACTED\tFINISH")
	rows(func(x exchangeRow) {
		n++
		iter := "-"
		if x.Iteration != nil {
			iter = strconv.Itoa(*x.Iteration)
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%d\t%dms\t%d\t%s\n", x.Seq, iter, x.Model, x.PromptTokens, x.CompletionTokens, x.DurationMs, x.Redactions, x.Finish)
	})
	if err := tw.Flush(); err != nil {
		return err
	}
	if n == 0 {
		_, err := fmt.Fprintln(out, "(no exchanges recorded — the project has recording.llm_exchanges off, or the step ran before it was set)")
		return err
	}
	return nil
}

// executionStepFileExport is the --export path shared by `execution input`
// and `execution result` (one flag variable: the two verbs never run in the
// same process).
var executionStepFileExport string

var executionInputCmd = &cobra.Command{
	Use:   "input <executionId> <stepId>",
	Short: "Print the task.json the executor handed the step's container",
	Long: `Print the step's input file as the daemon stored it — the task.json the
executor wrote for the container, redacted at write (step-I/O persistence design
§5). A [REDACTED:type] marker stands where the container saw a value; do not read
the stored file as what the container saw where one appears. Bare JSON on stdout
so it pipes into jq; --export writes it 0600. Empty for a step run before the
daemon persisted boundary files, and for a step no container ran.`,
	Args: cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		return runExecutionStepFile("input", args[0], args[1], executionStepFileExport, os.Stdout)
	},
}

var executionResultCmd = &cobra.Command{
	Use:   "result <executionId> <stepId>",
	Short: "Print the result.json the step's container handed back",
	Long: `Print the step's result file as the daemon read it back — whole, after the
result_json secrets checkpoint, even when it did not parse (that is the case
where keeping it has value). Bare JSON on stdout so it pipes into jq; --export
writes it 0600, which is how a replay fixture's expected_result.json is taken
from the production run rather than from the first replay.`,
	Args: cobra.ExactArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		return runExecutionStepFile("result", args[0], args[1], executionStepFileExport, os.Stdout)
	},
}

// runExecutionStepFile fetches one boundary file and hands it to
// emitStepFile.
func runExecutionStepFile(part, executionID, stepID, export string, out io.Writer) error {
	raw, err := fetchJSON(fmt.Sprintf("/api/v1/executions/%s/steps/%s/%s", executionID, stepID, part))
	if err != nil {
		return err
	}
	return emitStepFile(out, raw, export)
}

// emitStepFile prints the bytes (with a trailing newline when the file lacks
// one) or writes them to export with 0600 and says how much it wrote. The
// stored file is served verbatim, never re-encoded: a fixture's task.json is
// the bytes the container read, and a re-marshalled copy would not be.
func emitStepFile(out io.Writer, raw []byte, export string) error {
	if export != "" {
		if err := os.WriteFile(export, raw, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", export, err)
		}
		_, _ = fmt.Fprintf(out, "wrote %d bytes to %s\n", len(raw), export)
		return nil
	}
	if _, err := out.Write(raw); err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		_, _ = fmt.Fprintln(out)
	}
	return nil
}
