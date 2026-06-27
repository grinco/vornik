package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type webhookEventResponse struct {
	ID           string  `json:"id"`
	ProjectID    string  `json:"project_id"`
	Source       string  `json:"source"`
	EventID      string  `json:"event_id"`
	PayloadHash  string  `json:"payload_hash"`
	Status       string  `json:"status"`
	TaskID       *string `json:"task_id,omitempty"`
	ErrorCode    string  `json:"error_code,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

type listWebhookEventsResponse struct {
	Events []webhookEventResponse `json:"events"`
	Total  int                    `json:"total"`
}

var webhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Inspect webhook ingress",
	Long:  "Inspect signed webhook ingress events recorded by the vornik control plane.",
}

var webhookEventsCmd = &cobra.Command{
	Use:   "events",
	Short: "List webhook events",
	Long:  "List accepted, rejected, and duplicate webhook ingress events for a project.",
	RunE:  runWebhookEvents,
}

var (
	webhookEventsProject string
	webhookEventsSource  string
	webhookEventsStatus  string
	webhookEventsLimit   int
	webhookEventsJSON    bool
)

func init() {
	webhookEventsCmd.Flags().StringVarP(&webhookEventsProject, "project", "p", "", "Project ID (required)")
	webhookEventsCmd.Flags().StringVar(&webhookEventsSource, "source", "", "Filter by webhook source")
	webhookEventsCmd.Flags().StringVar(&webhookEventsStatus, "status", "", "Filter by status (accepted, rejected, duplicate)")
	webhookEventsCmd.Flags().IntVarP(&webhookEventsLimit, "limit", "n", 50, "Maximum number of events to show")
	webhookEventsCmd.Flags().BoolVar(&webhookEventsJSON, "json", false, "Output in JSON format")
	_ = webhookEventsCmd.MarkFlagRequired("project")

	webhookCmd.AddCommand(webhookEventsCmd)
	rootCmd.AddCommand(webhookCmd)
}

func runWebhookEvents(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	values := url.Values{}
	if webhookEventsSource != "" {
		values.Set("source", webhookEventsSource)
	}
	if webhookEventsStatus != "" {
		values.Set("status", webhookEventsStatus)
	}
	if webhookEventsLimit > 0 {
		values.Set("limit", fmt.Sprintf("%d", webhookEventsLimit))
	}
	path := fmt.Sprintf("/api/v1/projects/%s/webhooks/events", webhookEventsProject)
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}

	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("failed to list webhook events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	var result listWebhookEventsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	if webhookEventsJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TIME\tSTATUS\tSOURCE\tEVENT ID\tTASK ID\tERROR")
	for _, event := range result.Events {
		taskID := "-"
		if event.TaskID != nil && *event.TaskID != "" {
			taskID = *event.TaskID
		}
		errText := event.ErrorCode
		if event.ErrorMessage != "" {
			if errText != "" {
				errText += ": "
			}
			errText += event.ErrorMessage
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			event.CreatedAt,
			event.Status,
			event.Source,
			event.EventID,
			taskID,
			errText,
		)
	}
	_ = w.Flush()
	fmt.Printf("\nTotal: %d\n", result.Total)
	return nil
}
