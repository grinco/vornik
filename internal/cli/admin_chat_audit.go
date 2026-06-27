package cli

// `vornikctl admin chat-audit` — CLI mirror of /ui/admin/chat-audit.
// Reads the same chat_audit_log rows the UI surfaces; one row per
// dispatcher turn. Pairs with `vornikctl admin audit` (operator
// config actions) and `vornikctl operator audit` (operator-keyed
// profile-use rows).

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	adminChatAuditCmd = &cobra.Command{
		Use:   "chat-audit",
		Short: "List recent dispatcher chat turns",
		Long: `Show recent rows from the chat_audit_log table. Mirrors the
/ui/admin/chat-audit page over the data-plane API. Requires an admin-scoped
API key.

Filters compose with AND semantics. Default limit is 50; max is 500.`,
		Example: `  vornikctl admin chat-audit --limit 20
  vornikctl admin chat-audit --chat telegram:42
  vornikctl admin chat-audit --project assistant --json`,
		RunE: runAdminChatAudit,
	}

	adminChatAuditLimit   int
	adminChatAuditChat    string
	adminChatAuditProject string
	adminChatAuditSince   string
	adminChatAuditJSON    bool
)

func init() {
	adminChatAuditCmd.Flags().IntVarP(&adminChatAuditLimit, "limit", "n", 50, "Maximum rows to return (1-500)")
	adminChatAuditCmd.Flags().StringVar(&adminChatAuditChat, "chat", "", "Filter by chat id (exact match)")
	adminChatAuditCmd.Flags().StringVar(&adminChatAuditProject, "project", "", "Filter by project id (exact match)")
	adminChatAuditCmd.Flags().StringVar(&adminChatAuditSince, "since", "", "Lower bound on timestamp (RFC3339 or YYYY-MM-DD)")
	adminChatAuditCmd.Flags().BoolVar(&adminChatAuditJSON, "json", false, "Output JSON instead of table")

	adminCmd.AddCommand(adminChatAuditCmd)
}

type chatAuditRow struct {
	ID                       string  `json:"id"`
	Timestamp                string  `json:"ts"`
	ChatID                   string  `json:"chat_id,omitempty"`
	UserID                   string  `json:"user_id,omitempty"`
	ProjectID                string  `json:"project_id,omitempty"`
	RoleUsed                 string  `json:"role_used,omitempty"`
	Model                    string  `json:"model,omitempty"`
	SystemPromptHash         string  `json:"system_prompt_hash,omitempty"`
	UserMessage              string  `json:"user_message,omitempty"`
	ToolCallsJSON            string  `json:"tool_calls_json,omitempty"`
	Response                 string  `json:"response,omitempty"`
	Iterations               int     `json:"iterations,omitempty"`
	DurationMs               int64   `json:"duration_ms,omitempty"`
	CostUSD                  float64 `json:"cost_usd,omitempty"`
	HallucinationSignalsJSON string  `json:"hallucination_signals_json,omitempty"`
}

type chatAuditResponse struct {
	Entries []chatAuditRow `json:"entries"`
}

func runAdminChatAudit(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	if adminChatAuditChat != "" {
		q.Set("chat", adminChatAuditChat)
	}
	if adminChatAuditProject != "" {
		q.Set("project", adminChatAuditProject)
	}
	if adminChatAuditSince != "" {
		q.Set("since", adminChatAuditSince)
	}
	if adminChatAuditLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", adminChatAuditLimit))
	}
	path := "/api/v1/admin/chat-audit"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("admin chat-audit: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}

	var out chatAuditResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("admin chat-audit: decode failed: %w", err)
	}

	if adminChatAuditJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(out.Entries) == 0 {
		fmt.Println("No chat audit entries found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "TIME\tCHAT\tPROJECT\tROLE\tMODEL\tITERS\tDUR_MS\tCOST_USD\tRESPONSE"); err != nil {
		return err
	}
	for _, e := range out.Entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%.4f\t%s\n",
			truncate(e.Timestamp, 19),
			truncate(e.ChatID, 18),
			truncate(e.ProjectID, 16),
			truncate(e.RoleUsed, 12),
			truncate(e.Model, 24),
			e.Iterations,
			e.DurationMs,
			e.CostUSD,
			truncate(e.Response, 60),
		); err != nil {
			return err
		}
	}
	return nil
}
