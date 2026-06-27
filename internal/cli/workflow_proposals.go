package cli

// `vornikctl workflow-proposals` — list / show / approve / reject
// architect-emitted workflow proposals (memetic-workflows arc,
// Slice 3b). Mirrors the admin endpoints at
// /api/v1/admin/workflow-proposals so terminal-only operators
// can review without firing up the UI.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// wfProposalJSON mirrors the api package's workflowProposalJSON
// shape. Kept here so the CLI is self-contained — missing fields
// are JSON-null-tolerated.
type wfProposalJSON struct {
	ID             string   `json:"id"`
	WorkflowID     string   `json:"workflow_id"`
	Status         string   `json:"status"`
	ProposalYAML   string   `json:"proposal_yaml"`
	Motivation     string   `json:"motivation"`
	EvidenceRunIDs []string `json:"evidence_run_ids"`
	Confidence     float32  `json:"confidence"`
	ArchitectModel string   `json:"architect_model"`
	CreatedAt      string   `json:"created_at"`
	DecidedAt      string   `json:"decided_at,omitempty"`
	DecidedBy      string   `json:"decided_by,omitempty"`
	AppliedAt      string   `json:"applied_at,omitempty"`
	AppliedCommit  string   `json:"applied_commit,omitempty"`
	RollbackCommit string   `json:"rollback_commit,omitempty"`
	Notes          string   `json:"notes,omitempty"`
}

type wfProposalListJSON struct {
	Proposals []wfProposalJSON `json:"proposals"`
}

var (
	wfpListStatus   string
	wfpListWorkflow string
	wfpListLimit    int
	wfpListJSON     bool
	wfpShowJSON     bool
	wfpShowYAML     bool
	wfpDecideNotes  string
)

var workflowProposalsCmd = &cobra.Command{
	Use:   "workflow-proposals",
	Short: "Review architect-emitted workflow proposals (admin)",
	Long: `Review proposals from the workflow architect (memetic-workflows
arc, Slice 3). Subcommands: list, show, approve, reject.

Requires an admin-scoped API key.`,
}

var workflowProposalsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workflow proposals",
	Long: `List proposals. By default returns pending only — operators
typically care about what needs deciding. Use --status all (or a
csv like "approved,rejected") to broaden.

Examples:
  vornikctl workflow-proposals list
  vornikctl workflow-proposals list --status all
  vornikctl workflow-proposals list --status approved,applied --workflow research
  vornikctl workflow-proposals list --json`,
	RunE: runWorkflowProposalsList,
}

var workflowProposalsShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one proposal in full",
	Long: `Show one proposal including the full proposed YAML and the
operator's notes. By default prints a human-readable header + the
YAML body; --yaml emits just the proposed YAML so you can pipe it
into a diff tool against the current WORKFLOW.md.

Examples:
  vornikctl workflow-proposals show wpr_20260525_abc123
  vornikctl workflow-proposals show wpr_20260525_abc123 --yaml | diff - configs/workflows/research.md
  vornikctl workflow-proposals show wpr_20260525_abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkflowProposalsShow,
}

var workflowProposalsApproveCmd = &cobra.Command{
	Use:   "approve <id>",
	Short: "Approve a pending proposal",
	Long: `Mark a pending proposal as approved. Approval records the
decision but does NOT write the YAML — run 'workflow-proposals
apply <id>' (or click Apply on the drill-down page) to actually
write the new file, git-commit it, and broadcast the config-reload.
Splitting decide from apply lets multiple operators review without
any one of them accidentally touching disk.`,
	Args: cobra.ExactArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		return decideProposal(args[0], "approved", wfpDecideNotes)
	},
}

var workflowProposalsRejectCmd = &cobra.Command{
	Use:   "reject <id>",
	Short: "Reject a pending proposal",
	Long: `Mark a pending proposal as rejected. The architect can re-propose
later if the underlying evidence changes — rejecting one proposal
doesn't permanently silence the architect for that workflow.`,
	Args: cobra.ExactArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		return decideProposal(args[0], "rejected", wfpDecideNotes)
	},
}

