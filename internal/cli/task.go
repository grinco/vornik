package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Task responses from the API
type taskResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	ProjectID string `json:"projectId"`
	TaskType  string `json:"taskType"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"createdAt"`
	LastError string `json:"lastError,omitempty"`
}

type listTasksResponse struct {
	Tasks []taskResponse `json:"tasks"`
	Total int            `json:"total"`
}

type cancelTaskResponse struct {
	TaskID      string `json:"taskId"`
	Status      string `json:"status"`
	WasRunning  bool   `json:"wasRunning"`
	CancelledAt string `json:"cancelledAt"`
}

type retryTaskResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	Attempt   int    `json:"attempt"`
	RetriedAt string `json:"retriedAt"`
}

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage tasks",
	Long:  "Inspect, list, cancel, and retry tasks in the vornik control plane.",
}

var taskListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks",
	Long:  "List tasks for a project, optionally filtered by status.",
	RunE:  runTaskList,
}

var taskCancelCmd = &cobra.Command{
	Use:   "cancel <taskId>",
	Short: "Cancel a task",
	Long:  "Cancel a task by ID. Only tasks in QUEUED, LEASED, RUNNING, or PENDING status can be cancelled.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskCancel,
}

var taskRetryCmd = &cobra.Command{
	Use:   "retry <taskId>",
	Short: "Retry a task",
	Long:  "Retry a failed or cancelled task by ID. The task is re-queued for execution.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskRetry,
}

var taskTailCmd = &cobra.Command{
	Use:   "tail <taskId>",
	Short: "Tail task logs",
	Long:  "Print current container logs for a running task, or the latest persisted failure/result excerpt after completion.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskTail,
}

var taskSubmitCmd = &cobra.Command{
	Use:   "submit",
	Short: "Submit a new task",
	Long: `Submit a new task to a project's queue. The --prompt flag is the
operator-friendly shortcut for the context.prompt payload shape every
researcher role already reads; for custom shapes use --context-json.

Examples:
  vornikctl task submit -p janka --prompt "Summarise yesterday's scans"
  vornikctl task submit -p snake --task-type feature --prompt "Implement X"
  vornikctl task submit -p n8n-agents --workflow adaptive --prompt "..." --priority 10`,
	RunE: runTaskSubmit,
}

var taskGetCmd = &cobra.Command{
	Use:   "get <taskId>",
	Short: "Get a single task's details",
	Long:  "Fetch one task by ID. Prints the task envelope (status, workflow, timestamps) and, with --json, the full API response including payload.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskGet,
}

// tailCmd is a top-level alias for `vornikctl task tail`. Both share the
// same RunE so behaviour stays identical; keeping the alias avoids
// breaking scripts that pre-date the move into the task subtree.
var tailCmd = &cobra.Command{
	Use:   "tail <taskId>",
	Short: "Tail task logs (alias for `vornikctl task tail`)",
	Long:  "Alias for `vornikctl task tail`. Prints current container logs for a running task, or the latest persisted failure/result excerpt after completion.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskTail,
}

var (
	taskListProject    string
	taskListStatus     string
	taskListJSON       bool
	taskCancelProject  string
	taskRetryProject   string
	taskRetryResetFlag bool
	taskTailProject    string
	taskTailLines      int
	taskTailFollow     bool

	taskSubmitProject        string
	taskSubmitWorkflow       string
	taskSubmitTaskType       string
	taskSubmitPriority       int
	taskSubmitPrompt         string
	taskSubmitContextJSON    string
	taskSubmitIdempotencyKey string
	taskSubmitJSON           bool
	taskSubmitAttach         []string // file paths to attach via inputArtifacts

	taskGetProject string
	taskGetJSON    bool
)

