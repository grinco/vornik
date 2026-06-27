package cli

// `vornikctl memory regraph` — operator-driven backfill of isolated
// knowledge-graph entities. Flips needs_graph_extraction = TRUE on
// every chunk in --project that produced zero published edges so the
// daemon's KG worker re-runs the extraction pipeline against them
// with whatever logic is current.
//
// Use case: after a pipeline-logic fix (e.g. the 2026-05-25
// evidence-substring normalisation, commit ed1a501) the operator
// wants the existing isolated entities (48% of the live KG at audit
// time) to actually benefit, not just future ingest. Without this
// surface they wait for new chunks to arrive.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

var (
	regraphProject string
	regraphDryRun  bool
	regraphJSON    bool
)

var memoryRegraphCmd = &cobra.Command{
	Use:   "regraph",
	Short: "Re-flag chunks with zero edges so the KG worker reprocesses them",
	Long: `Re-flag every chunk in --project that produced zero published
edges, so the daemon's KG extraction worker picks them up on its next
tick and re-runs the four-stage pipeline (extractor → resolver →
relationship → validator) with whatever logic is currently shipping.

Use after a KG-pipeline fix to make existing isolated entities benefit
from the change. Without this, the fix only helps NEW chunks.

Idempotent: re-running against the same project re-flags the same
chunk set minus any that the latest pass DID manage to extract edges
from. --dry-run reports the candidate count without writing.

Examples:
  vornikctl memory regraph --project assistant --dry-run
  vornikctl memory regraph --project assistant
  vornikctl memory regraph --project assistant --json`,
	RunE: runMemoryRegraph,
}

func init() {
	memoryRegraphCmd.Flags().StringVarP(&regraphProject, "project", "p", "", "Project ID (required)")
	memoryRegraphCmd.Flags().BoolVar(&regraphDryRun, "dry-run", false, "Report candidate count without writing")
	memoryRegraphCmd.Flags().BoolVar(&regraphJSON, "json", false, "Emit JSON summary instead of human-readable output")
	_ = memoryRegraphCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryRegraphCmd)
}

// memoryRegraphResponse mirrors api.MemoryGraphReflagResult on the
// client side. Kept here so the CLI stays a self-contained tool.
type memoryRegraphResponse struct {
	Project   string `json:"project"`
	DryRun    bool   `json:"dryRun"`
	ReFlagged int    `json:"reFlagged"`
}

func runMemoryRegraph(_ *cobra.Command, _ []string) error {
	if regraphProject == "" {
		return fmt.Errorf("--project is required")
	}
	q := url.Values{}
	q.Set("project", regraphProject)
	if regraphDryRun {
		q.Set("dry_run", "true")
	}
	path := "/api/v1/memory/regraph?" + q.Encode()

	raw, err := postJSON(path, nil)
	if err != nil {
		return fmt.Errorf("memory regraph: %w", err)
	}
	var resp memoryRegraphResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("memory regraph: parse response: %w", err)
	}

	if regraphJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	verb := "re-flagged"
	if resp.DryRun {
		verb = "would re-flag"
	}
	fmt.Printf("%s %d chunks in project %q for KG re-extraction.\n", verb, resp.ReFlagged, resp.Project)
	if resp.DryRun {
		fmt.Println("(dry-run: no changes written. re-run without --dry-run to apply.)")
	} else if resp.ReFlagged > 0 {
		fmt.Println("The KG worker will pick them up on its next tick (typically <1 minute).")
		fmt.Println("Watch progress: `vornikctl memory stats --project " + resp.Project + "` or the vornik_memory_graph_chunks_pending metric.")
	}
	return nil
}
