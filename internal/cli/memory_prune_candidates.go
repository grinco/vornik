package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

var (
	pruneCandidatesProject string
	pruneCandidatesSince   time.Duration
	pruneCandidatesLimit   int
	pruneCandidatesJSON    bool
)

var memoryPruneCandidatesCmd = &cobra.Command{
	Use:   "prune-candidates",
	Short: "List chunks that haven't been retrieved in --since (auto-prune candidates)",
	Long: `Print chunk IDs that are indexed for --project but haven't appeared in
any memory_retrieval_audit row since now() - --since. These are the
candidates for the memory feedback loop's prune signal: chunks that
the corpus stores but nothing actually reads.

The command DOES NOT delete anything — it surfaces the list so an
operator can decide. To actually prune, follow up with project-side
tooling or 'vornikctl memory wipe' for the whole project.

Examples:
  vornikctl memory prune-candidates --project assistant
  vornikctl memory prune-candidates --project assistant --since 90d
  vornikctl memory prune-candidates --project assistant --json --limit 500`,
	RunE: runMemoryPruneCandidates,
}

func init() {
	memoryPruneCandidatesCmd.Flags().StringVarP(&pruneCandidatesProject, "project", "p", "", "Project ID (required)")
	memoryPruneCandidatesCmd.Flags().DurationVar(&pruneCandidatesSince, "since", 30*24*time.Hour, "Retrieval lookback window (default 30d)")
	memoryPruneCandidatesCmd.Flags().IntVar(&pruneCandidatesLimit, "limit", 100, "Max candidates to return")
	memoryPruneCandidatesCmd.Flags().BoolVar(&pruneCandidatesJSON, "json", false, "Emit JSON instead of a table")
	_ = memoryPruneCandidatesCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryPruneCandidatesCmd)
}

// pruneCandidatesAuditRepo narrows the persistence interface so the
// CLI test can supply a stub. *postgres.MemoryRetrievalAuditRepository
// satisfies it via its existing method.
type pruneCandidatesAuditRepo interface {
	UnretrievedChunkIDs(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error)
}

func runMemoryPruneCandidates(_ *cobra.Command, _ []string) error {
	if pruneCandidatesProject == "" {
		return fmt.Errorf("--project is required")
	}

	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()

	return doMemoryPruneCandidates(ctx, backend.Repos.MemoryRetrievalAudit, pruneCandidatesProject, pruneCandidatesSince, pruneCandidatesLimit, pruneCandidatesJSON, os.Stdout)
}

// doMemoryPruneCandidates is the testable body. Takes a writer so
// the unit test captures output cleanly.
func doMemoryPruneCandidates(
	ctx context.Context,
	repo pruneCandidatesAuditRepo,
	projectID string,
	window time.Duration,
	limit int,
	asJSON bool,
	out *os.File,
) error {
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}
	if limit <= 0 {
		limit = 100
	}
	since := time.Now().Add(-window)
	ids, err := repo.UnretrievedChunkIDs(ctx, projectID, since, limit)
	if err != nil {
		return fmt.Errorf("query unretrieved chunks: %w", err)
	}
	if asJSON {
		return json.NewEncoder(out).Encode(map[string]any{
			"project":         projectID,
			"since":           since.UTC().Format(time.RFC3339),
			"limit":           limit,
			"candidate_count": len(ids),
			"chunk_ids":       ids,
		})
	}
	if len(ids) == 0 {
		_, _ = fmt.Fprintf(out, "no auto-prune candidates in project %q over the last %s.\n", projectID, window)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%d chunks in %q not retrieved since %s:\n\n", len(ids), projectID, since.Format(time.RFC3339))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHUNK_ID")
	for _, id := range ids {
		_, _ = fmt.Fprintln(tw, id)
	}
	_ = tw.Flush()
	return nil
}
