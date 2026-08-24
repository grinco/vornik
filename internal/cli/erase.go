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
	"vornik.io/vornik/internal/erasure"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

var (
	eraseArtifactYes     bool
	eraseArtifactRequest string
)

var eraseCmd = &cobra.Command{
	Use:   "erase",
	Short: "Execute a GDPR Art 17 erasure (irreversible)",
	Long: `Erase personal data derived from a source artifact.

Deleting an artifact on its own does NOT remove what was derived from it. This
command closes that gap: it removes the extraction rows, the memory chunks
built from them, and the extraction's storage directory (extracted text and,
for video, sampled keyframes).

Why a separate command instead of a database cascade: an extraction's data is
partly on disk, where no foreign key reaches; and making artifact deletion
cascade to memory chunks would fire on ordinary retention pruning too and
destroy memory the deployment relies on.`,
}

var eraseArtifactCmd = &cobra.Command{
	Use:   "artifact <artifact-id>",
	Short: "Erase an artifact's extractions, derived memory chunks, and stored files",
	Long: `Show what would be erased for an artifact and, on confirmation, erase it.

Prints the plan and asks for confirmation by default, because the operation is
irreversible. --yes skips the prompt for scripted use.

The artifact ROW itself is not deleted — this removes what was DERIVED from it,
which is the part nothing else cleans up. Delete the artifact separately once
this reports success.

Examples:
  vornikctl erase artifact artifact_20260729005100_8dd1594d353c68e1
  vornikctl erase artifact artifact_2026... --yes`,
	Args: cobra.ExactArgs(1),
	RunE: runEraseArtifact,
}

func init() {
	eraseArtifactCmd.Flags().BoolVar(&eraseArtifactYes, "yes", false,
		"skip the confirmation prompt (for scripted erasure)")
	eraseArtifactCmd.Flags().StringVar(&eraseArtifactRequest, "request", "",
		"the data-subject request id this erasure satisfies; recorded as the authority "+
			"for the knowledge-graph deletions (defaults to an operator-attributed id)")
	eraseCmd.AddCommand(eraseArtifactCmd)
	rootCmd.AddCommand(eraseCmd)
}

// eraseRequestAuthority is what the knowledge-graph deletions are recorded
// under.
//
// --request carries the data-subject request id when this erasure satisfies
// one. Without it the command still runs, because this command also serves
// deletions that are not Art 17 requests at all — retention, a security
// incident, an operator removing their own upload — and refusing would only
// teach operators to type a fabricated request id.
//
// But the fallback must not READ like a request. "cli-erase:alice" in an audit
// trail answers "which request authorised this?" with something that is not a
// request, which is a false attribution rather than a missing one. So the
// fallback names the authority for what it is: the operator's own.
// A regulator reading "operator:alice" learns exactly the true thing — this
// deletion cites no data-subject request.
func eraseRequestAuthority() string {
	if id := strings.TrimSpace(eraseArtifactRequest); id != "" {
		return id
	}
	return "operator:" + currentOperatorIdentity()
}

