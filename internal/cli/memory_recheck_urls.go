package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/storage"
)

var (
	recheckURLsProject string
	recheckURLsLimit   int
	recheckURLsTimeout time.Duration
	recheckURLsJSON    bool
)

var memoryRecheckURLsCmd = &cobra.Command{
	Use:   "recheck-urls",
	Short: "HEAD-ping every URL in a project's memory chunks and flag dead ones",
	Long: `Walk project_memory_chunks for --project, extract URLs from each chunk's
content, and issue a short-timeout HEAD against every URL. Chunks whose URLs
are all dead are flagged (is_alive=false); chunks with at least one alive URL
are confirmed (is_alive=true). Dead URLs stay indexed — they're just flagged
so consuming agents (researcher, dispatcher) can prefer live hits and warn
when only dead ones survive.

The command is the operator-actionable MVP for URL liveness. A periodic
auto-worker that runs this on a schedule is a follow-up; for now operators
run this on demand when a recent E2E shows agents pulling stale URLs.

Examples:
  vornikctl memory recheck-urls --project assistant
  vornikctl memory recheck-urls --project assistant --limit 100
  vornikctl memory recheck-urls --project assistant --timeout 3s --json`,
	RunE: runMemoryRecheckURLs,
}

func init() {
	memoryRecheckURLsCmd.Flags().StringVarP(&recheckURLsProject, "project", "p", "", "Project ID (required)")
	memoryRecheckURLsCmd.Flags().IntVar(&recheckURLsLimit, "limit", 0, "Max chunks to check (0 = all)")
	memoryRecheckURLsCmd.Flags().DurationVar(&recheckURLsTimeout, "timeout", 5*time.Second, "Per-URL HEAD timeout (default 5s)")
	memoryRecheckURLsCmd.Flags().BoolVar(&recheckURLsJSON, "json", false, "Emit JSON summary instead of human-readable output")
	_ = memoryRecheckURLsCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryRecheckURLsCmd)
}

// recheckURLsRunner is the interface the CLI uses to drive a recheck.
// Defined locally so the unit test can swap in a fake; the production
// path wires *memory.URLLivenessChecker.
type recheckURLsRunner interface {
	RecheckProject(ctx context.Context, projectID string, limit int) (memory.RecheckOutcome, error)
}

func runMemoryRecheckURLs(_ *cobra.Command, _ []string) error {
	if recheckURLsProject == "" {
		return fmt.Errorf("--project is required")
	}
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "memory recheck-urls")
	if err != nil {
		return err
	}
	repo := memory.NewRepository(db)
	checker := memory.NewURLLivenessChecker(repo)
	checker.SetTimeout(recheckURLsTimeout)
	return doMemoryRecheckURLs(ctx, checker, recheckURLsProject, recheckURLsLimit, recheckURLsJSON, os.Stdout)
}

// doMemoryRecheckURLs is the testable body. Takes an injectable
// runner and writer so unit tests can verify behaviour without
// opening a real DB / HTTP connection.
func doMemoryRecheckURLs(
	ctx context.Context,
	runner recheckURLsRunner,
	projectID string,
	limit int,
	asJSON bool,
	out io.Writer,
) error {
	if projectID == "" {
		return fmt.Errorf("--project is required")
	}
	res, err := runner.RecheckProject(ctx, projectID, limit)
	if err != nil {
		return fmt.Errorf("recheck failed: %w", err)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"project":          projectID,
			"chunks_scanned":   res.ChunksScanned,
			"chunks_with_urls": res.ChunksWithURLs,
			"urls_checked":     res.URLsChecked,
			"urls_alive":       res.URLsAlive,
			"urls_dead":        res.URLsDead,
			"chunks_confirmed": res.ChunksConfirmed,
			"chunks_flagged":   res.ChunksFlagged,
		})
	}
	_, _ = fmt.Fprintf(out, "URL liveness recheck for project %q\n", projectID)
	_, _ = fmt.Fprintf(out, "  chunks scanned:    %d\n", res.ChunksScanned)
	_, _ = fmt.Fprintf(out, "  chunks with URLs:  %d\n", res.ChunksWithURLs)
	_, _ = fmt.Fprintf(out, "  URLs checked:      %d (alive=%d, dead=%d)\n",
		res.URLsChecked, res.URLsAlive, res.URLsDead)
	_, _ = fmt.Fprintf(out, "  chunks confirmed:  %d (is_alive=true)\n", res.ChunksConfirmed)
	_, _ = fmt.Fprintf(out, "  chunks flagged:    %d (is_alive=false)\n", res.ChunksFlagged)
	if res.ChunksFlagged > 0 {
		_, _ = fmt.Fprintf(out, "\n%d chunk(s) now flagged with dead URLs. Consuming agents (researcher, dispatcher) will see is_alive=false on these search hits.\n", res.ChunksFlagged)
	}
	return nil
}