var workflowProposalsApplyCmd = &cobra.Command{
	Use:   "apply <id>",
	Short: "Apply an approved proposal (writes file + git commit + reload)",
	Long: `Apply an approved proposal: write the new WORKFLOW.md to disk,
git-commit the change in the source tree, fire the cross-instance
config-reload broadcast, and stamp the proposal row as applied.

The proposal MUST already be in status=approved. Apply on a
pending row fails with 409 — approve first via 'approve <id>'.

Operators can roll back the change via 'rollback <id>' (Slice 5)
which uses git revert to restore the previous WORKFLOW.md.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return postProposalAction(args[0], "apply")
	},
}

var workflowProposalsRollbackCmd = &cobra.Command{
	Use:   "rollback <id>",
	Short: "Roll back an applied proposal (git revert + reload)",
	Long: `Roll back a previously-applied proposal. Runs 'git revert' against
the proposal's applied_commit in the source tree and restores the
previous WORKFLOW.md, then fires the config-reload broadcast.

The proposal MUST be in status=applied.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return postProposalAction(args[0], "rollback")
	},
}

func init() {
	workflowProposalsListCmd.Flags().StringVar(&wfpListStatus, "status", "pending",
		`Status filter: 'pending' (default), 'all', or csv ('approved,rejected')`)
	workflowProposalsListCmd.Flags().StringVar(&wfpListWorkflow, "workflow", "",
		"Restrict to one workflow_id")
	workflowProposalsListCmd.Flags().IntVar(&wfpListLimit, "limit", 50,
		"Max rows to return (1-500)")
	workflowProposalsListCmd.Flags().BoolVar(&wfpListJSON, "json", false,
		"Emit JSON instead of a human-readable table")

	workflowProposalsShowCmd.Flags().BoolVar(&wfpShowJSON, "json", false,
		"Emit full JSON instead of formatted output")
	workflowProposalsShowCmd.Flags().BoolVar(&wfpShowYAML, "yaml", false,
		"Emit only the proposed YAML (for piping into diff tools)")

	workflowProposalsApproveCmd.Flags().StringVar(&wfpDecideNotes, "notes", "",
		"Optional rationale for the decision (recorded in the audit trail)")
	workflowProposalsRejectCmd.Flags().StringVar(&wfpDecideNotes, "notes", "",
		"Optional rationale for the decision (recorded in the audit trail)")

	workflowProposalsCmd.AddCommand(workflowProposalsListCmd)
	workflowProposalsCmd.AddCommand(workflowProposalsShowCmd)
	workflowProposalsCmd.AddCommand(workflowProposalsApproveCmd)
	workflowProposalsCmd.AddCommand(workflowProposalsRejectCmd)
	workflowProposalsCmd.AddCommand(workflowProposalsApplyCmd)
	workflowProposalsCmd.AddCommand(workflowProposalsRollbackCmd)
	rootCmd.AddCommand(workflowProposalsCmd)
}

func runWorkflowProposalsList(_ *cobra.Command, _ []string) error {
	q := url.Values{}
	switch strings.ToLower(strings.TrimSpace(wfpListStatus)) {
	case "all":
		// Omit the status filter entirely — server returns
		// every status.
	case "":
		q.Set("status", "pending")
	default:
		q.Set("status", wfpListStatus)
	}
	if wfpListWorkflow != "" {
		q.Set("workflow", wfpListWorkflow)
	}
	if wfpListLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", wfpListLimit))
	}
	path := "/api/v1/admin/workflow-proposals"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("workflow-proposals list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}

	var list wfProposalListJSON
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fmt.Errorf("workflow-proposals list: decode: %w", err)
	}

	if wfpListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	return renderProposalsList(os.Stdout, list.Proposals)
}

