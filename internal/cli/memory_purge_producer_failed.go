package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/storage"
)

var (
	purgeProducerProject     string
	purgeProducerDryRun      bool
	purgeProducerConfirm     bool
	purgeProducerExpectCount int
)

var memoryPurgeProducerFailedCmd = &cobra.Command{
	Use:   "purge-producer-failed",
	Short: "Retro-clean RAG chunks produced by failed/cancelled executions",
	Long: `Hard-evict every memory chunk whose PRODUCING EXECUTION ended
unsuccessfully (FAILED or CANCELLED). This is the retro-clean half of the
RAG-ingest producer-success gate (LLD 2026-07-12-rag-ingest-producer-success-
gate §5): the gate stops NEW failed-task outputs from being ingested; this
command removes the PRE-GATE garbage already in the store — e.g. the
2026-07-12 person-dossier incident where a failed research task's wrong-people
candidates were ingested and now surface on recall as if they were findings.

Candidate selection joins chunks → artifacts → executions and filters on
executions.status IN ('FAILED','CANCELLED'), so:
  - a task's failed-execution chunks ARE selected;
  - a task's successfully-retried (COMPLETED) execution's chunks are NOT;
  - companion notes / uploaded docs (empty task_id) are never selected.

Deletion reuses the audited hard-evict path (per-chunk memory_eviction_audit
tombstone). It is DESTRUCTIVE and IRREVERSIBLE — restore needs a DB restore.

Safety flow (stricter than a single --confirm, because this is a bulk op):
  1. Run --dry-run first: prints the candidate chunk IDs + count.
  2. Re-run with --confirm --expect-count=N, where N is the dry-run count.
     A mismatch (the set changed between runs) aborts without deleting.

Examples:
  vornikctl memory purge-producer-failed --project assistant --dry-run
  vornikctl memory purge-producer-failed --project assistant --confirm --expect-count 17`,
	RunE: runMemoryPurgeProducerFailed,
}

func init() {
	memoryPurgeProducerFailedCmd.Flags().StringVarP(&purgeProducerProject, "project", "p", "", "Project ID (required)")
	memoryPurgeProducerFailedCmd.Flags().BoolVar(&purgeProducerDryRun, "dry-run", false, "List candidate chunk IDs + count without deleting")
	memoryPurgeProducerFailedCmd.Flags().BoolVar(&purgeProducerConfirm, "confirm", false, "REQUIRED (with --expect-count) to actually delete")
	memoryPurgeProducerFailedCmd.Flags().IntVar(&purgeProducerExpectCount, "expect-count", -1, "The count from your --dry-run; a mismatch aborts (bulk-op safety)")
	_ = memoryPurgeProducerFailedCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryPurgeProducerFailedCmd)
}

func runMemoryPurgeProducerFailed(_ *cobra.Command, _ []string) error {
	if strings.TrimSpace(purgeProducerProject) == "" {
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
	db, err := requirePostgresDB(backend, "memory purge-producer-failed")
	if err != nil {
		return err
	}

	repo := memory.NewRepository(db)
	chunkIDs, err := repo.ListChunkIDsByFailedProducer(ctx, purgeProducerProject)
	if err != nil {
		return fmt.Errorf("list failed-producer chunks: %w", err)
	}

	fmt.Printf("%d chunk(s) produced by FAILED/CANCELLED executions in project %q\n",
		len(chunkIDs), purgeProducerProject)
	if len(chunkIDs) == 0 {
		fmt.Println("(nothing to purge)")
		return nil
	}

	if purgeProducerDryRun || !purgeProducerConfirm {
		for _, id := range chunkIDs {
			fmt.Printf("  - %s\n", id)
		}
		if purgeProducerDryRun {
			fmt.Printf("\n(dry-run: no changes made) — to delete: --confirm --expect-count %d\n", len(chunkIDs))
			return nil
		}
		// No --confirm: refuse, but report the count so the operator can
		// pass --expect-count on the real run (a built-in dry run).
		return fmt.Errorf("purge is destructive and irreversible — re-run with --confirm --expect-count %d", len(chunkIDs))
	}

	// --confirm requires an explicit --expect-count that matches the current
	// candidate set. A mismatch means the set changed since the dry-run
	// (a new failed task landed, or a prior purge already ran) — abort so the
	// operator re-inspects rather than deleting a set they didn't preview.
	if err := checkExpectCount(purgeProducerExpectCount, len(chunkIDs)); err != nil {
		return err
	}

	// Reuse the audited hard-evict path (per-chunk tombstone in
	// memory_eviction_audit). Searcher is nil — purge works on explicit IDs.
	corrector := memory.NewCorrector(repo, nil)
	reason := "producer-failed retro-clean (RAG-ingest producer-success gate §5)"
	res, err := corrector.HardEvict(ctx, purgeProducerProject, chunkIDs, reason, currentOperatorIdentity())
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}

	fmt.Printf("purged %d of %d chunk(s) under project %q\n", res.Count(), len(chunkIDs), purgeProducerProject)
	if res.Count() < len(chunkIDs) {
		fmt.Println("(non-deleted IDs were stale or already evicted)")
	}
	fmt.Printf("derived data: %d knowledge entit(ies), %d graph edge(s), %d quarantined "+
		"pre-ingest cop(ies), %d cached embedding(s)\n",
		res.Derived.Entities, res.Derived.Edges, res.QuarantinedCopiesDeleted,
		res.EmbeddingCacheKeysDeleted)
	return nil
}

// checkExpectCount is the bulk-op safety gate: --confirm must carry an
// --expect-count (>= 0) that exactly matches the current candidate set. A
// negative expect means the operator omitted the flag; a mismatch means the
// set changed since their --dry-run. Extracted for unit testing (the rest of
// runMemoryPurgeProducerFailed needs a live DB).
func checkExpectCount(expect, actual int) error {
	if expect < 0 {
		return fmt.Errorf("--confirm requires --expect-count (the count from your --dry-run); current candidate set is %d", actual)
	}
	if expect != actual {
		return fmt.Errorf("--expect-count %d does not match the current candidate set of %d — the set changed since your dry-run; re-run --dry-run and retry",
			expect, actual)
	}
	return nil
}
