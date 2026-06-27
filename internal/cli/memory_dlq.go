package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/storage"
)

// Memory DLQ operator surface. Mirrors the memory reassign pattern —
// direct DB access rather than HTTP, because the DLQ is an operational
// back-office table rather than something agents ever touch.

var (
	memoryDLQProject string
	memoryDLQLimit   int
	memoryDLQJSON    bool
)

var memoryDLQCmd = &cobra.Command{
	Use:   "dlq",
	Short: "Inspect and replay chunks in the memory embed DLQ",
	Long: `The memory DLQ (memory_embed_dlq table) holds chunks the embed worker
couldn't store on its own — embedder unavailable, dimension mismatch,
oversized content, etc. The worker auto-retries rows whose retry_after
has lapsed; permanently-parked rows (retry_count = -1) need an operator
to either fix the underlying cause and replay, or delete the chunk.`,
}

var memoryDLQListCmd = &cobra.Command{
	Use:   "list",
	Short: "List DLQ entries",
	RunE:  runMemoryDLQList,
}

var memoryDLQReplayCmd = &cobra.Command{
	Use:   "replay <chunkId...>",
	Short: "Move one or more chunks back to the embed queue",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runMemoryDLQReplay,
}

func init() {
	memoryDLQListCmd.Flags().StringVarP(&memoryDLQProject, "project", "p", "", "Filter by project ID")
	memoryDLQListCmd.Flags().IntVarP(&memoryDLQLimit, "limit", "n", 100, "Max rows (1-1000)")
	memoryDLQListCmd.Flags().BoolVar(&memoryDLQJSON, "json", false, "Output JSON instead of table")

	memoryDLQCmd.AddCommand(memoryDLQListCmd, memoryDLQReplayCmd)
	memoryCmd.AddCommand(memoryDLQCmd)
}

func connectMemoryDB(ctx context.Context) (*memory.Repository, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, func() {}, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open database: %w", err)
	}
	cleanup := func() { _ = backend.Close() }
	db, err := requirePostgresDB(backend, "memory dlq")
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return memory.NewRepository(db), cleanup, nil
}

func runMemoryDLQList(cmd *cobra.Command, args []string) error {
	if memoryDLQLimit <= 0 {
		memoryDLQLimit = 100
	}
	if memoryDLQLimit > 1000 {
		memoryDLQLimit = 1000
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo, cleanup, err := connectMemoryDB(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	entries, err := repo.DLQList(ctx, memoryDLQProject, memoryDLQLimit)
	if err != nil {
		return err
	}

	if memoryDLQJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"entries": entries, "total": len(entries)})
	}
	if len(entries) == 0 {
		fmt.Println("(no DLQ entries)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHUNK\tPROJECT\tREASON\tRETRIES\tRETRY_AFTER\tLAST_ERROR")
	for _, e := range entries {
		retryLabel := fmt.Sprintf("%d", e.RetryCount)
		if e.RetryCount < 0 {
			retryLabel = "parked"
		}
		retryAfter := "—"
		if !e.RetryAfter.IsZero() {
			retryAfter = e.RetryAfter.Local().Format("2006-01-02 15:04")
		}
		errText := e.LastError
		if len(errText) > 60 {
			errText = errText[:57] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortenID(e.ChunkID), e.ProjectID, e.Reason, retryLabel, retryAfter, errText)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", len(entries))
	return nil
}

func runMemoryDLQReplay(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, cleanup, err := connectMemoryDB(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	n, err := repo.DLQReplay(ctx, args)
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d chunk(s) from DLQ back to the embed queue: %s\n",
		n, strings.Join(args, ", "))
	return nil
}
