package cli

// vornikctl blackbox trigger / override — Phase B operator surface
// CLI parity for the REST endpoints under
// /api/v1/admin/workflow-healing/. Wraps the JSON wire shapes the
// api package emits.
//
//   vornikctl blackbox trigger list [--project P] [--workflow W] [--status S] [--class C] [--limit N] [--json]
//   vornikctl blackbox trigger dismiss <id>
//   vornikctl blackbox trigger generate-candidate <id> [--json]
//   vornikctl blackbox trigger bulk-dismiss <id1> [<id2> ...] [--json]
//
//   vornikctl blackbox override list [--json]
//   vornikctl blackbox override set --project P --workflow W --class C
//                                  [--threshold-pct N] [--mute-hours H]
//                                  [--clear-mute] [--notes "..."] [--json]
//   vornikctl blackbox override delete --project P --workflow W --class C

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	// trigger subcommands
	blackboxTriggerCmd = &cobra.Command{
		Use:   "trigger",
		Short: "Workflow-healing trigger lifecycle (list / dismiss / generate-candidate / bulk-dismiss)",
		Long: `Operator commands for the Black Box Phase B trigger ledger.

The hourly detector writes a trigger row when a workflow's
failure-rate or avg-cost regresses against its 7-day baseline.
These commands cover the operator-triage lifecycle: inspect,
dismiss (expected regression), or hand the evidence off to the
memetic architect (generate-candidate).`,
	}

	blackboxTriggerListCmd = &cobra.Command{
		Use:   "list",
		Short: "List workflow-healing triggers",
		RunE:  runBlackBoxTriggerList,
	}
	blackboxTriggerDismissCmd = &cobra.Command{
		Use:   "dismiss <id>",
		Short: "Dismiss an open trigger (expected regression)",
		Args:  cobra.ExactArgs(1),
		RunE:  runBlackBoxTriggerDismiss,
	}
	blackboxTriggerGenerateCmd = &cobra.Command{
		Use:   "generate-candidate <id>",
		Short: "Hand a trigger's evidence to the memetic architect",
		Long: `Calls the workflow architect synchronously, stamps the resulting
proposal_id on the trigger, and prints the proposal ID. Use
'vornikctl workflow-proposals show <id>' to review the proposal.`,
		Args: cobra.ExactArgs(1),
		RunE: runBlackBoxTriggerGenerateCandidate,
	}
	blackboxTriggerBulkDismissCmd = &cobra.Command{
		Use:   "bulk-dismiss <id1> [<id2> ...]",
		Short: "Dismiss multiple triggers in one call",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runBlackBoxTriggerBulkDismiss,
	}

	// override subcommands
	blackboxOverrideCmd = &cobra.Command{
		Use:   "override",
		Short: "Per-(project, workflow, class) detector overrides (threshold + mute)",
		Long: `Operator overrides for the Phase B detector. Each row tunes the
detector for a specific (project, workflow, trigger_class) tuple:

  - threshold-pct: replaces the default relative-delta cap (25%
    for failure_rate_spike, 40% for cost_regression).
  - mute-hours: silences the detector for that tuple until now +
    N hours. Stale mutes (past timestamps) do NOT silence the
    detector — they're treated as cleared.`,
	}

	blackboxOverrideListCmd = &cobra.Command{
		Use:   "list",
		Short: "List active overrides",
		RunE:  runBlackBoxOverrideList,
	}
	blackboxOverrideSetCmd = &cobra.Command{
		Use:   "set",
		Short: "Create or update an override",
		Long: `At least one of --threshold-pct / --mute-hours / --clear-mute
must be set. --threshold-pct is entered as a percent (e.g. 50 →
relative delta 0.50). --mute-hours is resolved against the daemon's
wall clock (now + N hours).`,
		RunE: runBlackBoxOverrideSet,
	}
	blackboxOverrideDeleteCmd = &cobra.Command{
		Use:   "delete",
		Short: "Remove an override",
		RunE:  runBlackBoxOverrideDelete,
	}

	// trigger flags
	triggerListProject  string
	triggerListWorkflow string
	triggerListStatus   string
	triggerListClass    string
	triggerListLimit    int
	triggerListJSON     bool
	triggerGenerateJSON bool
	triggerBulkJSON     bool

	// override flags
	overrideListJSON     bool
	overrideSetJSON      bool
	overrideSetProject   string
	overrideSetWorkflow  string
	overrideSetClass     string
	overrideSetThreshold float64
	overrideSetMuteHours int
	overrideSetClearMute bool
	overrideSetNotes     string
	overrideDelProject   string
	overrideDelWorkflow  string
	overrideDelClass     string
)

