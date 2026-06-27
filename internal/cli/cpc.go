package cli

// Cross-project call CLI — inter-project orchestration Phase D
// operator surface (LLD §11 follow-on). Mirrors the daemon's
// /api/v1/admin/cpc endpoints. Three verbs:
//
//   vornikctl cpc list   [--status STATUS] [--caller PROJECT]
//                       [--callee PROJECT] [--since YYYY-MM-DD]
//                       [--limit N] [--json]
//   vornikctl cpc show   <id> [--json]
//   vornikctl cpc cancel <id> --reason TEXT [--json]
//
// All three require an admin-scoped API key (gated server-
// side by /admin/cpc's admin middleware). Without admin scope
// the daemon returns 403 — the CLI translates to a clear
// error so operators understand the gate.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	cpcCmd = &cobra.Command{
		Use:   "cpc",
		Short: "Cross-project call (inter-project orchestration) administration",
		Long: `Inspect + force-resolve rows in the cross_project_calls ledger.

Used to recover from stuck cross-project workflows when a callee
project's task hangs indefinitely without hitting its timeout, or to
audit which projects have been delegating to which over a time window.

Requires an admin-scoped API key (configured via admin.allowed_keys
in the daemon's config.yaml). Calls are served by the daemon's
/api/v1/admin/cpc endpoints.`,
	}

	cpcListCmd = &cobra.Command{
		Use:   "list",
		Short: "List cross-project calls (filterable by status, project, age)",
		Long: `Show rows from the cross_project_calls ledger, newest first.

Filters compose with AND. Default limit 50; max 1000.

Common queries:
  vornikctl cpc list --status running                # rows still in flight
  vornikctl cpc list --status timed_out --since 2026-05-20
  vornikctl cpc list --caller marketing --callee architect`,
		RunE: runCPCList,
	}

	cpcShowCmd = &cobra.Command{
		Use:   "show <id>",
		Short: "Show a single cross-project call by id",
		Args:  cobra.ExactArgs(1),
		RunE:  runCPCShow,
	}

	cpcCancelCmd = &cobra.Command{
		Use:   "cancel <id>",
		Short: "Force-resolve a stuck cross-project call (admin-only)",
		Long: `Flip a pending or running cross_project_calls row to status=rejected
with the supplied --reason. The caller task wakes from WAITING_FOR_CHILDREN
and takes its on_fail branch.

This does NOT cancel the callee task — that work may still be useful;
the operator decides whether to cancel it separately via the callee
project's task surface.

Idempotent: cancelling an already-terminal row is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: runCPCCancel,
	}

	cpcListStatus   string
	cpcListCaller   string
	cpcListCallee   string
	cpcListSince    string
	cpcListLimit    int
	cpcListJSON     bool
	cpcShowJSON     bool
	cpcCancelReason string
	cpcCancelJSON   bool
)

func init() {
	cpcListCmd.Flags().StringVar(&cpcListStatus, "status", "", "Filter by status (pending|running|completed|failed|timed_out|rejected)")
	cpcListCmd.Flags().StringVar(&cpcListCaller, "caller", "", "Filter by caller project ID")
	cpcListCmd.Flags().StringVar(&cpcListCallee, "callee", "", "Filter by callee project ID")
	cpcListCmd.Flags().StringVar(&cpcListSince, "since", "", "Only rows created at or after this timestamp (YYYY-MM-DD or RFC3339)")
	cpcListCmd.Flags().IntVarP(&cpcListLimit, "limit", "n", 50, "Maximum rows to return (1-1000)")
	cpcListCmd.Flags().BoolVar(&cpcListJSON, "json", false, "Output JSON instead of table")

	cpcShowCmd.Flags().BoolVar(&cpcShowJSON, "json", false, "Output JSON instead of human-readable")

	cpcCancelCmd.Flags().StringVar(&cpcCancelReason, "reason", "", "Operator-supplied reason (recorded in the audit row); defaults to a generic note")
	cpcCancelCmd.Flags().BoolVar(&cpcCancelJSON, "json", false, "Output JSON instead of human-readable")

	cpcCmd.AddCommand(cpcListCmd)
	cpcCmd.AddCommand(cpcShowCmd)
	cpcCmd.AddCommand(cpcCancelCmd)
	rootCmd.AddCommand(cpcCmd)
}

// cpcEntry mirrors api.CPCEntryJSON. Kept here as a local
// shape so the CLI doesn't import the api package (cycle
// concern — the CLI is a separate binary).
type cpcEntry struct {
	ID             string `json:"id"`
	CallerTaskID   string `json:"caller_task_id"`
	CallerStepID   string `json:"caller_step_id"`
	CallerProject  string `json:"caller_project"`
	CalleeProject  string `json:"callee_project"`
	CalleeWorkflow string `json:"callee_workflow"`
	CalleeTaskID   string `json:"callee_task_id,omitempty"`
	ExpectedSchema string `json:"expected_schema,omitempty"`
	Status         string `json:"status"`
	ErrorMessage   string `json:"error_message,omitempty"`
	TimeoutAt      string `json:"timeout_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	ResolvedAt     string `json:"resolved_at,omitempty"`
}

