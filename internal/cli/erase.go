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
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

var eraseArtifactYes bool

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
	eraseCmd.AddCommand(eraseArtifactCmd)
	rootCmd.AddCommand(eraseCmd)
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

	svc := &erasure.Service{
		Docs:         postgres.NewExtractedDocumentRepository(db),
		Chunks:       memory.NewRepository(db),
		ArtifactRoot: root,
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
	fmt.Printf("The artifact row %s itself is untouched; delete it separately if that is intended.\n", artifactID)
	return nil
}
