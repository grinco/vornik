package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// `vornikctl control-plane` (alias `cp`) — the control-plane proposal ledger
// (LLD 2026-07-07-control-plane-design, Phase 1). Human-gated change
// proposals: propose, list, show, approve, reject. There is NO apply command
// in Phase 1 — an approved proposal is actioned by hand; gated apply/rollback
// is Phase 2. All commands hit the operator-scoped
// /api/v1/operator/proposals REST surface (requireOperatorScope; CE-safe).
//
// (The `operator` command is a different feature — operator IDENTITY
// management — so the control plane gets its own namespace.)
var controlPlaneCmd = &cobra.Command{
	Use:     "control-plane",
	Aliases: []string{"cp"},
	Short:   "Control-plane proposal ledger (troubleshooting / config proposals)",
	Long: `Review and decide control-plane change proposals.

A proposal is a human-gated suggested change (a config diff, a model swap)
raised by the operator or a Tune detector. Phase 1 lets you propose, list,
show, approve, and reject; applying an approved change is done by hand (gated
auto-apply is a later phase).`,
}

var cpProposalsCmd = &cobra.Command{
	Use:   "proposals",
	Short: "List control-plane proposals",
	RunE:  runCPProposalsList,
}

var cpShowCmd = &cobra.Command{
	Use:   "show <proposal-id>",
	Short: "Show a single proposal (full diff + rationale)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runCPShow(args[0]) },
}

var cpApproveCmd = &cobra.Command{
	Use:   "approve <proposal-id>",
	Short: "Approve a DRAFT proposal (you must not be its proposer)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runCPDecide(args[0], "approve") },
}

var cpRejectCmd = &cobra.Command{
	Use:   "reject <proposal-id>",
	Short: "Reject a DRAFT proposal",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runCPDecide(args[0], "reject") },
}

var cpProposeCmd = &cobra.Command{
	Use:   "propose",
	Short: "Raise a control-plane proposal (writes a DRAFT for review)",
	RunE:  runCPPropose,
}

var cpApplyCmd = &cobra.Command{
	Use:   "apply <proposal-id>",
	Short: "Apply an APPROVED proposal (hot-reload; auto-rolls-back on failure)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runCPApply(args[0]) },
}

var cpRollbackCmd = &cobra.Command{
	Use:   "rollback <proposal-id>",
	Short: "Roll an APPLIED proposal back to its pre-apply snapshot",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runCPRollback(args[0]) },
}

var cpDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <focus>",
	Short: "Diagnose a project/task from its logs & metrics (read-only unless --propose)",
	Long: `Run a single-shot diagnosis: the daemon assembles an evidence bundle
(recent failed/successful executions, metrics, logs, known failure patterns)
and asks the configured LLM for a root cause. Read-only by default; with
--propose it may file a review-only DRAFT proposal (never auto-applied). Any
suggested change carrying a secret or an external URL is rejected before a
proposal is filed.

<focus> is a task id (task_...) or a project id / substring.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error { return runCPDiagnose(args[0]) },
}

var (
	cpListProject string
	cpListStatus  string
	cpListJSON    bool

	cpProposeProject   string
	cpProposeKind      string
	cpProposeScope     string
	cpProposeTitle     string
	cpProposeDiff      string
	cpProposeRationale string

	cpActor    string
	cpApplyAck bool

	cpDiagnosePropose bool
	cpDiagnoseJSON    bool
)

func init() {
	cpProposalsCmd.Flags().StringVarP(&cpListProject, "project", "p", "", "Filter by project ID")
	cpProposalsCmd.Flags().StringVarP(&cpListStatus, "status", "s", "", "Filter by status (DRAFT/APPROVED/REJECTED/APPLIED/ROLLED_BACK)")
	cpProposalsCmd.Flags().BoolVar(&cpListJSON, "json", false, "Emit JSON")

	cpApproveCmd.Flags().StringVar(&cpActor, "author", cpDefaultActor(), "Approver identity (must differ from the proposer)")
	cpRejectCmd.Flags().StringVar(&cpActor, "author", cpDefaultActor(), "Rejecter identity")

	cpProposeCmd.Flags().StringVarP(&cpProposeProject, "project", "p", "", "Affected project (omit for a daemon-scope proposal)")
	cpProposeCmd.Flags().StringVar(&cpProposeKind, "kind", "config", "config | model | scaffold")
	cpProposeCmd.Flags().StringVar(&cpProposeScope, "blast-radius", "project", "model | project | swarm | daemon")
	cpProposeCmd.Flags().StringVar(&cpProposeTitle, "title", "", "One-line title (required)")
	cpProposeCmd.Flags().StringVar(&cpProposeDiff, "diff", "", "The proposed change (diff / patch text)")
	cpProposeCmd.Flags().StringVar(&cpProposeRationale, "rationale", "", "Why")
	_ = cpProposeCmd.MarkFlagRequired("title")

	cpApplyCmd.Flags().StringVar(&cpActor, "author", cpDefaultActor(), "Operator identity applying the change")
	cpApplyCmd.Flags().BoolVar(&cpApplyAck, "ack-daemon", false, "Acknowledge a daemon-scope change affects every project")

	cpDiagnoseCmd.Flags().BoolVar(&cpDiagnosePropose, "propose", false, "File a review-only DRAFT proposal from the diagnosis (never auto-applied)")
	cpDiagnoseCmd.Flags().BoolVar(&cpDiagnoseJSON, "json", false, "Emit the raw verdict JSON")

	controlPlaneCmd.AddCommand(cpProposalsCmd, cpShowCmd, cpApproveCmd, cpRejectCmd, cpProposeCmd, cpApplyCmd, cpRollbackCmd, cpDiagnoseCmd)
	rootCmd.AddCommand(controlPlaneCmd)
}

func cpDefaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "operator"
}

type cpProposalWire struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	Kind        string `json:"kind"`
	BlastRadius string `json:"blastRadius"`
	Title       string `json:"title"`
	Diff        string `json:"diff"`
	Rationale   string `json:"rationale"`
	Status      string `json:"status"`
	ProposedBy  string `json:"proposedBy"`
	Approver    string `json:"approver"`
	CreatedAt   string `json:"createdAt"`
}

func runCPProposalsList(_ *cobra.Command, _ []string) error {
	q := "/api/v1/operator/proposals"
	sep := "?"
	if cpListProject != "" {
		q += sep + "project=" + cpListProject
		sep = "&"
	}
	if cpListStatus != "" {
		q += sep + "status=" + cpListStatus
	}
	client := ClientFromEnv()
	resp, err := client.Get(q)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Proposals []cpProposalWire `json:"proposals"`
		Count     int              `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if cpListJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if out.Count == 0 {
		fmt.Println("No proposals.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tKIND\tSCOPE\tPROJECT\tTITLE")
	for _, p := range out.Proposals {
		proj := p.ProjectID
		if proj == "" {
			proj = "(daemon)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Status, p.Kind, p.BlastRadius, proj, p.Title)
	}
	return tw.Flush()
}

func runCPShow(id string) error {
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/operator/proposals/" + id)
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var p cpProposalWire
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("id:           %s\nstatus:       %s\nkind:         %s\nblast_radius: %s\nproject:      %s\ntitle:        %s\nproposed_by:  %s\n",
		p.ID, p.Status, p.Kind, p.BlastRadius, p.ProjectID, p.Title, p.ProposedBy)
	if p.Approver != "" {
		fmt.Printf("approver:     %s\n", p.Approver)
	}
	if p.Rationale != "" {
		fmt.Printf("\nrationale:\n%s\n", p.Rationale)
	}
	if p.Diff != "" {
		fmt.Printf("\ndiff:\n%s\n", p.Diff)
	}
	return nil
}

func runCPDecide(id, decision string) error {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operator/proposals/"+id+"/decide",
		map[string]any{"decision": decision, "actor": cpActor})
	if err != nil {
		return fmt.Errorf("%s: %w", decision, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var p cpProposalWire
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Proposal %s is now %s (by %s).\n", p.ID, p.Status, p.Approver)
	if p.Status == "APPROVED" {
		fmt.Println("Note: Phase 1 has no auto-apply — action the approved change by hand.")
	}
	return nil
}

func runCPApply(id string) error {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operator/proposals/"+id+"/apply",
		map[string]any{"actor": cpActor, "ackDaemon": cpApplyAck})
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var p cpProposalWire
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Applied %s (%s) — status %s. Rollback with: vornikctl cp rollback %s\n", p.ID, p.Title, p.Status, p.ID)
	return nil
}

func runCPRollback(id string) error {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operator/proposals/"+id+"/rollback", map[string]any{})
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var p cpProposalWire
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Rolled back %s — status %s.\n", p.ID, p.Status)
	return nil
}

func runCPDiagnose(focus string) error {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operator/diagnose",
		map[string]any{"focus": focus, "propose": cpDiagnosePropose})
	if err != nil {
		return fmt.Errorf("diagnose: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Verdict struct {
			RootCause       string   `json:"root_cause"`
			Confidence      string   `json:"confidence"`
			Evidence        []string `json:"evidence"`
			SuggestedChange string   `json:"suggested_change"`
		} `json:"verdict"`
		ProposalID string `json:"proposalId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if cpDiagnoseJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("root cause:  %s\nconfidence:  %s\n", out.Verdict.RootCause, out.Verdict.Confidence)
	if len(out.Verdict.Evidence) > 0 {
		fmt.Println("\nevidence:")
		for _, e := range out.Verdict.Evidence {
			fmt.Printf("  - %s\n", e)
		}
	}
	if out.Verdict.SuggestedChange != "" {
		fmt.Printf("\nsuggested change:\n%s\n", out.Verdict.SuggestedChange)
	}
	switch {
	case out.ProposalID != "":
		fmt.Printf("\nFiled review-only proposal %s. Review: vornikctl cp show %s\n", out.ProposalID, out.ProposalID)
	case cpDiagnosePropose:
		fmt.Println("\nNo proposal filed (no actionable, safe suggested change).")
	}
	return nil
}

func runCPPropose(_ *cobra.Command, _ []string) error {
	body := map[string]any{
		"projectId":   cpProposeProject,
		"kind":        cpProposeKind,
		"blastRadius": cpProposeScope,
		"title":       cpProposeTitle,
		"diff":        cpProposeDiff,
		"rationale":   cpProposeRationale,
		"proposedBy":  cpDefaultActor(),
	}
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operator/proposals", body)
	if err != nil {
		return fmt.Errorf("propose: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var p cpProposalWire
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Proposed %s (%s) — status %s. Review: vornikctl cp show %s\n", p.ID, p.Title, p.Status, p.ID)
	return nil
}