// renderProposalsList prints the list as a tab-separated table. ID
// columns are truncated to 24 chars (enough to disambiguate without
// wrapping every row).
func renderProposalsList(out *os.File, proposals []wfProposalJSON) error {
	if len(proposals) == 0 {
		_, _ = fmt.Fprintln(out, "No proposals.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tWORKFLOW\tSTATUS\tCONF\tEVIDENCE\tCREATED\tDECIDED_BY")
	for _, p := range proposals {
		decidedBy := p.DecidedBy
		if decidedBy == "" {
			decidedBy = "—"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\t%d\t%s\t%s\n",
			truncate(p.ID, 24),
			truncate(p.WorkflowID, 20),
			p.Status,
			p.Confidence,
			len(p.EvidenceRunIDs),
			truncate(p.CreatedAt, 19),
			truncate(decidedBy, 20),
		)
	}
	return tw.Flush()
}

func runWorkflowProposalsShow(_ *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/admin/workflow-proposals/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("workflow-proposals show: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}

	var p wfProposalJSON
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("workflow-proposals show: decode: %w", err)
	}
	if wfpShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	if wfpShowYAML {
		_, _ = os.Stdout.WriteString(p.ProposalYAML)
		return nil
	}
	return renderProposalShow(os.Stdout, &p)
}

func renderProposalShow(out *os.File, p *wfProposalJSON) error {
	_, _ = fmt.Fprintf(out, "Proposal: %s\n", p.ID)
	_, _ = fmt.Fprintf(out, "Workflow: %s\n", p.WorkflowID)
	_, _ = fmt.Fprintf(out, "Status:   %s\n", p.Status)
	_, _ = fmt.Fprintf(out, "Confidence: %.2f\n", p.Confidence)
	_, _ = fmt.Fprintf(out, "Architect model: %s\n", p.ArchitectModel)
	_, _ = fmt.Fprintf(out, "Created:  %s\n", truncate(p.CreatedAt, 19))
	if p.DecidedAt != "" {
		_, _ = fmt.Fprintf(out, "Decided:  %s by %s\n", truncate(p.DecidedAt, 19), p.DecidedBy)
	}
	if p.AppliedAt != "" {
		_, _ = fmt.Fprintf(out, "Applied:  %s (commit %s)\n", truncate(p.AppliedAt, 19), p.AppliedCommit)
	}
	if p.RollbackCommit != "" {
		_, _ = fmt.Fprintf(out, "Rollback: commit %s\n", p.RollbackCommit)
	}
	if p.Notes != "" {
		_, _ = fmt.Fprintf(out, "Notes:    %s\n", p.Notes)
	}
	if len(p.EvidenceRunIDs) > 0 {
		sortedEv := append([]string(nil), p.EvidenceRunIDs...)
		sort.Strings(sortedEv)
		_, _ = fmt.Fprintf(out, "Evidence (%d runs):\n", len(sortedEv))
		for _, id := range sortedEv {
			_, _ = fmt.Fprintf(out, "  • %s\n", id)
		}
	}
	_, _ = fmt.Fprintln(out, "\nMotivation:")
	_, _ = fmt.Fprintln(out, indent(p.Motivation, "  "))
	_, _ = fmt.Fprintln(out, "\n=== Proposed WORKFLOW.md ===")
	_, _ = fmt.Fprint(out, p.ProposalYAML)
	if !strings.HasSuffix(p.ProposalYAML, "\n") {
		_, _ = fmt.Fprintln(out)
	}
	return nil
}

func decideProposal(id, status, notes string) error {
	body := map[string]string{"status": status}
	if notes != "" {
		body["notes"] = notes
	}
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return err
	}
	client := ClientFromEnv()
	resp, err := client.Post(
		"/api/v1/admin/workflow-proposals/"+url.PathEscape(id)+"/decide",
		body)
	if err != nil {
		return fmt.Errorf("workflow-proposals %s: %w", status, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var p wfProposalJSON
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		// Server returned 200 but body is the minimal
		// {id, status} fallback (read-after-write failed
		// server-side). That's still success — operator just
		// won't see decided_at locally.
		_, _ = fmt.Fprintf(os.Stdout, "Proposal %s: %s\n", id, status)
		return nil
	}
	_, _ = fmt.Fprintf(os.Stdout, "Proposal %s: %s by %s\n", p.ID, p.Status, p.DecidedBy)
	return nil
}

// postProposalAction POSTs against /api/v1/admin/workflow-proposals/
// {id}/{action}, decodes the response, and prints a one-line
// confirmation. Shared by `apply` and `rollback`.
func postProposalAction(id, action string) error {
	client := ClientFromEnv()
	resp, err := client.Post(
		"/api/v1/admin/workflow-proposals/"+url.PathEscape(id)+"/"+action,
		map[string]string{})
	if err != nil {
		return fmt.Errorf("workflow-proposals %s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var p wfProposalJSON
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Proposal %s: %s\n", id, action)
		return nil
	}
	switch action {
	case "apply":
		_, _ = fmt.Fprintf(os.Stdout, "Proposal %s: applied (commit %s)\n",
			p.ID, truncate(p.AppliedCommit, 12))
	case "rollback":
		_, _ = fmt.Fprintf(os.Stdout, "Proposal %s: rolled_back (commit %s)\n",
			p.ID, truncate(p.RollbackCommit, 12))
	default:
		_, _ = fmt.Fprintf(os.Stdout, "Proposal %s: %s\n", p.ID, p.Status)
	}
	return nil
}

// indent prepends `prefix` to every line of `s`. Used for
// motivation prose so a multi-line block is visually offset from
// the field labels above it.
func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