func init() {
	blackboxTriggerListCmd.Flags().StringVar(&triggerListProject, "project", "", "Filter by project_id")
	blackboxTriggerListCmd.Flags().StringVar(&triggerListWorkflow, "workflow", "", "Filter by workflow_id")
	blackboxTriggerListCmd.Flags().StringVar(&triggerListStatus, "status", "", "Filter by status (open|dismissed|generated_candidate)")
	blackboxTriggerListCmd.Flags().StringVar(&triggerListClass, "class", "", "Filter by trigger_class (failure_rate_spike|cost_regression)")
	blackboxTriggerListCmd.Flags().IntVar(&triggerListLimit, "limit", 50, "Max rows to return (max 500)")
	blackboxTriggerListCmd.Flags().BoolVar(&triggerListJSON, "json", false, "Emit JSON instead of a table")

	blackboxTriggerGenerateCmd.Flags().BoolVar(&triggerGenerateJSON, "json", false, "Emit JSON instead of human-readable summary")
	blackboxTriggerBulkDismissCmd.Flags().BoolVar(&triggerBulkJSON, "json", false, "Emit JSON instead of human-readable summary")

	blackboxTriggerCmd.AddCommand(blackboxTriggerListCmd)
	blackboxTriggerCmd.AddCommand(blackboxTriggerDismissCmd)
	blackboxTriggerCmd.AddCommand(blackboxTriggerGenerateCmd)
	blackboxTriggerCmd.AddCommand(blackboxTriggerBulkDismissCmd)
	blackboxCmd.AddCommand(blackboxTriggerCmd)

	blackboxOverrideListCmd.Flags().BoolVar(&overrideListJSON, "json", false, "Emit JSON instead of a table")

	blackboxOverrideSetCmd.Flags().StringVar(&overrideSetProject, "project", "", "Project ID (required)")
	blackboxOverrideSetCmd.Flags().StringVar(&overrideSetWorkflow, "workflow", "", "Workflow ID (required)")
	blackboxOverrideSetCmd.Flags().StringVar(&overrideSetClass, "class", "", "Trigger class (failure_rate_spike|cost_regression) (required)")
	blackboxOverrideSetCmd.Flags().Float64Var(&overrideSetThreshold, "threshold-pct", 0, "Override threshold as percent (e.g. 50 means +50% lift)")
	blackboxOverrideSetCmd.Flags().IntVar(&overrideSetMuteHours, "mute-hours", 0, "Mute the (workflow, class) for this many hours from now")
	blackboxOverrideSetCmd.Flags().BoolVar(&overrideSetClearMute, "clear-mute", false, "Wipe any existing mute on save (mutually exclusive with --mute-hours)")
	blackboxOverrideSetCmd.Flags().StringVar(&overrideSetNotes, "notes", "", "Operator-facing notes recorded with the override")
	blackboxOverrideSetCmd.Flags().BoolVar(&overrideSetJSON, "json", false, "Emit JSON instead of human-readable summary")

	blackboxOverrideDeleteCmd.Flags().StringVar(&overrideDelProject, "project", "", "Project ID (required)")
	blackboxOverrideDeleteCmd.Flags().StringVar(&overrideDelWorkflow, "workflow", "", "Workflow ID (required)")
	blackboxOverrideDeleteCmd.Flags().StringVar(&overrideDelClass, "class", "", "Trigger class (required)")

	blackboxOverrideCmd.AddCommand(blackboxOverrideListCmd)
	blackboxOverrideCmd.AddCommand(blackboxOverrideSetCmd)
	blackboxOverrideCmd.AddCommand(blackboxOverrideDeleteCmd)
	blackboxCmd.AddCommand(blackboxOverrideCmd)
}

// healingTriggerRow mirrors api.HealingTriggerJSON for CLI decode.
// Same exclude-on-empty pattern (omitempty) so the table renderer
// can show "—" when the daemon didn't populate a field.
type healingTriggerRow struct {
	ID              string  `json:"id"`
	ProjectID       string  `json:"project_id"`
	WorkflowID      string  `json:"workflow_id"`
	TriggerClass    string  `json:"trigger_class"`
	Status          string  `json:"status"`
	BaselineValue   float64 `json:"baseline_value"`
	ComparisonValue float64 `json:"comparison_value"`
	ThresholdValue  float64 `json:"threshold_value"`
	CreatedAt       string  `json:"created_at"`
	ProposalID      string  `json:"proposal_id,omitempty"`
}

