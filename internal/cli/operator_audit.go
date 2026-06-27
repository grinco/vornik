package cli

// `vornikctl operator audit <operator-id>` — the read side of
// the Phase-B profile-use audit feature. Backs the daemon's
// GET /api/v1/operators/{id}/audit endpoint.
//
// Operators query this to answer "when did the model start
// using my 'prefers Czech' preference, and is that the right
// call?". The row is written by the dispatcher every turn whose
// system prompt carried a non-empty <operator_profile> block.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	operatorAuditLimit int
	operatorAuditSince string
	operatorAuditUntil string
	operatorAuditJSON  bool

	operatorAuditCmd = &cobra.Command{
		Use:   "audit <operator-id>",
		Short: "Show per-turn profile-use audit rows for one operator",
		Long: `Lists every turn whose dispatcher injected a non-empty
<operator_profile> block for the named operator. Newest-first.

Each row records which structured keys influenced the prompt
plus a flag for whether the free-form notes column was used.
The dispatcher writes one row per chat turn whose system prompt
carried a profile block; the audit table is the receipt that
the model SAW the field, complementing the citation markers in
the reply that show whether the model actually CITED it.

  vornikctl operator audit telegram:42 --limit 20
  vornikctl operator audit web:abc --since 2026-05-01T00:00:00Z --json`,
		Args: cobra.ExactArgs(1),
		RunE: runOperatorAudit,
	}
)

func init() {
	operatorAuditCmd.Flags().IntVar(&operatorAuditLimit, "limit", 50, "Maximum rows (1-500)")
	operatorAuditCmd.Flags().StringVar(&operatorAuditSince, "since", "", "Only rows at or after this RFC3339 timestamp (e.g. 2026-05-01T00:00:00Z)")
	operatorAuditCmd.Flags().StringVar(&operatorAuditUntil, "until", "", "Only rows at or before this RFC3339 timestamp")
	operatorAuditCmd.Flags().BoolVar(&operatorAuditJSON, "json", false, "Output JSON instead of table")
	operatorCmd.AddCommand(operatorAuditCmd)
}

// operatorAuditEntry mirrors api.ProfileUseAuditEntryJSON.
type operatorAuditEntry struct {
	ID         int64    `json:"id"`
	OperatorID string   `json:"operator_id"`
	TaskID     string   `json:"task_id,omitempty"`
	UsedKeys   []string `json:"used_keys"`
	UsedNotes  bool     `json:"used_notes"`
	CreatedAt  string   `json:"created_at"`
}

type operatorAuditResponse struct {
	Entries []operatorAuditEntry `json:"entries"`
}

func runOperatorAudit(cmd *cobra.Command, args []string) error {
	id := strings.TrimSpace(args[0])
	if id == "" {
		return fmt.Errorf("operator audit: operator id required")
	}
	if operatorAuditLimit < 1 || operatorAuditLimit > 500 {
		return fmt.Errorf("operator audit: --limit must be 1..500 (got %d)", operatorAuditLimit)
	}
	if operatorAuditSince != "" {
		if _, err := time.Parse(time.RFC3339, operatorAuditSince); err != nil {
			return fmt.Errorf("operator audit: --since must be RFC3339 (got %q)", operatorAuditSince)
		}
	}
	if operatorAuditUntil != "" {
		if _, err := time.Parse(time.RFC3339, operatorAuditUntil); err != nil {
			return fmt.Errorf("operator audit: --until must be RFC3339 (got %q)", operatorAuditUntil)
		}
	}
	params := url.Values{}
	if operatorAuditLimit != 0 {
		params.Set("limit", fmt.Sprintf("%d", operatorAuditLimit))
	}
	if operatorAuditSince != "" {
		params.Set("since", operatorAuditSince)
	}
	if operatorAuditUntil != "" {
		params.Set("until", operatorAuditUntil)
	}
	path := "/api/v1/operators/" + url.PathEscape(id) + "/audit"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("operator audit: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	var out operatorAuditResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("operator audit: decode response: %w", err)
	}
	if operatorAuditJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(out.Entries) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No profile-use audit rows for %s.\n", id)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CREATED AT\tUSED KEYS\tNOTES\tTASK")
	for _, e := range out.Entries {
		notes := "no"
		if e.UsedNotes {
			notes = "yes"
		}
		keys := strings.Join(e.UsedKeys, ",")
		if keys == "" {
			keys = "—"
		}
		task := e.TaskID
		if task == "" {
			task = "—"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.CreatedAt, keys, notes, task)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nTotal: %d row(s).\n", len(out.Entries))
	return nil
}
