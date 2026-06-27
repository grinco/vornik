package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/storage"
)

var (
	reembedProject  string
	reembedJSON     bool
	reembedNoWatch  bool
	reembedInterval time.Duration
)

var memoryReembedCmd = &cobra.Command{
	Use:   "reembed",
	Short: "Re-enqueue every chunk in a project for embedding",
	Long: `Push every chunk in --project back onto memory_embed_queue so the
worker re-embeds them with the currently-configured model + dimension.
Use after upgrading the embedding model or changing
memory.embedding_dimension in vornik.yaml.

The command only inserts queue rows — it doesn't touch existing
embeddings; the worker overwrites those one batch at a time. Safe to
interrupt and re-run; queued rows for chunks that already came around
are skipped via ON CONFLICT.

By default the command stays attached after enqueueing and reports
progress every few seconds until the queue drains. Pass --no-watch
to detach immediately (e.g. for use inside scripts where you want
the parent process to handle the polling).

Examples:
  vornikctl memory reembed --project assistant
  vornikctl memory reembed --project assistant --no-watch
  vornikctl memory reembed --project assistant --interval 1s
  vornikctl memory reembed --project assistant --json`,
	RunE: runMemoryReembed,
}

func init() {
	memoryReembedCmd.Flags().StringVarP(&reembedProject, "project", "p", "", "Project ID (required)")
	memoryReembedCmd.Flags().BoolVar(&reembedJSON, "json", false, "Emit machine-readable JSON summary (implies --no-watch)")
	memoryReembedCmd.Flags().BoolVar(&reembedNoWatch, "no-watch", false, "Detach after enqueueing instead of polling for progress")
	memoryReembedCmd.Flags().DurationVar(&reembedInterval, "interval", 3*time.Second, "Progress poll interval (with --watch)")
	_ = memoryReembedCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryReembedCmd)
}

func runMemoryReembed(_ *cobra.Command, _ []string) error {
	if reembedProject == "" {
		return fmt.Errorf("--project is required")
	}

	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Two-phase context: short timeout for the enqueue, separate
	// cancellable context for the long-running watch loop (so a long
	// re-embed run doesn't get killed by the enqueue timeout).
	enqueueCtx, cancelEnqueue := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelEnqueue()

	backend, err := storage.Open(enqueueCtx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()

	db, err := requirePostgresDB(backend, "memory reembed")
	if err != nil {
		return err
	}
	repo := memory.NewRepository(db)
	enqueued, err := repo.RequeueAllForEmbedding(enqueueCtx, reembedProject)
	if err != nil {
		return fmt.Errorf("re-enqueue: %w", err)
	}

	if reembedJSON {
		return prettyPrintJSON([]byte(fmt.Sprintf(
			`{"project":"%s","enqueued":%d}`, reembedProject, enqueued)))
	}

	if enqueued == 0 {
		fmt.Printf("nothing to do — no chunks for project %q (or all already queued).\n", reembedProject)
		return nil
	}
	fmt.Printf("queued %d chunks for re-embedding in project %q.\n", enqueued, reembedProject)

	if reembedNoWatch {
		fmt.Println("the embed worker will pick them up on its next tick (default 5s).")
		return nil
	}

	// Watch mode: poll the queue depth until it drains.
	fmt.Printf("watching progress (poll every %s, ctrl-C to detach)...\n", reembedInterval)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	// SIGINT detaches the watch but DOES NOT cancel the work — the
	// worker keeps draining; the operator can re-check with
	// `vornikctl memory stats` anytime.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancelWatch()
	}()

	return watchReembedProgress(watchCtx, repo, reembedProject, enqueued, reembedInterval)
}

// watchReembedProgress polls Stats() and prints a one-line progress
// update each tick. Exits cleanly when the queue depth reaches 0 or
// when ctx is cancelled (operator interrupted). Pure I/O; covered by
// a unit test that supplies a deterministic stats provider.
func watchReembedProgress(ctx context.Context, repo reembedStatsProvider, projectID string, initial int, interval time.Duration) error {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	start := time.Now()

	// Print one immediate sample so the operator sees activity before
	// the first sleep cycle.
	printOne := func() (depth int, ok bool) {
		stats, err := repo.Stats(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stats poll: %v\n", err)
			return -1, false
		}
		for _, s := range stats {
			if s.ProjectID != projectID {
				continue
			}
			done := initial - int(s.QueueDepth)
			if done < 0 {
				done = 0
			}
			elapsed := time.Since(start).Round(time.Second)
			fmt.Printf("  [%s] %d/%d embedded; queue depth %d\n", elapsed, done, initial, s.QueueDepth)
			return int(s.QueueDepth), true
		}
		// Project not in Stats — likely zero chunks (already done).
		fmt.Printf("  no stats row for project %q (likely drained already)\n", projectID)
		return 0, true
	}

	depth, ok := printOne()
	if ok && depth == 0 {
		fmt.Printf("done. %d chunks re-embedded in %s.\n", initial, time.Since(start).Round(time.Second))
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("detached. the worker keeps draining in the background.")
			fmt.Println("re-check with: vornikctl memory stats --project " + projectID)
			return nil
		case <-ticker.C:
			depth, ok := printOne()
			if !ok {
				continue
			}
			if depth == 0 {
				fmt.Printf("done. %d chunks re-embedded in %s.\n", initial, time.Since(start).Round(time.Second))
				return nil
			}
		}
	}
}

// reembedStatsProvider narrows the Repository surface so the watch
// loop's unit test doesn't need a sqlmock harness. *memory.Repository
// satisfies it via the Stats method already on the public API.
type reembedStatsProvider interface {
	Stats(ctx context.Context) ([]memory.ProjectMemoryStats, error)
}