func init() {
	// task list flags
	taskListCmd.Flags().StringVarP(&taskListProject, "project", "p", "", "Project ID (required)")
	taskListCmd.Flags().StringVarP(&taskListStatus, "status", "s", "", "Filter by status (PENDING, QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED)")
	taskListCmd.Flags().BoolVar(&taskListJSON, "json", false, "Output in JSON format")
	_ = taskListCmd.MarkFlagRequired("project")

	// task cancel flags
	taskCancelCmd.Flags().StringVarP(&taskCancelProject, "project", "p", "", "Project ID (required)")
	_ = taskCancelCmd.MarkFlagRequired("project")

	// task retry flags
	taskRetryCmd.Flags().StringVarP(&taskRetryProject, "project", "p", "", "Project ID (required)")
	taskRetryCmd.Flags().BoolVar(&taskRetryResetFlag, "reset-attempts", false, "Reset attempt counter to 1")
	_ = taskRetryCmd.MarkFlagRequired("project")

	taskTailCmd.Flags().StringVarP(&taskTailProject, "project", "p", "", "Project ID (required)")
	taskTailCmd.Flags().IntVarP(&taskTailLines, "lines", "n", 200, "Number of lines to show")
	taskTailCmd.Flags().BoolVarP(&taskTailFollow, "follow", "f", false, "Poll and print appended log lines")
	_ = taskTailCmd.MarkFlagRequired("project")
	tailCmd.Flags().StringVarP(&taskTailProject, "project", "p", "", "Project ID (required)")
	tailCmd.Flags().IntVarP(&taskTailLines, "lines", "n", 200, "Number of lines to show")
	tailCmd.Flags().BoolVarP(&taskTailFollow, "follow", "f", false, "Poll and print appended log lines")
	_ = tailCmd.MarkFlagRequired("project")

	// task submit flags
	taskSubmitCmd.Flags().StringVarP(&taskSubmitProject, "project", "p", "", "Project ID (required)")
	taskSubmitCmd.Flags().StringVar(&taskSubmitWorkflow, "workflow", "", "Override the project's default workflow")
	taskSubmitCmd.Flags().StringVar(&taskSubmitTaskType, "task-type", "research", "Task type label (free-form; surfaces in `task list`)")
	taskSubmitCmd.Flags().IntVar(&taskSubmitPriority, "priority", 0, "Priority 0-100; 0 uses the project's default")
	taskSubmitCmd.Flags().StringVar(&taskSubmitPrompt, "prompt", "", "Task prompt — shorthand for --context-json '{\"prompt\":\"...\"}'")
	taskSubmitCmd.Flags().StringVar(&taskSubmitContextJSON, "context-json", "", "Raw JSON object to set as the task context (mutually exclusive with --prompt)")
	taskSubmitCmd.Flags().StringVar(&taskSubmitIdempotencyKey, "idempotency-key", "", "Optional idempotency key; duplicate submits return the existing task")
	taskSubmitCmd.Flags().StringSliceVar(&taskSubmitAttach, "attach", nil, "Attach a file as an input artifact (snapshotted + auto-extracted into project memory). Repeatable; e.g. --attach book.epub --attach paper.pdf")
	taskSubmitCmd.Flags().BoolVar(&taskSubmitJSON, "json", false, "Output the raw API response instead of the human summary")
	_ = taskSubmitCmd.MarkFlagRequired("project")

	// task get flags
	taskGetCmd.Flags().StringVarP(&taskGetProject, "project", "p", "", "Project ID (required)")
	taskGetCmd.Flags().BoolVar(&taskGetJSON, "json", false, "Output in JSON format")
	_ = taskGetCmd.MarkFlagRequired("project")

	// Add subcommands
	taskCmd.AddCommand(taskListCmd)
	taskCmd.AddCommand(taskCancelCmd)
	taskCmd.AddCommand(taskRetryCmd)
	taskCmd.AddCommand(taskTailCmd)
	taskCmd.AddCommand(taskSubmitCmd)
	taskCmd.AddCommand(taskGetCmd)

	// Add to root
	rootCmd.AddCommand(taskCmd)
	rootCmd.AddCommand(tailCmd)
}