type healingTriggerListResp struct {
	Entries []healingTriggerRow `json:"entries"`
}

type healingTriggerBulkDismissResp struct {
	Dismissed int                  `json:"dismissed"`
	Failures  []healingBulkFailure `json:"failures,omitempty"`
}

type healingBulkFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

func runBlackBoxTriggerList(_ *cobra.Command, _ []string) error {
	q := url.Values{}
	if triggerListProject != "" {
		q.Set("project", triggerListProject)
	}
	if triggerListWorkflow != "" {
		q.Set("workflow", triggerListWorkflow)
	}
	if triggerListStatus != "" {
		q.Set("status", triggerListStatus)
	}
	if triggerListClass != "" {
		q.Set("class", triggerListClass)
	}
	if triggerListLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", triggerListLimit))
	}
	path := "/api/v1/admin/workflow-healing/triggers"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := ClientFromEnv().Get(path)
	if err != nil {
		return fmt.Errorf("trigger list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if triggerListJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var out healingTriggerListResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("trigger list: decode: %w", err)
	}
	if len(out.Entries) == 0 {
		fmt.Println("No triggers match the filters.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tPROJECT/WORKFLOW\tCLASS\tBASELINE→COMPARISON\tOPENED")
	for _, t := range out.Entries {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s/%s\t%s\t%.4f→%.4f\t%s\n",
			t.ID, t.Status, t.ProjectID, t.WorkflowID, t.TriggerClass,
			t.BaselineValue, t.ComparisonValue, t.CreatedAt)
	}
	return tw.Flush()
}

