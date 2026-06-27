package cli

// Scheduled-reminders CLI surface (2026.7.0). Mirrors the daemon's
// /api/v1/reminders endpoints. v1 is read + cancel only; creation
// flows through the dispatcher's set_reminder tool, not the CLI.
//
//   vornikctl reminders list   [--status STATUS] [--operator OPID]
//                             [--project PID] [--limit N] [--json]
//   vornikctl reminders show   <id> [--json]
//   vornikctl reminders cancel <id> [--json]
//
// See https://docs.vornik.io

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	remindersCmd = &cobra.Command{
		Use:   "reminders",
		Short: "Inspect / cancel scheduled reminders",
		Long: `List and cancel rows in the dispatcher_reminders ledger.

Reminders are created by the dispatcher's set_reminder tool when an
operator asks the bot for one in chat. This CLI exists for terminal-
only operators (no chat session) and for cleaning up stuck rows.

Calls are served by the daemon's /api/v1/reminders endpoints.`,
	}

	remindersListCmd = &cobra.Command{
		Use:   "list",
		Short: "List reminders (filterable by status, operator, project)",
		Long: `Show rows from the dispatcher_reminders ledger, fire-time ascending.

Filters compose with AND. Default limit 50; max 500.

Common queries:
  vornikctl reminders list --status pending
  vornikctl reminders list --status pending --project assistant
  vornikctl reminders list --operator telegram:42 --status fired`,
		RunE: runRemindersList,
	}

	remindersShowCmd = &cobra.Command{
		Use:   "show <id>",
		Short: "Show a single reminder by id",
		Args:  cobra.ExactArgs(1),
		RunE:  runRemindersShow,
	}

	remindersCancelCmd = &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a pending reminder",
		Long: `Flip a pending or firing dispatcher_reminders row to status=cancelled.
The heartbeat will skip it on subsequent ticks.

Idempotent: cancelling an already-terminal row is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: runRemindersCancel,
	}

	remindersDeleteCmd = &cobra.Command{
		Use:   "delete <id>",
		Short: "Physically remove a reminder row",
		Long: `Delete the dispatcher_reminders row entirely. Distinct from
'cancel' which preserves the row for audit. Intended for operator
cleanup of stale rows — e.g. reminders that survived a project
deletion (B-12), recurring rules gone awry, test data lingering.

The row is gone after this — no recovery, no audit trail of the
content (only an admin_audit_log entry naming the deletion). Use
'cancel' instead if you want the row preserved.

Returns 404 if the id doesn't exist; idempotent for scripts that
need to ignore "already gone".`,
		Args: cobra.ExactArgs(1),
		RunE: runRemindersDelete,
	}

	remindersDeleteYes  bool
	remindersDeleteJSON bool

	remindersListStatus   string
	remindersListOperator string
	remindersListProject  string
	remindersListLimit    int
	remindersListJSON     bool
	remindersShowJSON     bool
	remindersCancelJSON   bool

	remindersScheduleOperator   string
	remindersScheduleChannel    string
	remindersScheduleChannelRef string
	remindersScheduleProject    string
	remindersScheduleTimezone   string
	remindersScheduleYes        bool
	remindersScheduleJSON       bool

	remindersScheduleCmd = &cobra.Command{
		Use:   "schedule <natural-language text>",
		Short: "Create a one-shot or recurring reminder from natural language",
		Long: `Parse free-form text into a reminder via the daemon's
natural-language parser, confirm, and commit. Supports both
one-shot ("tomorrow at 9") and recurring ("every Monday at 9am")
schedules; recurring rows carry a 5-field POSIX cron expression
and the heartbeat re-arms them after every fire.

Examples:
  vornikctl reminders schedule "remind me in 3 hours to check the deploy" \
      --operator telegram:42 --channel telegram --channel-ref 42
  vornikctl reminders schedule "every Monday at 9am send the news digest" \
      --operator telegram:42 --channel telegram --channel-ref 42
  vornikctl reminders schedule "every weekday morning until June 1 send a tick" \
      --operator webchat:abc --channel webchat --channel-ref abc \
      --timezone Europe/Prague

