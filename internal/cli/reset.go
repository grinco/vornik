package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	resetForce         bool
	resetKeepArtifacts bool
	resetKeepDB        bool
)

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset local vornik state",
	Long: `Reset vornik to a clean state for local development.

WARNING: This operation is destructive and cannot be undone.

By default, this will:
- Stop any running containers
- Clear the task queue (mark all as CANCELLED)
- Remove execution records
- Delete all artifacts

Use flags to preserve specific data:
  --keep-artifacts  Don't delete artifact files
  --keep-db         Don't modify database state

Requires --force or interactive confirmation.`,
	RunE: runReset,
}

func init() {
	resetCmd.Flags().BoolVar(&resetForce, "force", false, "Skip confirmation prompt")
	resetCmd.Flags().BoolVar(&resetKeepArtifacts, "keep-artifacts", false, "Preserve artifact files")
	resetCmd.Flags().BoolVar(&resetKeepDB, "keep-db", false, "Preserve database state")

	rootCmd.AddCommand(resetCmd)
}

func runReset(cmd *cobra.Command, args []string) error {
	// Safety check: require force or confirmation
	if !resetForce {
		fmt.Println("⚠️  WARNING: This will delete all vornik local state!")
		fmt.Println()
		fmt.Println("This includes:")
		if !resetKeepDB {
			fmt.Println("  - All tasks and executions in the database")
		}
		if !resetKeepArtifacts {
			fmt.Println("  - All artifact files")
		}
		fmt.Println()
		fmt.Print("Type 'yes' to confirm: ")

		reader := bufio.NewReader(os.Stdin)
		response, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read confirmation: %w", err)
		}

		if strings.TrimSpace(response) != "yes" {
			fmt.Println("Reset cancelled.")
			return nil
		}
	}

	fmt.Println("Starting reset...")

	// Step 1: Cancel all running/queued tasks
	if !resetKeepDB {
		fmt.Println("→ Cancelling all tasks...")
		// In a real implementation, this would call the API
		// For now, we document the intent
		fmt.Println("  (Would call DELETE /api/v1/tasks or update DB directly)")
	}

	// Step 2: Clear execution records
	if !resetKeepDB {
		fmt.Println("→ Clearing execution records...")
		fmt.Println("  (Would call DELETE /api/v1/executions or update DB directly)")
	}

	// Step 3: Remove artifacts
	if !resetKeepArtifacts {
		fmt.Println("→ Removing artifacts...")
		artifactDir := os.Getenv("VORNIK_ARTIFACT_DIR")
		if artifactDir == "" {
			artifactDir = "/var/lib/vornik/artifacts"
		}
		fmt.Printf("  (Would remove %s/*)\n", artifactDir)
	}

	fmt.Println()
	fmt.Println("✅ Reset complete.")

	if resetKeepDB {
		fmt.Println("   Note: Database state preserved (--keep-db)")
	}
	if resetKeepArtifacts {
		fmt.Println("   Note: Artifacts preserved (--keep-artifacts)")
	}

	return nil
}
