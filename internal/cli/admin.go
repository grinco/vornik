package cli

// Admin CLI — admin-ui-design.md slice 1. Mirrors the
// /ui/admin/audit table over the data-plane HTTP API. Other
// admin verbs land in slice 2+.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	adminCmd = &cobra.Command{
		Use:   "admin",
		Short: "Daemon-level administrative commands",
		Long: `Surfaces the daemon's admin audit trail and (in later slices)
configuration / lifecycle controls. Requires an admin-scoped API key
configured via admin.allowed_keys in the daemon's config.yaml.`,
	}

	adminAuditCmd = &cobra.Command{
		Use:   "audit",
		Short: "List recent admin actions",
		Long: `Show recent rows from the admin_audit table. Mirrors the
/ui/admin/audit page over the data-plane API. Requires an admin-scoped
API key.

Filters compose with AND semantics. Default limit is 50; max is 500.`,
		RunE: runAdminAudit,
	}

	adminAuditLimit     int
	adminAuditPrincipal string
	adminAuditAction    string
	adminAuditTarget    string
	adminAuditJSON      bool
)

func init() {
	adminAuditCmd.Flags().IntVarP(&adminAuditLimit, "limit", "n", 50, "Maximum rows to return (1-500)")
	adminAuditCmd.Flags().StringVar(&adminAuditPrincipal, "principal", "", "Filter by principal (exact match)")
	adminAuditCmd.Flags().StringVar(&adminAuditAction, "action", "", "Filter by action verb (e.g. mcp.refresh)")
	adminAuditCmd.Flags().StringVar(&adminAuditTarget, "target", "", "Filter by target prefix")
	adminAuditCmd.Flags().BoolVar(&adminAuditJSON, "json", false, "Output JSON instead of table")

	adminCmd.AddCommand(adminAuditCmd)
	rootCmd.AddCommand(adminCmd)
}

type adminAuditRow struct {
	ID        string `json:"id"`
	Timestamp string `json:"ts"`
	Principal string `json:"principal"`
	Source    string `json:"source"`
	Action    string `json:"action"`
	Target    string `json:"target,omitempty"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
	IP        string `json:"ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

type adminAuditResponse struct {
	Entries []adminAuditRow `json:"entries"`
}

func runAdminAudit(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	if adminAuditAction != "" {
		q.Set("action", adminAuditAction)
	}
	if adminAuditPrincipal != "" {
		q.Set("principal", adminAuditPrincipal)
	}
	if adminAuditTarget != "" {
		q.Set("target", adminAuditTarget)
	}
	if adminAuditLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", adminAuditLimit))
	}
	path := "/api/v1/admin/audit"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("admin audit: request failed: %w", err)
	}
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()

	var out adminAuditResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("admin audit: decode failed: %w", err)
	}

	if adminAuditJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(out.Entries) == 0 {
		fmt.Println("No admin audit entries found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "TIME\tACTION\tSRC\tPRINCIPAL\tTARGET\tIP"); err != nil {
		return err
	}
	for _, e := range out.Entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(e.Timestamp, 19),
			truncate(e.Action, 24),
			e.Source,
			truncate(e.Principal, 24),
			truncate(e.Target, 30),
			e.IP); err != nil {
			return err
		}
	}
	return nil
}

// truncate clips s to maxLen runes — keeps the audit table from
// degenerating on long IDs / IP6 addresses. Maintains string
// integrity by slicing on byte boundaries; the audit fields are
// ASCII in every realistic case (timestamps, action verbs, IDs).
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return s[:maxLen]
	}
	return s[:maxLen-1] + "…"
}
