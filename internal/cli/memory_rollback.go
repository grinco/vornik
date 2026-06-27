package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Phase 3 of memory hardening: epoch + rollback CLI surface.
// Wraps the HTTP API endpoints (see internal/api/memory_handlers.go).

var (
	rollbackProject string
	rollbackToEpoch string
	rollbackReason  string
	rollbackApply   bool

	epochsProject string
	epochsLimit   int
)

var memoryRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Roll back the corpus to a prior snapshot",
	Long: `Atomically deactivate every epoch newer than --to and re-activate every
epoch up to and including --to for the given project. Default is preview;
pass --apply to execute. Records the action in corpus_rollbacks.

Example:
  vornikctl memory rollback --project assistant --to epoch_xxx
  vornikctl memory rollback --project assistant --to epoch_xxx --apply

Use the epoch listing first to pick a target:
  vornikctl memory epochs --project assistant`,
	RunE: runMemoryRollback,
}

var memoryEpochsCmd = &cobra.Command{
	Use:   "epochs",
	Short: "List recent corpus epochs (snapshots) for a project",
	RunE:  runMemoryEpochs,
}

func init() {
	memoryRollbackCmd.Flags().StringVar(&rollbackProject, "project", "", "Project ID (required)")
	memoryRollbackCmd.Flags().StringVar(&rollbackToEpoch, "to", "", "Target epoch ID (required)")
	memoryRollbackCmd.Flags().StringVar(&rollbackReason, "reason", "", "Optional rollback reason")
	memoryRollbackCmd.Flags().BoolVar(&rollbackApply, "apply", false, "Actually perform the rollback (default: dry-run)")
	_ = memoryRollbackCmd.MarkFlagRequired("project")
	_ = memoryRollbackCmd.MarkFlagRequired("to")

	memoryEpochsCmd.Flags().StringVar(&epochsProject, "project", "", "Project ID (required)")
	memoryEpochsCmd.Flags().IntVar(&epochsLimit, "limit", 20, "Max epochs to show")
	_ = memoryEpochsCmd.MarkFlagRequired("project")

	memoryCmd.AddCommand(memoryRollbackCmd, memoryEpochsCmd)
}

type epochRow struct {
	ID                string  `json:"id"`
	IngestExecutionID *string `json:"ingestExecutionId,omitempty"`
	CreatedAt         string  `json:"createdAt"`
	ClosedAt          *string `json:"closedAt,omitempty"`
	ChunksAdmitted    int     `json:"chunksAdmitted"`
	ChunksQuarantined int     `json:"chunksQuarantined"`
	ChunksVerified    int     `json:"chunksVerified"`
	ChunksRefuted     int     `json:"chunksRefuted"`
	ChunksSuperseded  int     `json:"chunksSuperseded"`
	IsActive          bool    `json:"isActive"`
}

func runMemoryEpochs(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", epochsLimit))
	path := fmt.Sprintf("/api/v1/projects/%s/memory/epochs?%s", epochsProject, q.Encode())
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	var resp struct {
		Epochs []epochRow `json:"epochs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "EPOCH ID\tCREATED\tCLOSED\tACTIVE\tADM\tQUAR\tVERIF\tSUPERS")
	for _, e := range resp.Epochs {
		closed := "open"
		if e.ClosedAt != nil {
			closed = *e.ClosedAt
		}
		active := "no"
		if e.IsActive {
			active = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\n",
			e.ID, trimTS(e.CreatedAt), trimTS(closed), active,
			e.ChunksAdmitted, e.ChunksQuarantined, e.ChunksVerified, e.ChunksSuperseded)
	}
	if len(resp.Epochs) == 0 {
		_, _ = fmt.Fprintln(w, "(no epochs yet — pipeline hasn't published any snapshots for this project)")
	}
	return w.Flush()
}

func runMemoryRollback(cmd *cobra.Command, args []string) error {
	by := os.Getenv("USER")
	if by == "" {
		by = "vornikctl"
	}
	body := map[string]interface{}{
		"target_epoch_id": rollbackToEpoch,
		"reason":          rollbackReason,
		"triggered_by":    "operator:" + by,
		"apply":           rollbackApply,
	}
	raw, err := postJSON(fmt.Sprintf("/api/v1/projects/%s/memory/rollback", rollbackProject), body)
	if err != nil {
		return err
	}
	var resp struct {
		Project             string `json:"project"`
		Target              string `json:"target"`
		TargetCreatedAt     string `json:"targetCreatedAt"`
		WouldDeactivate     int    `json:"wouldDeactivate"`
		WouldReactivate     int    `json:"wouldReactivate"`
		Applied             bool   `json:"applied"`
		ActuallyDeactivated int    `json:"actuallyDeactivated"`
		ActuallyReactivated int    `json:"actuallyReactivated"`
		// Restore-pass numbers (migration 89, rollback × supersession).
		// Pointers so "field absent" (older daemon / preview-count
		// failure) is distinguishable from a real zero and omitted.
		WouldRestoreChunks         *int `json:"wouldRestoreChunks"`
		NonRestorableSupersessions *int `json:"nonRestorableSupersessions"`
		ChunksRestored             *int `json:"chunksRestored"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	fmt.Printf("Project: %s\nTarget:  %s (created %s)\n", resp.Project, resp.Target, trimTS(resp.TargetCreatedAt))
	fmt.Printf("Plan:    %d epoch(s) → deactivate, %d epoch(s) → reactivate\n",
		resp.WouldDeactivate, resp.WouldReactivate)
	if resp.WouldRestoreChunks != nil {
		fmt.Printf("         %d previously-superseded chunk(s) → restore\n", *resp.WouldRestoreChunks)
	}
	if resp.NonRestorableSupersessions != nil && *resp.NonRestorableSupersessions > 0 {
		fmt.Printf("Note:    %d superseded chunk(s) have no restore provenance (pre-migration history) and will NOT come back; re-ingest the source if needed.\n",
			*resp.NonRestorableSupersessions)
	}
	if resp.Applied {
		fmt.Printf("\nApplied: %d deactivated, %d (re)activated", resp.ActuallyDeactivated, resp.ActuallyReactivated)
		if resp.ChunksRestored != nil {
			fmt.Printf(", %d chunk(s) restored", *resp.ChunksRestored)
		}
		fmt.Println(".")
	} else {
		fmt.Println("\nDry-run only. Re-run with --apply to execute.")
	}
	return nil
}

// trimTS returns the date+time portion of an RFC3339 timestamp,
// dropping seconds and timezone for readability in terminal tables.
func trimTS(s string) string {
	if s == "" || s == "open" {
		return s
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Could be tabwriter formatting trick, return as-is.
		return s
	}
	return t.Format("2006-01-02 15:04")
}
