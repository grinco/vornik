package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

var (
	pruneOrphanedProject string
	pruneOrphanedExecute bool
)

var memoryPruneOrphanedCmd = &cobra.Command{
	Use:   "prune-orphaned-entities",
	Short: "Delete knowledge-graph entities left behind by past deletions (dry-run by default)",
	Long: `Remove knowledge_entities that no chunk mentions any more.

WHY THESE EXIST. Deleting a memory chunk cascades entity_mentions by foreign
key and stops. knowledge_entities and knowledge_edges have no foreign key to
chunks at all, so every deletion that has ever run — retention pruning,
evictions, and Article 17 erasures before 2026-08-21 — left the entities and
edges derived from those chunks in place. Measured on production 2026-08-21:
3,795 stranded entities, 456 of type PERSON and 254 VENDOR, all carrying an
embedding and therefore all still reachable by semantic search.

Erasure now removes what it derives, but a prospective fix does not clean a
spill that already happened. This command is that cleanup.

WHY A COMMAND AND NOT A MIGRATION. A migration that deletes personal data
leaves no operator decision and no audit trail, and "the upgrade did it" is not
an answer to a regulator. So this is deliberate, previewable and audited:
--execute writes an admin_audit row naming the operator and the scope.

WHY NOT PART OF AN ERASURE. An erasure removes what THAT request derived. These
rows cannot be attributed to any request — some may predate mention tracking, or
have been created by the entity resolver without mentions — so sweeping them
under a subject's request would put a false claim in the audit trail.

Dry-run by DEFAULT. It deletes personal data, and the operator sees the count
and its composition before anything happens.

Deletes in batches. The row lock this needs is the same one an ingest takes to
record a mention, so locking a project's whole stranded population at once would
stall ingestion for the length of the run. Candidates another transaction holds
are skipped rather than waited on — a locked row is one something is actively
referencing — and a re-run picks up anything skipped. Partial progress is real
progress: a run that fails part-way still audits what it removed.

An entity is "stranded" only when NO surviving chunk reaches it — neither
through entity_mentions nor through a knowledge_edge citing a chunk that still
exists. The second route matters: the graph pipeline writes a mention only when
an extracted candidate carries a valid character span, so live entities can and
do exist with no mention row. Measured on production 2026-08-21, 522 of 3,796
mention-less entities were still reachable through a live edge.

Deleting an entity also removes its edges by foreign key cascade; the count is
reported.

Examples:
  vornikctl memory prune-orphaned-entities
  vornikctl memory prune-orphaned-entities --project assistant
  vornikctl memory prune-orphaned-entities --execute`,
	Args: cobra.NoArgs,
	RunE: runMemoryPruneOrphaned,
}

func init() {
	memoryPruneOrphanedCmd.Flags().StringVar(&pruneOrphanedProject, "project", "",
		"limit to one project (default: every project)")
	memoryPruneOrphanedCmd.Flags().BoolVar(&pruneOrphanedExecute, "execute", false,
		"actually delete; without it this is a read-only preview")
	memoryCmd.AddCommand(memoryPruneOrphanedCmd)
}

// previewByType renders what the operator saw before deciding, for the audit
// row. Compact on purpose — it goes in a JSON string field.
func previewByType(counts []postgres.OrphanedEntityCount) string {
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d/%d", c.Type, c.Count, c.WithEmbedding))
	}
	return strings.Join(parts, ",")
}

func runMemoryPruneOrphaned(_ *cobra.Command, _ []string) error {
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
	db, err := requirePostgresDB(backend, "memory prune-orphaned-entities")
	if err != nil {
		return err
	}

	repo := postgres.NewErasureDerivedRepository(db)
	counts, err := repo.CountOrphanedEntities(ctx, pruneOrphanedProject)
	if err != nil {
		return fmt.Errorf("count orphaned entities: %w", err)
	}

	scope := pruneOrphanedProject
	if scope == "" {
		scope = "(all projects)"
	}
	total, embedded := 0, 0
	for _, c := range counts {
		total += c.Count
		embedded += c.WithEmbedding
	}

	// "no surviving chunk reaches it", not "no mention" — the two differ by 522
	// rows in production and the header must not misdescribe the predicate the
	// operator is about to authorise.
	fmt.Printf("Entities no surviving chunk reaches (no mention, and no edge citing a "+
		"live chunk) in %s: %d (%d carry an embedding)\n", scope, total, embedded)
	for _, c := range counts {
		fmt.Printf("  %-12s %6d  (%d with embedding)\n", c.Type, c.Count, c.WithEmbedding)
	}
	if total == 0 {
		fmt.Println("\nNothing to prune.")
		return nil
	}

	if !pruneOrphanedExecute {
		// The composition is the point of the preview: an operator deciding
		// whether to delete personal data needs to see WHAT it is, not just how
		// much.
		fmt.Println("\nDRY RUN — nothing was deleted. Re-run with --execute to delete these rows.")
		fmt.Println("Deleting an entity also removes its knowledge_edges by foreign-key cascade.")
		return nil
	}

	// The prune runs in batches and returns what it deleted even when a later
	// batch fails, so pruneErr is handled AFTER the audit row is written. Rows
	// removed by the batches that succeeded are gone whether or not the run
	// finished, and an audit that records only complete runs would leave exactly
	// the partial deletions unrecorded.
	entities, edges, pruneErr := repo.PruneOrphanedEntities(ctx, pruneOrphanedProject)
	fmt.Printf("\nDeleted %d entit(ies) and %d edge(s) removed with them.\n", entities, edges)
	if pruneErr != nil {
		fmt.Printf("The run did NOT finish: %v\n", pruneErr)
	}

	if entities == 0 && pruneErr != nil {
		return fmt.Errorf("prune orphaned entities: %w", pruneErr)
	}

	// Audit BEFORE returning, and never as a best-effort afterthought's excuse
	// for silence: a deletion of personal data with no record of the deletion is
	// itself a compliance failure. A failure to write it is reported, not
	// swallowed — the rows are already gone and the operator has to know the
	// trail is incomplete.
	audit := postgres.NewAdminAuditRepository(db)
	if err := audit.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: cliPrincipal(),
		Source:    "cli",
		Action:    "memory.prune-orphaned-entities",
		Target:    scope,
		// The invocation, not only the outcome: a regulator asking what was run
		// needs the scope and the preview the operator acted on, and the rows
		// themselves are gone.
		After: fmt.Sprintf(
			`{"entities_deleted":%d,"edges_deleted":%d,"with_embedding":%d,`+
				`"scope":%q,"previewed_total":%d,"previewed_by_type":%q,"completed":%t}`,
			entities, edges, embedded, scope, total, previewByType(counts), pruneErr == nil),
	}); err != nil {
		return fmt.Errorf("rows were deleted but the admin_audit row FAILED to write (%w) — "+
			"record this manually: %d entities and %d edges pruned from %s by %s",
			err, entities, edges, scope, cliPrincipal())
	}
	if pruneErr != nil {
		return fmt.Errorf("prune orphaned entities: %w — %d entit(ies) were deleted and "+
			"audited before the failure; re-run to finish", pruneErr, entities)
	}
	return nil
}
