package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

var (
	backfillMentionsProject string
	backfillMentionsExecute bool
)

var memoryBackfillMentionsCmd = &cobra.Command{
	Use:   "backfill-entity-mentions",
	Short: "Reconstruct entity→chunk links the extractor failed to record (dry-run by default)",
	Long: `Restore entity_mentions rows that the graph pipeline never wrote.

WHY THEY ARE MISSING. Until 2026-08-21 the pipeline wrote a mention row only
when the entity extractor returned a usable character span, and it discarded the
error when the insert failed. An entity whose offsets the model omitted, or got
out of range, therefore had no mention row for its entire life — while being
perfectly live data.

WHY THAT MATTERS. entity_mentions is how every deletion path decides whether an
entity still belongs to a surviving chunk. An entity with no mention reads as
stranded even when a live chunk produced it, which cuts both ways: it can be
pruned as an orphan, and erasing or evicting a DIFFERENT chunk that did mention
it destroys a row the survivor legitimately produced.

WHY IT IS REPAIRABLE. knowledge_edges.source_chunks records which chunks
evidenced an edge, and the relationship stage builds edges only from entities
resolved in that chunk. An edge citing a chunk that still exists is therefore
direct evidence that the chunk mentioned both endpoints. The link is
reconstructed from what the pipeline DID record, not invented.

The offsets are not reconstructable and are not guessed: rows land with
char_start 0 and char_end NULL — exactly what the fixed pipeline writes when the
extractor returns no span.

Additive only. It inserts rows and deletes nothing, so the worst case if the
reasoning is wrong is a link that keeps an entity alive rather than one that
removes it. Idempotent; safe to re-run.

Scoped to entities with NO mention at all. An entity missing one mention among
several still reads as live to every consumer, so repairing it would change
nothing and widen a targeted repair into a rewrite of the table.

Examples:
  vornikctl memory backfill-entity-mentions
  vornikctl memory backfill-entity-mentions --project assistant --execute`,
	Args: cobra.NoArgs,
	RunE: runMemoryBackfillMentions,
}

func init() {
	memoryBackfillMentionsCmd.Flags().StringVar(&backfillMentionsProject, "project", "",
		"limit to one project (default: every project)")
	memoryBackfillMentionsCmd.Flags().BoolVar(&backfillMentionsExecute, "execute", false,
		"actually write the rows; without it this is a read-only preview")
	memoryCmd.AddCommand(memoryBackfillMentionsCmd)
}

func runMemoryBackfillMentions(_ *cobra.Command, _ []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "memory backfill-entity-mentions")
	if err != nil {
		return err
	}

	repo := postgres.NewMentionBackfillRepository(db)
	links, entities, err := repo.CountReconstructableMentions(ctx, backfillMentionsProject)
	if err != nil {
		return fmt.Errorf("count reconstructable mentions: %w", err)
	}

	scope := backfillMentionsProject
	if scope == "" {
		scope = "(all projects)"
	}
	fmt.Printf("Reconstructable entity→chunk links in %s: %d, rescuing %d entit(ies) "+
		"that currently have no mention at all\n", scope, links, entities)
	if links == 0 {
		fmt.Println("\nNothing to repair.")
		return nil
	}

	if !backfillMentionsExecute {
		fmt.Println("\nDRY RUN — nothing was written. Re-run with --execute to insert these rows.")
		fmt.Println("Rows land with char_start 0 and char_end NULL: the link is recoverable, " +
			"the offsets are not, and guessing them would be worse than leaving them absent.")
		return nil
	}

	inserted, err := repo.BackfillMentionsFromEdges(ctx, backfillMentionsProject)
	if err != nil {
		return fmt.Errorf("backfill entity mentions: %w", err)
	}
	fmt.Printf("\nWrote %d entity→chunk link(s).\n", inserted)

	// Audited like the prune, though this one only adds rows: an operator
	// looking at why an entity's provenance changed should find the run that
	// changed it.
	audit := postgres.NewAdminAuditRepository(db)
	if err := audit.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: cliPrincipal(),
		Source:    "cli",
		Action:    "memory.backfill-entity-mentions",
		Target:    scope,
		After: fmt.Sprintf(`{"links_written":%d,"previewed_links":%d,"entities_rescued":%d}`,
			inserted, links, entities),
	}); err != nil {
		return fmt.Errorf("rows were written but the admin_audit row FAILED (%w) — "+
			"record this manually: %d links added to %s by %s",
			err, inserted, scope, cliPrincipal())
	}
	return nil
}
