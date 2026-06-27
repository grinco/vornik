package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// explainResponseCLI mirrors api.explainResponse. Kept private to
// the CLI so the daemon owns the wire shape; new fields surface
// automatically via the json decoder skipping unknown keys.
type explainResponseCLI struct {
	Summary string                 `json:"summary"`
	Inputs  map[string]interface{} `json:"inputs"`
}

var (
	explainProject  string
	explainJSON     bool
	explainShowData bool
)

var taskExplainCmd = &cobra.Command{
	Use:   "explain <task-id>",
	Short: "Generate a post-mortem explanation for a terminal task",
	Long: `Joins the task's failure context (last error, step outcomes, recent
tool calls, container log tail) and asks the configured chat
provider for an operator-friendly paragraph explaining what went
wrong and what to try next.

By default prints just the summary paragraph. Pass --show-data to
also dump the structured inputs the model saw, or --json for the
full machine-readable response.`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskExplain,
}

func init() {
	taskExplainCmd.Flags().StringVarP(&explainProject, "project", "p", "", "Project ID (required)")
	taskExplainCmd.Flags().BoolVar(&explainJSON, "json", false, "Output in JSON format")
	taskExplainCmd.Flags().BoolVar(&explainShowData, "show-data", false, "Print the structured inputs the model saw alongside the summary")
	_ = taskExplainCmd.MarkFlagRequired("project")
	taskCmd.AddCommand(taskExplainCmd)
}

func runTaskExplain(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	if taskID == "" {
		return fmt.Errorf("task ID is required")
	}
	if explainProject == "" {
		return fmt.Errorf("--project is required")
	}

	client := ClientFromEnv()
	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/explain",
		url.PathEscape(explainProject), url.PathEscape(taskID))
	resp, err := client.Post(path, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var report explainResponseCLI
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if explainJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	fmt.Println(strings.TrimSpace(report.Summary))

	if explainShowData {
		fmt.Println()
		fmt.Println("--- structured inputs the model saw ---")
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report.Inputs)
	}
	return nil
}
