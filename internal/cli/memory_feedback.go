package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// memoryFeedbackResponseCLI mirrors api.memoryFeedbackResponse.
type memoryFeedbackResponseCLI struct {
	WindowDays           int      `json:"window_days"`
	TotalChunks          int      `json:"total_chunks"`
	RetrievedChunks      int      `json:"retrieved_chunks"`
	UnretrievedChunks    int      `json:"unretrieved_chunks"`
	TotalSearches        int      `json:"total_searches"`
	UnretrievedSampleIDs []string `json:"unretrieved_sample_ids"`
}

var (
	memFeedbackProject string
	memFeedbackDays    int
	memFeedbackSample  int
	memFeedbackJSON    bool
)

var memoryFeedbackCmd = &cobra.Command{
	Use:   "feedback",
	Short: "Show chunk-utility analytics for a project's memory",
	Long: `Renders memory retrieval feedback for one project: how many chunks
are indexed, how many were actually retrieved at least once in the
window, and a sample of unretrieved chunk IDs that are auto-prune
candidates.

Sources: project_memory_chunks (indexed) + memory_retrieval_audit
(per-search rows). Empty values usually mean the audit repo isn't
wired or the schema migration hasn't run yet — see
deployments/postgres/schema/001_initial.sql.`,
	RunE: runMemoryFeedback,
}

func init() {
	memoryFeedbackCmd.Flags().StringVarP(&memFeedbackProject, "project", "p", "", "Project ID (required)")
	memoryFeedbackCmd.Flags().IntVar(&memFeedbackDays, "days", 30, "Window length in days (capped at 365)")
	memoryFeedbackCmd.Flags().IntVar(&memFeedbackSample, "sample", 20, "Number of unretrieved chunk IDs to print (capped at 200)")
	memoryFeedbackCmd.Flags().BoolVar(&memFeedbackJSON, "json", false, "Output in JSON format")
	_ = memoryFeedbackCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryFeedbackCmd)
}

func runMemoryFeedback(cmd *cobra.Command, args []string) error {
	if memFeedbackProject == "" {
		return fmt.Errorf("--project is required")
	}
	q := url.Values{}
	q.Set("days", strconv.Itoa(memFeedbackDays))
	q.Set("sample", strconv.Itoa(memFeedbackSample))
	path := fmt.Sprintf("/api/v1/projects/%s/memory/feedback?%s",
		url.PathEscape(memFeedbackProject), q.Encode())

	client := ClientFromEnv()
	resp, err := client.Get(path)
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
	var report memoryFeedbackResponseCLI
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if memFeedbackJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	fmt.Printf("Project: %s · last %d days\n\n", memFeedbackProject, report.WindowDays)
	fmt.Printf("  Indexed chunks:        %d\n", report.TotalChunks)
	fmt.Printf("  Retrieved at least 1×: %d\n", report.RetrievedChunks)
	fmt.Printf("  Never retrieved:       %d   (auto-prune candidates)\n", report.UnretrievedChunks)
	fmt.Printf("  Searches in window:    %d\n", report.TotalSearches)

	if len(report.UnretrievedSampleIDs) > 0 {
		fmt.Println()
		fmt.Printf("Sample of unretrieved chunk IDs (%d shown):\n", len(report.UnretrievedSampleIDs))
		for _, id := range report.UnretrievedSampleIDs {
			fmt.Printf("  %s\n", id)
		}
	}
	return nil
}