type cpcListResponse struct {
	Entries []cpcEntry `json:"entries"`
}

type cpcCancelResponse struct {
	Entry cpcEntry `json:"entry"`
}

func runCPCList(_ *cobra.Command, _ []string) error {
	q := url.Values{}
	if cpcListStatus != "" {
		q.Set("status", cpcListStatus)
	}
	if cpcListCaller != "" {
		q.Set("caller", cpcListCaller)
	}
	if cpcListCallee != "" {
		q.Set("callee", cpcListCallee)
	}
	if cpcListSince != "" {
		q.Set("since", cpcListSince)
	}
	if cpcListLimit > 0 {
		q.Set("limit", strconv.Itoa(cpcListLimit))
	}
	path := "/api/v1/admin/cpc"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}

	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("cpc list: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}

	var out cpcListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("cpc list: decode failed: %w", err)
	}
	if cpcListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(out.Entries) == 0 {
		fmt.Println("No cross-project calls match the filter.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tCALLER\tCALLEE\tWORKFLOW\tCREATED\tRESOLVED"); err != nil {
		return err
	}
	for _, e := range out.Entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(e.ID, 24),
			e.Status,
			truncate(e.CallerProject, 18),
			truncate(e.CalleeProject, 18),
			truncate(e.CalleeWorkflow, 22),
			truncate(e.CreatedAt, 19),
			truncate(e.ResolvedAt, 19)); err != nil {
			return err
		}
	}
	return nil
}

func runCPCShow(_ *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/admin/cpc/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("cpc show: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out cpcCancelResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("cpc show: decode failed: %w", err)
	}
	if cpcShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out.Entry)
	}
	printCPCRow(out.Entry)
	return nil
}

func runCPCCancel(_ *cobra.Command, args []string) error {
	id := args[0]
	body := map[string]string{}
	if cpcCancelReason != "" {
		body["reason"] = cpcCancelReason
	}
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/admin/cpc/"+url.PathEscape(id)+"/cancel", body)
	if err != nil {
		return fmt.Errorf("cpc cancel: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out cpcCancelResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("cpc cancel: decode failed: %w", err)
	}
	if cpcCancelJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out.Entry)
	}
	fmt.Printf("Cancelled %s. New status: %s\n", out.Entry.ID, out.Entry.Status)
	if out.Entry.ErrorMessage != "" {
		fmt.Printf("Reason: %s\n", out.Entry.ErrorMessage)
	}
	return nil
}

// printCPCRow renders a single row in the human-readable
// "field: value" shape used by `cpc show`.
func printCPCRow(e cpcEntry) {
	fmt.Printf("ID:              %s\n", e.ID)
	fmt.Printf("Status:          %s\n", e.Status)
	fmt.Printf("Caller project:  %s\n", e.CallerProject)
	fmt.Printf("Caller task:     %s\n", e.CallerTaskID)
	fmt.Printf("Caller step:     %s\n", e.CallerStepID)
	fmt.Printf("Callee project:  %s\n", e.CalleeProject)
	fmt.Printf("Callee workflow: %s\n", e.CalleeWorkflow)
	if e.CalleeTaskID != "" {
		fmt.Printf("Callee task:     %s\n", e.CalleeTaskID)
	}
	if e.ExpectedSchema != "" {
		fmt.Printf("Expected schema: %s\n", e.ExpectedSchema)
	}
	fmt.Printf("Created:         %s\n", e.CreatedAt)
	if e.TimeoutAt != "" {
		fmt.Printf("Timeout at:      %s\n", e.TimeoutAt)
	}
	if e.ResolvedAt != "" {
		fmt.Printf("Resolved:        %s\n", e.ResolvedAt)
	}
	if e.ErrorMessage != "" {
		fmt.Printf("Error message:   %s\n", e.ErrorMessage)
	}
}