func runBlackBoxTriggerDismiss(_ *cobra.Command, args []string) error {
	id := args[0]
	resp, err := ClientFromEnv().Post(
		"/api/v1/admin/workflow-healing/triggers/"+url.PathEscape(id)+"/dismiss", nil)
	if err != nil {
		return fmt.Errorf("trigger dismiss: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	fmt.Printf("Trigger %s dismissed.\n", id)
	return nil
}

func runBlackBoxTriggerGenerateCandidate(_ *cobra.Command, args []string) error {
	id := args[0]
	resp, err := ClientFromEnv().Post(
		"/api/v1/admin/workflow-healing/triggers/"+url.PathEscape(id)+"/generate-candidate", nil)
	if err != nil {
		return fmt.Errorf("generate-candidate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if triggerGenerateJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var out healingTriggerRow
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("generate-candidate: decode: %w", err)
	}
	fmt.Printf("Candidate generated.\n\n")
	fmt.Printf("  Trigger:        %s\n", out.ID)
	fmt.Printf("  Workflow:       %s\n", out.WorkflowID)
	fmt.Printf("  New status:     %s\n", out.Status)
	fmt.Printf("  Proposal ID:    %s\n", out.ProposalID)
	fmt.Println()
	fmt.Printf("Review: vornikctl workflow-proposals show %s\n", out.ProposalID)
	return nil
}

func runBlackBoxTriggerBulkDismiss(_ *cobra.Command, args []string) error {
	body := map[string][]string{"ids": args}
	resp, err := ClientFromEnv().Post(
		"/api/v1/admin/workflow-healing/triggers/bulk-dismiss", body)
	if err != nil {
		return fmt.Errorf("bulk-dismiss: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if triggerBulkJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var out healingTriggerBulkDismissResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("bulk-dismiss: decode: %w", err)
	}
	fmt.Printf("Dismissed %d of %d triggers.\n", out.Dismissed, len(args))
	for _, f := range out.Failures {
		fmt.Printf("  FAIL %s: %s\n", f.ID, f.Error)
	}
	if len(out.Failures) > 0 {
		// Non-zero exit so scripted callers detect partial failure.
		return fmt.Errorf("%d trigger(s) failed to dismiss", len(out.Failures))
	}
	return nil
}

// healingOverrideRow mirrors api.HealingOverrideJSON.
type healingOverrideRow struct {
	ProjectID         string   `json:"project_id"`
	WorkflowID        string   `json:"workflow_id"`
	TriggerClass      string   `json:"trigger_class"`
	ThresholdOverride *float64 `json:"threshold_override,omitempty"`
	MutedUntil        string   `json:"muted_until,omitempty"`
	Notes             string   `json:"notes,omitempty"`
	CreatedBy         string   `json:"created_by,omitempty"`
	UpdatedAt         string   `json:"updated_at,omitempty"`
}

type healingOverrideListResp struct {
	Entries []healingOverrideRow `json:"entries"`
}

func runBlackBoxOverrideList(_ *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/admin/workflow-healing/overrides")
	if err != nil {
		return fmt.Errorf("override list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if overrideListJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var out healingOverrideListResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("override list: decode: %w", err)
	}
	if len(out.Entries) == 0 {
		fmt.Println("No overrides configured.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT/WORKFLOW\tCLASS\tTHRESHOLD\tMUTED UNTIL\tNOTES\tUPDATED")
	for _, o := range out.Entries {
		thresh := "—"
		if o.ThresholdOverride != nil {
			thresh = fmt.Sprintf("+%.1f%%", 100.0*(*o.ThresholdOverride))
		}
		mute := o.MutedUntil
		if mute == "" {
			mute = "—"
		}
		notes := o.Notes
		if len(notes) > 40 {
			notes = notes[:37] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\t%s\t%s\n",
			o.ProjectID, o.WorkflowID, o.TriggerClass, thresh, mute, notes, o.UpdatedAt)
	}
	return tw.Flush()
}

func runBlackBoxOverrideSet(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(overrideSetProject) == "" ||
		strings.TrimSpace(overrideSetWorkflow) == "" ||
		strings.TrimSpace(overrideSetClass) == "" {
		return fmt.Errorf("--project, --workflow, --class are all required")
	}
	body := map[string]interface{}{
		"project_id":    overrideSetProject,
		"workflow_id":   overrideSetWorkflow,
		"trigger_class": overrideSetClass,
	}
	// Cobra's Float64Var defaults zero whether the flag was set or
	// not — disambiguate by inspecting Changed.
	if cmd.Flags().Changed("threshold-pct") {
		if overrideSetThreshold <= 0 {
			return fmt.Errorf("--threshold-pct must be > 0")
		}
		body["threshold_override"] = overrideSetThreshold / 100.0
	}
	if overrideSetClearMute {
		body["clear_mute"] = true
	} else if cmd.Flags().Changed("mute-hours") {
		if overrideSetMuteHours <= 0 {
			return fmt.Errorf("--mute-hours must be > 0")
		}
		body["mute_duration"] = fmt.Sprintf("%dh", overrideSetMuteHours)
	}
	if overrideSetNotes != "" {
		body["notes"] = overrideSetNotes
	}
	// Mirror the API's "nothing to save" guard so the CLI fails
	// fast without a server round-trip.
	if !cmd.Flags().Changed("threshold-pct") &&
		!cmd.Flags().Changed("mute-hours") &&
		!overrideSetClearMute {
		return fmt.Errorf("nothing to save: set --threshold-pct, --mute-hours, or --clear-mute")
	}
	resp, err := ClientFromEnv().Post("/api/v1/admin/workflow-healing/overrides", body)
	if err != nil {
		return fmt.Errorf("override set: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if overrideSetJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var saved healingOverrideRow
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		return fmt.Errorf("override set: decode: %w", err)
	}
	fmt.Printf("Override saved for %s/%s/%s.\n",
		saved.ProjectID, saved.WorkflowID, saved.TriggerClass)
	if saved.ThresholdOverride != nil {
		fmt.Printf("  Threshold: +%.1f%%\n", 100.0*(*saved.ThresholdOverride))
	}
	if saved.MutedUntil != "" {
		fmt.Printf("  Muted until: %s\n", saved.MutedUntil)
	}
	return nil
}

func runBlackBoxOverrideDelete(_ *cobra.Command, _ []string) error {
	if strings.TrimSpace(overrideDelProject) == "" ||
		strings.TrimSpace(overrideDelWorkflow) == "" ||
		strings.TrimSpace(overrideDelClass) == "" {
		return fmt.Errorf("--project, --workflow, --class are all required")
	}
	body := map[string]string{
		"project_id":    overrideDelProject,
		"workflow_id":   overrideDelWorkflow,
		"trigger_class": overrideDelClass,
	}
	resp, err := ClientFromEnv().Post("/api/v1/admin/workflow-healing/overrides/delete", body)
	if err != nil {
		return fmt.Errorf("override delete: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	fmt.Printf("Override deleted: %s/%s/%s\n",
		overrideDelProject, overrideDelWorkflow, overrideDelClass)
	return nil
}
