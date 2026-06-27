package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

var (
	memoryWipeProject        string
	memoryWipeDryRun         bool
	memoryWipeYes            bool
	memoryWipeKeepQuarantine bool
	memoryWipeKeepAudit      bool
	memoryWipeKeepGraph      bool
)

var memoryWipeCmd = &cobra.Command{
	Use:   "wipe",
	Short: "Delete every memory artifact for one project",
	Long: `Wipe one project's memory state in a single transaction:

  - project_memory_chunks            (chunk content + embeddings)
  - memory_embed_queue / dlq         (cascade from chunks)
  - knowledge_entities / edges       (KG nodes + relationships, project-scoped)
  - entity_mentions                  (cascade from entities + chunks)
  - project_memory_quarantine        (rejected chunks; --keep-quarantine to preserve)
  - project_ingest_queue             (pending ingest items)
  - corpus_epochs / corpus_epochs_active (snapshot history)
  - memory_retrieval_audit           (search history; --keep-audit to preserve)

This is irreversible. Use --dry-run first to see counts, then run
without it (you'll get a confirmation prompt unless --yes is set).

Examples:
  vornikctl memory wipe --project assistant --dry-run
  vornikctl memory wipe --project assistant
  vornikctl memory wipe --project assistant --yes --keep-quarantine

What it does NOT touch:
  - tasks, executions, artifacts (use those tools separately)
  - chunks already reassigned to other projects (only filters by project_id)
  - Source files on disk (this is purely a DB wipe)`,
	RunE: runMemoryWipe,
}

func init() {
	memoryWipeCmd.Flags().StringVarP(&memoryWipeProject, "project", "p", "", "Project ID (required)")
	memoryWipeCmd.Flags().BoolVar(&memoryWipeDryRun, "dry-run", false, "Show counts without deleting")
	memoryWipeCmd.Flags().BoolVar(&memoryWipeYes, "yes", false, "Skip confirmation prompt")
	memoryWipeCmd.Flags().BoolVar(&memoryWipeKeepQuarantine, "keep-quarantine", false, "Preserve project_memory_quarantine rows")
	memoryWipeCmd.Flags().BoolVar(&memoryWipeKeepAudit, "keep-audit", false, "Preserve memory_retrieval_audit rows")
	memoryWipeCmd.Flags().BoolVar(&memoryWipeKeepGraph, "keep-graph", false, "Preserve knowledge_entities / edges / mentions")
	_ = memoryWipeCmd.MarkFlagRequired("project")

	memoryCmd.AddCommand(memoryWipeCmd)
}

func runMemoryWipe(cmd *cobra.Command, args []string) error {
	if memoryWipeProject == "" {
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
	db, err := requirePostgresDB(backend, "memory wipe")
	if err != nil {
		return err
	}

	// Count what would be deleted, per table. Cheap (each query is
	// indexed on project_id). Cascade-only tables (memory_embed_queue,
	// entity_mentions) aren't counted directly — their rows disappear
	// with their parent and the count would over-state if we summed
	// independently.
	counts := map[string]int{}
	queries := []struct {
		label string
		sql   string
		skip  bool
	}{
		{"chunks", `SELECT COUNT(*) FROM project_memory_chunks WHERE project_id = $1`, false},
		{"kg_entities", `SELECT COUNT(*) FROM knowledge_entities WHERE project_id = $1`, memoryWipeKeepGraph},
		{"kg_edges", `SELECT COUNT(*) FROM knowledge_edges WHERE project_id = $1`, memoryWipeKeepGraph},
		{"quarantine", `SELECT COUNT(*) FROM project_memory_quarantine WHERE project_id = $1`, memoryWipeKeepQuarantine},
		{"ingest_queue", `SELECT COUNT(*) FROM project_ingest_queue WHERE project_id = $1`, false},
		{"epochs", `SELECT COUNT(*) FROM corpus_epochs WHERE project_id = $1`, false},
		{"retrieval_audit", `SELECT COUNT(*) FROM memory_retrieval_audit WHERE project_id = $1`, memoryWipeKeepAudit},
	}
	for _, q := range queries {
		if q.skip {
			counts[q.label] = -1 // sentinel for "skipped by flag"
			continue
		}
		var n int
		if err := db.QueryRowContext(ctx, q.sql, memoryWipeProject).Scan(&n); err != nil {
			return fmt.Errorf("count %s: %w", q.label, err)
		}
		counts[q.label] = n
	}

	fmt.Printf("Memory wipe plan for project %q:\n", memoryWipeProject)
	for _, q := range queries {
		v := counts[q.label]
		if v < 0 {
			fmt.Printf("  %-18s  (kept by flag)\n", q.label)
			continue
		}
		fmt.Printf("  %-18s  %d rows\n", q.label, v)
	}
	fmt.Println()

	total := 0
	for _, v := range counts {
		if v > 0 {
			total += v
		}
	}
	if total == 0 {
		fmt.Println("nothing to delete.")
		return nil
	}

	if memoryWipeDryRun {
		fmt.Println("(dry-run: no changes made)")
		return nil
	}

	if !memoryWipeYes {
		fmt.Printf("Type %q to confirm deletion of %d total rows: ", memoryWipeProject, total)
		reader := bufio.NewReader(os.Stdin)
		got, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if strings.TrimSpace(got) != memoryWipeProject {
			return fmt.Errorf("confirmation mismatch — aborting (you typed %q, expected %q)",
				strings.TrimSpace(got), memoryWipeProject)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	type deletePass struct {
		label string
		sql   string
		skip  bool
	}
	// Order matters when the dependent table doesn't cascade —
	// retrieval_audit doesn't have a FK on chunks/projects so it's
	// independent. KG entities cascade to edges + mentions; chunks
	// cascade to embed_queue/dlq + mentions.
	passes := []deletePass{
		// 1. KG first — entities CASCADE to edges + mentions before
		//    chunks would have nuked the mentions independently.
		{"knowledge_edges (project)", `DELETE FROM knowledge_edges WHERE project_id = $1`, memoryWipeKeepGraph},
		{"knowledge_entities", `DELETE FROM knowledge_entities WHERE project_id = $1`, memoryWipeKeepGraph},
		// 2. Chunks — cascades to embed_queue, embed_dlq, remaining
		//    entity_mentions (already gone if KG was wiped).
		{"project_memory_chunks", `DELETE FROM project_memory_chunks WHERE project_id = $1`, false},
		// 3. Quarantine, ingest queue, epochs, audit — independent.
		{"project_memory_quarantine", `DELETE FROM project_memory_quarantine WHERE project_id = $1`, memoryWipeKeepQuarantine},
		{"project_ingest_queue", `DELETE FROM project_ingest_queue WHERE project_id = $1`, false},
		{"corpus_epochs", `DELETE FROM corpus_epochs WHERE project_id = $1`, false},
		{"memory_retrieval_audit", `DELETE FROM memory_retrieval_audit WHERE project_id = $1`, memoryWipeKeepAudit},
	}
	for _, p := range passes {
		if p.skip {
			continue
		}
		res, err := tx.ExecContext(ctx, p.sql, memoryWipeProject)
		if err != nil {
			return fmt.Errorf("delete %s: %w", p.label, err)
		}
		n, _ := res.RowsAffected()
		fmt.Printf("  deleted %-30s  %d\n", p.label, n)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Printf("\nproject %q memory wiped.\n", memoryWipeProject)
	return nil
}
