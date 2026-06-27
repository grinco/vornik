package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	backfillTitlesBatchSize int
	backfillTitlesMax       int
	backfillTitlesDryRun    bool
	backfillTitlesJSON      bool
)

var memoryBackfillTitlesCmd = &cobra.Command{
	Use:   "backfill-titles",
	Short: "Generate LLM topic labels for chunks with NULL content_title",
	Long: `Walk project_memory_chunks rows where content_title IS NULL and ask the
configured Titler LLM (memory.titler.model in vornik.yaml) to generate a
short topic label for each. The label powers the operator vector-cloud
UI: chunks without one fall back to their first markdown heading and
then to the source filename, which is usually noise.

The daemon does the work — this command just drives it batch by batch
and prints progress. Safe to interrupt and re-run; it always resumes
from wherever it left off because the query selects WHERE content_title
IS NULL.

Examples:
  vornikctl memory backfill-titles                    # title everything
  vornikctl memory backfill-titles --dry-run          # count only
  vornikctl memory backfill-titles --batch-size 5     # smaller batches
  vornikctl memory backfill-titles --max 100          # stop after 100`,
	RunE: runMemoryBackfillTitles,
}

func init() {
	memoryBackfillTitlesCmd.Flags().IntVar(&backfillTitlesBatchSize, "batch-size", 10, "Chunks per LLM-call batch (1-100)")
	memoryBackfillTitlesCmd.Flags().IntVar(&backfillTitlesMax, "max", 0, "Stop after this many chunks have been processed (0 = unlimited)")
	memoryBackfillTitlesCmd.Flags().BoolVar(&backfillTitlesDryRun, "dry-run", false, "Only report how many chunks are missing a title")
	memoryBackfillTitlesCmd.Flags().BoolVar(&backfillTitlesJSON, "json", false, "Emit JSON summary instead of human-readable progress")
	memoryCmd.AddCommand(memoryBackfillTitlesCmd)
}

// backfillBatchResponse mirrors api.MemoryTitleBackfillResult on the
// client side. Kept here so the CLI stays a self-contained tool.
type backfillBatchResponse struct {
	Processed int      `json:"processed"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"`
	Remaining int      `json:"remaining"`
	Errors    []string `json:"errors,omitempty"`
}

func runMemoryBackfillTitles(cmd *cobra.Command, args []string) error {
	if backfillTitlesBatchSize < 1 {
		backfillTitlesBatchSize = 1
	}
	if backfillTitlesBatchSize > 100 {
		backfillTitlesBatchSize = 100
	}

	// Always probe Remaining first so the operator knows the scope
	// before any LLM calls fire.
	probe, err := postJSON("/api/v1/memory/backfill-titles?count=true", nil)
	if err != nil {
		return fmt.Errorf("probe remaining: %w", err)
	}
	var probed backfillBatchResponse
	if err := json.Unmarshal(probe, &probed); err != nil {
		return fmt.Errorf("parse probe response: %w", err)
	}

	if backfillTitlesDryRun {
		if backfillTitlesJSON {
			return prettyPrintJSON(probe)
		}
		fmt.Printf("%d chunks missing content_title.\n", probed.Remaining)
		fmt.Println("(dry-run: no LLM calls were made)")
		return nil
	}

	if probed.Remaining == 0 {
		fmt.Println("nothing to do — all chunks already have content_title.")
		return nil
	}

	fmt.Printf("backfilling %d chunks (batch size %d)\n", probed.Remaining, backfillTitlesBatchSize)
	if backfillTitlesMax > 0 && backfillTitlesMax < probed.Remaining {
		fmt.Printf("(stopping after %d per --max)\n", backfillTitlesMax)
	}

	var totals backfillBatchResponse
	prevRemaining := probed.Remaining
	stalledBatches := 0
	for {
		// Honour --max by trimming the batch on the last call.
		nextBatch := backfillTitlesBatchSize
		if backfillTitlesMax > 0 {
			rem := backfillTitlesMax - totals.Processed
			if rem <= 0 {
				break
			}
			if rem < nextBatch {
				nextBatch = rem
			}
		}

		path := fmt.Sprintf("/api/v1/memory/backfill-titles?batch_size=%d", nextBatch)
		raw, err := postJSON(path, nil)
		if err != nil {
			return fmt.Errorf("backfill batch: %w", err)
		}
		var b backfillBatchResponse
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("parse batch response: %w", err)
		}
		totals.Processed += b.Processed
		totals.Succeeded += b.Succeeded
		totals.Failed += b.Failed
		totals.Skipped += b.Skipped
		totals.Remaining = b.Remaining
		for _, e := range b.Errors {
			if len(totals.Errors) < 20 {
				totals.Errors = append(totals.Errors, e)
			}
		}

		if !backfillTitlesJSON {
			fmt.Printf("  processed=%d succeeded=%d failed=%d skipped=%d remaining=%d\n",
				totals.Processed, totals.Succeeded, totals.Failed, totals.Skipped, totals.Remaining)
		}
		// Per-chunk error text stays out of the console — it lands in
		// the daemon log at WARN level (component=memory worker=
		// title-backfill) where operators can grep it without the
		// CLI flooding the foreground.

		// Nothing left → done.
		if b.Remaining == 0 {
			break
		}
		// Defensive: server returned no rows even though Remaining > 0.
		if b.Processed == 0 {
			break
		}
		// Stall detection: failed chunks stay in the pending pool
		// (content_title remains NULL) so without this guard a
		// systemic failure (model misconfigured, gateway down) spins
		// forever re-reading the same rows. Two consecutive batches
		// with zero forward progress is the signal to bail.
		if b.Succeeded == 0 && b.Skipped == 0 && b.Remaining >= prevRemaining {
			stalledBatches++
			if stalledBatches >= 2 {
				fmt.Println()
				fmt.Printf("stalled: %d consecutive batches made no progress.\n", stalledBatches)
				fmt.Println("check the daemon log (component=memory worker=title-backfill, level=warn)")
				fmt.Println("for the upstream error from each failed chunk.")
				return fmt.Errorf("backfill stalled with %d chunks remaining", b.Remaining)
			}
		} else {
			stalledBatches = 0
		}
		prevRemaining = b.Remaining
	}

	if backfillTitlesJSON {
		enc, _ := json.MarshalIndent(totals, "", "  ")
		fmt.Println(string(enc))
		return nil
	}

	fmt.Println()
	fmt.Printf("done. processed=%d succeeded=%d failed=%d skipped=%d remaining=%d\n",
		totals.Processed, totals.Succeeded, totals.Failed, totals.Skipped, totals.Remaining)
	if totals.Failed > 0 {
		fmt.Println("(see daemon log component=memory worker=title-backfill for failure details)")
	}
	return nil
}