func runEraseArtifact(_ *cobra.Command, args []string) error {
	artifactID := strings.TrimSpace(args[0])
	if artifactID == "" {
		return fmt.Errorf("artifact id is required")
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
	db, err := requirePostgresDB(backend, "erase artifact")
	if err != nil {
		return err
	}

	root := cfg.Storage.ArtifactsPath
	if strings.TrimSpace(root) == "" {
		// Without a root there is no containment boundary for the recursive
		// delete, so refuse rather than guess.
		return fmt.Errorf("storage.artifacts_path is not configured; refusing to erase files without a containment root")
	}

	// The derived cascade is wired here because this command is an Art 17
	// erasure by its own definition, and an erasure that removed the chunks and
	// left the entities, edges and quarantined copy built from them would report
	// success over data it did not remove (design §4.14).
	svc := &erasure.Service{
		Docs:         postgres.NewExtractedDocumentRepository(db),
		Chunks:       memory.NewRepository(db),
		Derived:      postgres.NewErasureDerivedRepository(db),
		RequestID:    eraseRequestAuthority(),
		ArtifactRoot: root,
	}

	if strings.TrimSpace(eraseArtifactRequest) == "" {
		// Said before the confirmation, not after: if this erasure IS satisfying
		// a data-subject request, the operator wants to abort and pass --request
		// rather than discover the gap in the audit trail later.
		fmt.Printf("NOTE: no --request given. The knowledge-graph deletions will be "+
			"recorded under %q, which cites no data-subject request.\n", eraseRequestAuthority())
	}

	plan, err := svc.Plan(ctx, artifactID)
	if err != nil {
		return err
	}
	fmt.Print(plan.Summary())

	if len(plan.Documents) == 0 && plan.DirectChunkCount == 0 {
		fmt.Println("\nNothing derived from this artifact was found — nothing to erase.")
		return nil
	}

	if !eraseArtifactYes {
		fmt.Print("\nType 'erase' to proceed: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if strings.TrimSpace(answer) != "erase" {
			fmt.Println("Aborted. Nothing was deleted.")
			return nil
		}
	}

	res, err := svc.Erase(ctx, artifactID)
	if err != nil {
		return err
	}
	fmt.Printf("\nErased: %d extracted document(s), %d memory chunk(s), %d storage directory/ies.\n",
		res.DocumentsDeleted, res.ChunksDeleted, res.DirectoriesRemoved)
	// Printed unconditionally, zeros included. These rows outlived erasures
	// reported as complete until 2026-08-21; once they are gone this line is the
	// only evidence the erasure covered them.
	fmt.Printf("Derived data: %d knowledge entit(ies), %d graph edge(s), %d quarantined "+
		"pre-ingest cop(ies).\n",
		res.Derived.Entities, res.Derived.Edges, res.Derived.Quarantined)
	fmt.Printf("The artifact row %s itself is untouched; delete it separately if that is intended.\n", artifactID)

	// RECORD IT. Until 2026-08-21 this command printed the counts and persisted
	// nothing at all — not the chunks, not the files, not the derived rows. The
	// subject-erase path stores a hashed report against the request, and the
	// eviction path writes tombstones plus a run header; this one, which the
	// help calls a GDPR Art 17 erasure, left no trace whatsoever once the
	// terminal scrolled. A deletion of personal data with no record of the
	// deletion is itself non-compliant.
	//
	// admin_audit rather than a new table: this is an operator-run destructive
	// command, the same shape as `memory prune-orphaned-entities`, and the
	// authority recorded is the one that gated the graph deletes.
	audit := postgres.NewAdminAuditRepository(db)
	if err := audit.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: cliPrincipal(),
		Source:    "cli",
		Action:    "erasure.artifact",
		Target:    artifactID,
		After: fmt.Sprintf(
			`{"authority":%q,"documents_deleted":%d,"chunks_deleted":%d,`+
				`"directories_removed":%d,"graph_entities_deleted":%d,`+
				`"graph_edges_deleted":%d,"quarantined_copies_deleted":%d}`,
			svc.RequestID, res.DocumentsDeleted, res.ChunksDeleted, res.DirectoriesRemoved,
			res.Derived.Entities, res.Derived.Edges, res.Derived.Quarantined),
	}); err != nil {
		return fmt.Errorf("the erasure COMPLETED but its admin_audit row failed to write (%w) — "+
			"record this manually: artifact %s erased by %s under authority %q, "+
			"%d document(s), %d chunk(s), %d graph entit(ies), %d edge(s), %d quarantined cop(ies)",
			err, artifactID, cliPrincipal(), svc.RequestID,
			res.DocumentsDeleted, res.ChunksDeleted,
			res.Derived.Entities, res.Derived.Edges, res.Derived.Quarantined)
	}
	return nil
}