// runTaskSubmit wraps POST /api/v1/projects/{p}/tasks with an operator-
// friendly flag surface. --prompt is the common case and gets wrapped
// into the canonical {prompt: ...} context shape; --context-json lets
// power users send any shape the agent runtime understands.
func runTaskSubmit(cmd *cobra.Command, args []string) error {
	if taskSubmitPrompt != "" && taskSubmitContextJSON != "" {
		return fmt.Errorf("--prompt and --context-json are mutually exclusive")
	}

	type attachedArtifact struct {
		Name    string `json:"name"`
		Content string `json:"content"` // base64-encoded
	}
	type submitReq struct {
		TaskType       string             `json:"taskType"`
		Priority       int                `json:"priority,omitempty"`
		WorkflowID     string             `json:"workflowId,omitempty"`
		IdempotencyKey string             `json:"idempotencyKey,omitempty"`
		InputArtifacts []attachedArtifact `json:"inputArtifacts,omitempty"`
		Context        json.RawMessage    `json:"context,omitempty"`
	}
	req := submitReq{
		TaskType:       taskSubmitTaskType,
		Priority:       taskSubmitPriority,
		WorkflowID:     taskSubmitWorkflow,
		IdempotencyKey: taskSubmitIdempotencyKey,
	}
	for _, p := range taskSubmitAttach {
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read --attach %q: %w", p, err)
		}
		req.InputArtifacts = append(req.InputArtifacts, attachedArtifact{
			Name:    filepath.Base(p),
			Content: base64.StdEncoding.EncodeToString(data),
		})
	}
	switch {
	case taskSubmitPrompt != "":
		ctx := map[string]string{"prompt": taskSubmitPrompt}
		b, err := json.Marshal(ctx)
		if err != nil {
			return fmt.Errorf("encode context: %w", err)
		}
		req.Context = b
	case taskSubmitContextJSON != "":
		if !json.Valid([]byte(taskSubmitContextJSON)) {
			return fmt.Errorf("--context-json is not valid JSON")
		}
		req.Context = json.RawMessage(taskSubmitContextJSON)
	}

	client := ClientFromEnv()
	path := fmt.Sprintf("/api/v1/projects/%s/tasks", taskSubmitProject)
	resp, err := client.Post(path, req)
	if err != nil {
		return fmt.Errorf("submit task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		// Re-wrap as a fresh response so ParseAPIError sees the body.
		r := &http.Response{StatusCode: resp.StatusCode, Body: io.NopCloser(strings.NewReader(string(body)))}
		return ParseAPIError(r)
	}
	if taskSubmitJSON {
		return prettyPrintJSON(body)
	}
	var parsed struct {
		TaskID string `json:"taskId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Printf("task submitted: %s (status %s)\n", parsed.TaskID, parsed.Status)
	fmt.Printf("  tail with: vornikctl task tail %s -p %s\n", parsed.TaskID, taskSubmitProject)
	return nil
}

// runTaskGet wraps GET /api/v1/projects/{p}/tasks/{id}.
func runTaskGet(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	raw, err := fetchJSON(fmt.Sprintf("/api/v1/projects/%s/tasks/%s", taskGetProject, taskID))
	if err != nil {
		return err
	}
	if taskGetJSON {
		return prettyPrintJSON(raw)
	}
	var t taskResponse
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Printf("Task ID:    %s\n", t.TaskID)
	fmt.Printf("Project:    %s\n", t.ProjectID)
	fmt.Printf("Status:     %s\n", t.Status)
	fmt.Printf("Type:       %s\n", t.TaskType)
	fmt.Printf("Priority:   %d\n", t.Priority)
	fmt.Printf("Created:    %s\n", t.CreatedAt)
	if t.LastError != "" {
		fmt.Printf("Last error: %s\n", t.LastError)
	}
	return nil
}

func runTaskList(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()

	// Build URL with query parameters
	path := fmt.Sprintf("/api/v1/projects/%s/tasks", taskListProject)
	params := []string{}
	if taskListStatus != "" {
		params = append(params, fmt.Sprintf("status=%s", taskListStatus))
	}
	if len(params) > 0 {
		path += "?" + params[0]
		for i := 1; i < len(params); i++ {
			path += "&" + params[i]
		}
	}

	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("failed to list tasks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var result listTasksResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if taskListJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	// Table output
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TASK ID\tSTATUS\tTYPE\tPRIORITY\tCREATED AT")
	for _, task := range result.Tasks {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
			task.TaskID,
			task.Status,
			task.TaskType,
			task.Priority,
			task.CreatedAt,
		)
	}
	_ = w.Flush()

	fmt.Printf("\nTotal: %d\n", result.Total)
	return nil
}

func runTaskCancel(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	taskID := args[0]

	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/cancel", taskCancelProject, taskID)

	resp, err := client.Post(path, nil)
	if err != nil {
		return fmt.Errorf("failed to cancel task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	var result cancelTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	fmt.Printf("Task %s cancelled successfully\n", result.TaskID)
	fmt.Printf("Status: %s\n", result.Status)
	fmt.Printf("Was running: %v\n", result.WasRunning)
	fmt.Printf("Cancelled at: %s\n", result.CancelledAt)

	return nil
}

func runTaskRetry(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	taskID := args[0]

	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/retry", taskRetryProject, taskID)

	var body interface{}
	if taskRetryResetFlag {
		body = map[string]bool{"resetAttempts": true}
	}

	resp, err := client.Post(path, body)
	if err != nil {
		return fmt.Errorf("failed to retry task: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return ParseAPIError(resp)
	}

	var result retryTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	fmt.Printf("Task %s retried successfully\n", result.TaskID)
	fmt.Printf("Status: %s\n", result.Status)
	fmt.Printf("Attempt: %d\n", result.Attempt)
	fmt.Printf("Retried at: %s\n", result.RetriedAt)

	return nil
}

func runTaskTail(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	taskID := args[0]
	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/logs?tail=%d", taskTailProject, taskID, taskTailLines)

	printOnce := func(last string) (string, error) {
		resp, err := client.Get(path)
		if err != nil {
			return last, fmt.Errorf("failed to fetch logs: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return last, ParseAPIError(resp)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return last, fmt.Errorf("failed to read logs: %w", err)
		}
		text := string(data)
		if !taskTailFollow {
			fmt.Print(text)
			if text != "" && !strings.HasSuffix(text, "\n") {
				fmt.Println()
			}
			return text, nil
		}
		if strings.HasPrefix(text, last) {
			fmt.Print(strings.TrimPrefix(text, last))
		} else if text != last {
			if last != "" {
				fmt.Println("\n--- log window reset ---")
			}
			fmt.Print(text)
		}
		return text, nil
	}

	last, err := printOnce("")
	if err != nil || !taskTailFollow {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-ticker.C:
			var err error
			last, err = printOnce(last)
			if err != nil {
				return err
			}
		}
	}
}