By default the CLI prints the parsed reminder, asks for y/N confirmation,
then commits. Pass --yes to skip the prompt (scripted use).`,
		Args: cobra.MinimumNArgs(1),
		RunE: runRemindersSchedule,
	}
)

func init() {
	remindersListCmd.Flags().StringVar(&remindersListStatus, "status", "", "Filter by status (pending|firing|fired|cancelled|expired)")
	remindersListCmd.Flags().StringVar(&remindersListOperator, "operator", "", "Filter by operator id (e.g. 'telegram:42')")
	remindersListCmd.Flags().StringVar(&remindersListProject, "project", "", "Filter by project id")
	remindersListCmd.Flags().IntVarP(&remindersListLimit, "limit", "n", 50, "Maximum rows to return (1-500)")
	remindersListCmd.Flags().BoolVar(&remindersListJSON, "json", false, "Output JSON instead of table")

	remindersShowCmd.Flags().BoolVar(&remindersShowJSON, "json", false, "Output JSON instead of human-readable")
	remindersCancelCmd.Flags().BoolVar(&remindersCancelJSON, "json", false, "Output JSON instead of human-readable")

	remindersScheduleCmd.Flags().StringVar(&remindersScheduleOperator, "operator", "", "Operator id (e.g. telegram:42) — required")
	remindersScheduleCmd.Flags().StringVar(&remindersScheduleChannel, "channel", "", "Delivery channel kind (telegram|slack|email|webchat|github) — required")
	remindersScheduleCmd.Flags().StringVar(&remindersScheduleChannelRef, "channel-ref", "", "Channel-specific delivery ref (chat_id, thread, message-id) — required")
	remindersScheduleCmd.Flags().StringVar(&remindersScheduleProject, "project", "", "Project id (optional)")
	remindersScheduleCmd.Flags().StringVar(&remindersScheduleTimezone, "timezone", "", "Operator timezone (IANA, e.g. Europe/Prague). Defaults to UTC.")
	remindersScheduleCmd.Flags().BoolVar(&remindersScheduleYes, "yes", false, "Skip the y/N confirmation prompt")
	remindersScheduleCmd.Flags().BoolVar(&remindersScheduleJSON, "json", false, "Output JSON instead of human-readable")

	remindersDeleteCmd.Flags().BoolVar(&remindersDeleteYes, "yes", false, "Skip the y/N confirmation prompt")
	remindersDeleteCmd.Flags().BoolVar(&remindersDeleteJSON, "json", false, "Output JSON instead of human-readable")

	remindersCmd.AddCommand(remindersListCmd)
	remindersCmd.AddCommand(remindersShowCmd)
	remindersCmd.AddCommand(remindersCancelCmd)
	remindersCmd.AddCommand(remindersDeleteCmd)
	remindersCmd.AddCommand(remindersScheduleCmd)
	rootCmd.AddCommand(remindersCmd)
}

// reminderEntry mirrors api.ReminderEntryJSON.
type reminderEntry struct {
	ID              string `json:"id"`
	OperatorID      string `json:"operator_id"`
	Channel         string `json:"channel"`
	ChannelRef      string `json:"channel_ref"`
	ProjectID       string `json:"project_id,omitempty"`
	FireAt          string `json:"fire_at"`
	Content         string `json:"content"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	FiredAt         string `json:"fired_at,omitempty"`
	CancelledAt     string `json:"cancelled_at,omitempty"`
	CreatedVia      string `json:"created_via"`
	ErrorCount      int    `json:"error_count,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
	RecurrenceUntil string `json:"recurrence_until,omitempty"`
}

type reminderListResponse struct {
	Entries []reminderEntry `json:"entries"`
}

func runRemindersList(_ *cobra.Command, _ []string) error {
	q := url.Values{}
	if remindersListStatus != "" {
		q.Set("status", remindersListStatus)
	}
	if remindersListOperator != "" {
		q.Set("operator", remindersListOperator)
	}
	if remindersListProject != "" {
		q.Set("project", remindersListProject)
	}
	if remindersListLimit > 0 {
		q.Set("limit", strconv.Itoa(remindersListLimit))
	}
	path := "/api/v1/reminders"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("reminders list: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out reminderListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("reminders list: decode failed: %w", err)
	}
	if remindersListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(out.Entries) == 0 {
		fmt.Println("No reminders match the filter.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tFIRE AT\tOPERATOR\tPROJECT\tCONTENT"); err != nil {
		return err
	}
	for _, e := range out.Entries {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(e.ID, 28),
			e.Status,
			truncate(e.FireAt, 25),
			truncate(e.OperatorID, 18),
			truncate(e.ProjectID, 14),
			truncate(e.Content, 50),
		); err != nil {
			return err
		}
	}
	return nil
}

func runRemindersShow(_ *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/reminders/" + url.PathEscape(args[0]))
	if err != nil {
		return fmt.Errorf("reminders show: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var e reminderEntry
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return fmt.Errorf("reminders show: decode failed: %w", err)
	}
	if remindersShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}
	printReminderRow(e)
	return nil
}

func runRemindersDelete(_ *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()

	// Read the row first so we can show it in the confirmation
	// prompt — operators routinely paste the wrong id, and
	// physically deleting the wrong reminder is silent damage.
	getResp, err := client.Get("/api/v1/reminders/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("reminders delete: fetch failed: %w", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	if getResp.StatusCode == http.StatusNotFound {
		// Idempotent: silently report missing rather than erroring.
		// Scripts that loop over a stale id list shouldn't fail.
		if remindersDeleteJSON {
			fmt.Println(`{"deleted": false, "reason": "not_found"}`)
		} else {
			fmt.Printf("reminder %s not found (already deleted?)\n", id)
		}
		return nil
	}
	if getResp.StatusCode != 200 {
		return ParseAPIError(getResp)
	}
	var e reminderEntry
	if err := json.NewDecoder(getResp.Body).Decode(&e); err != nil {
		return fmt.Errorf("reminders delete: decode failed: %w", err)
	}

	if !remindersDeleteYes {
		fmt.Println("About to physically delete this reminder (cannot be undone):")
		fmt.Println()
		printReminderRow(e)
		fmt.Println()
		fmt.Print("Delete? [y/N]: ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") &&
			!strings.EqualFold(strings.TrimSpace(answer), "yes") {
			fmt.Println("aborted")
			return nil
		}
	}

	resp, err := client.Delete("/api/v1/reminders/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("reminders delete: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return ParseAPIError(resp)
	}
	if remindersDeleteJSON {
		fmt.Printf(`{"deleted": true, "id": %q}`+"\n", id)
	} else {
		fmt.Printf("deleted %s\n", id)
	}
	return nil
}

func runRemindersCancel(_ *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/reminders/"+url.PathEscape(args[0])+"/cancel", nil)
	if err != nil {
		return fmt.Errorf("reminders cancel: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var e reminderEntry
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return fmt.Errorf("reminders cancel: decode failed: %w", err)
	}
	if remindersCancelJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}
	fmt.Printf("Cancelled %s. New status: %s\n", e.ID, e.Status)
	return nil
}

func printReminderRow(e reminderEntry) {
	fmt.Printf("ID:           %s\n", e.ID)
	fmt.Printf("Status:       %s\n", e.Status)
	fmt.Printf("Fire at:      %s\n", e.FireAt)
	if e.CronExpr != "" {
		fmt.Printf("Cron:         %s\n", e.CronExpr)
		if e.RecurrenceUntil != "" {
			fmt.Printf("Until:        %s\n", e.RecurrenceUntil)
		} else {
			fmt.Printf("Until:        (unbounded)\n")
		}
	}
	fmt.Printf("Operator:     %s\n", e.OperatorID)
	fmt.Printf("Channel:      %s (ref=%s)\n", e.Channel, e.ChannelRef)
	if e.ProjectID != "" {
		fmt.Printf("Project:      %s\n", e.ProjectID)
	}
	fmt.Printf("Created:      %s (via %s)\n", e.CreatedAt, e.CreatedVia)
	if e.FiredAt != "" {
		fmt.Printf("Fired:        %s\n", e.FiredAt)
	}
	if e.CancelledAt != "" {
		fmt.Printf("Cancelled:    %s\n", e.CancelledAt)
	}
	if e.ErrorCount > 0 {
		fmt.Printf("Errors:       %d\n", e.ErrorCount)
	}
	if e.LastError != "" {
		fmt.Printf("Last error:   %s\n", e.LastError)
	}
	fmt.Println("---")
	fmt.Println(e.Content)
}

// fromTextRequest mirrors api.FromTextRequest for the CLI's
// JSON marshaling — exporting the api package's type would
// drag the whole api dependency into cmd/vornikctl. Keeping a
// parallel struct here is the right trade.
type fromTextRequest struct {
	Text             string `json:"text"`
	OperatorID       string `json:"operator_id"`
	Channel          string `json:"channel"`
	ChannelRef       string `json:"channel_ref"`
	ProjectID        string `json:"project_id,omitempty"`
	OperatorTimezone string `json:"operator_timezone,omitempty"`
	DryRun           bool   `json:"dry_run,omitempty"`
}

type fromTextIntent struct {
	Kind            string  `json:"kind"`
	FireAt          string  `json:"fire_at"`
	CronExpr        string  `json:"cron_expr,omitempty"`
	RecurrenceUntil string  `json:"recurrence_until,omitempty"`
	Content         string  `json:"content"`
	Confidence      float64 `json:"confidence"`
	Reasoning       string  `json:"reasoning,omitempty"`
}

type fromTextResponse struct {
	Intent   fromTextIntent `json:"intent"`
	Reminder *reminderEntry `json:"reminder,omitempty"`
}

func runRemindersSchedule(_ *cobra.Command, args []string) error {
	if remindersScheduleOperator == "" || remindersScheduleChannel == "" || remindersScheduleChannelRef == "" {
		return fmt.Errorf("--operator, --channel, and --channel-ref are required")
	}

	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "" {
		return fmt.Errorf("natural-language text is required")
	}

	req := fromTextRequest{
		Text:             text,
		OperatorID:       remindersScheduleOperator,
		Channel:          remindersScheduleChannel,
		ChannelRef:       remindersScheduleChannelRef,
		ProjectID:        remindersScheduleProject,
		OperatorTimezone: remindersScheduleTimezone,
		DryRun:           !remindersScheduleYes,
	}
	dryResp, err := postSchedule(req)
	if err != nil {
		return err
	}

	if !remindersScheduleYes {
		// Show the parsed intent + prompt for confirmation. The
		// human-readable form lands on stdout so the operator
		// can spot misinterpretation before commit.
		fmt.Println("Parsed reminder:")
		kind := dryResp.Intent.Kind
		if kind == "" {
			kind = "one_shot"
		}
		fmt.Printf("  Kind:       %s\n", kind)
		fmt.Printf("  Fire at:    %s\n", dryResp.Intent.FireAt)
		if dryResp.Intent.CronExpr != "" {
			fmt.Printf("  Cron:       %s\n", dryResp.Intent.CronExpr)
		}
		if dryResp.Intent.RecurrenceUntil != "" {
			fmt.Printf("  Until:      %s\n", dryResp.Intent.RecurrenceUntil)
		}
		fmt.Printf("  Content:    %s\n", dryResp.Intent.Content)
		fmt.Printf("  Confidence: %.2f\n", dryResp.Intent.Confidence)
		if dryResp.Intent.Reasoning != "" {
			fmt.Printf("  Reasoning:  %s\n", dryResp.Intent.Reasoning)
		}
		fmt.Print("Commit? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
		req.DryRun = false
		dryResp, err = postSchedule(req)
		if err != nil {
			return err
		}
	}

	if remindersScheduleJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(dryResp)
	}
	if dryResp.Reminder == nil {
		return fmt.Errorf("commit completed but daemon returned no reminder row")
	}
	if dryResp.Reminder.CronExpr != "" {
		until := dryResp.Reminder.RecurrenceUntil
		if until == "" {
			until = "unbounded"
		}
		fmt.Printf("Created reminder %s — recurring %q (next fire %s, until %s).\n",
			dryResp.Reminder.ID, dryResp.Reminder.CronExpr, dryResp.Reminder.FireAt, until)
		return nil
	}
	fmt.Printf("Created reminder %s — fires at %s.\n", dryResp.Reminder.ID, dryResp.Reminder.FireAt)
	return nil
}

func postSchedule(req fromTextRequest) (*fromTextResponse, error) {
	client := ClientFromEnv()
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("reminders schedule: marshal failed: %w", err)
	}
	resp, err := client.Post("/api/v1/reminders/from-text", body)
	if err != nil {
		return nil, fmt.Errorf("reminders schedule: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, ParseAPIError(resp)
	}
	var out fromTextResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("reminders schedule: decode failed: %w", err)
	}
	return &out, nil
}
