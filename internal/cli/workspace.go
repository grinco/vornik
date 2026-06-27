package cli

// Workspace administration CLI. Today the only verb is
// `canonicalise` — migrates per-project workspaces from the
// legacy autonomy/ layout to the canonical .autonomy/
// convention. See
// https://docs.vornik.io §2.
//
//   vornikctl workspace canonicalise [--workspaces-root <path>]
//                                   [--project <id>]
//                                   [--dry-run] [--json]
//
// Runs LOCALLY against the filesystem the daemon is running
// against — does not talk to the daemon. The operator is
// responsible for executing on the same host (typical for
// single-node deployments). Multi-host follow-on is a future
// "remote canonicalise" verb that hits a daemon endpoint.

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/workspacecanonicalise"
)

var (
	workspaceCmd = &cobra.Command{
		Use:   "workspace",
		Short: "Workspace administration (canonicalise the autonomy-context layout, etc.)",
	}

	workspaceCanonicaliseCmd = &cobra.Command{
		Use:   "canonicalise",
		Short: "Migrate every project workspace from autonomy/ to .autonomy/",
		Long: `Walks the project workspace root and renames every project's
autonomy/ directory to .autonomy/. Mixed projects (both
directories present) are reported but not touched — the
operator decides manually because merging risks data loss.

By default operates against the workspace root from the
daemon's config (Runtime.ProjectWorkspacePath). Override via
--workspaces-root or scope to one project via --project.

Use --dry-run to preview the plan without touching the
filesystem.`,
		RunE: runWorkspaceCanonicalise,
	}

	workspaceCanonRoot    string
	workspaceCanonProject string
	workspaceCanonDryRun  bool
	workspaceCanonJSON    bool
)

func init() {
	workspaceCanonicaliseCmd.Flags().StringVar(&workspaceCanonRoot, "workspaces-root", "", "Project workspace root path (defaults to $VORNIK_PROJECT_WORKSPACE_PATH or /var/lib/vornik/workspaces)")
	workspaceCanonicaliseCmd.Flags().StringVar(&workspaceCanonProject, "project", "", "Migrate only this project (otherwise walks every subdir under --workspaces-root)")
	workspaceCanonicaliseCmd.Flags().BoolVar(&workspaceCanonDryRun, "dry-run", false, "Print the plan without performing any rename")
	workspaceCanonicaliseCmd.Flags().BoolVar(&workspaceCanonJSON, "json", false, "Output JSON instead of a table")

	workspaceCmd.AddCommand(workspaceCanonicaliseCmd)
	rootCmd.AddCommand(workspaceCmd)
}

func runWorkspaceCanonicalise(_ *cobra.Command, _ []string) error {
	root := resolveWorkspacesRoot()
	if root == "" {
		return fmt.Errorf("workspaces root not configured: pass --workspaces-root or set $VORNIK_PROJECT_WORKSPACE_PATH")
	}

	var results []workspacecanonicalise.Result
	if workspaceCanonProject != "" {
		one, err := workspacecanonicalise.CanonicaliseOne(root, workspaceCanonProject, workspaceCanonDryRun)
		if err != nil {
			return err
		}
		results = []workspacecanonicalise.Result{one}
	} else {
		var err error
		if workspaceCanonDryRun {
			results, err = workspacecanonicalise.Scan(root)
		} else {
			results, err = workspacecanonicalise.CanonicaliseAll(root)
		}
		if err != nil {
			return err
		}
	}

	if workspaceCanonJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "PROJECT\tOUTCOME\tNOTE"); err != nil {
		return err
	}
	for _, r := range results {
		note := ""
		switch r.Outcome {
		case workspacecanonicalise.OutcomeMigrated:
			if workspaceCanonDryRun {
				note = "would migrate autonomy/ → .autonomy/"
			} else {
				note = "migrated autonomy/ → .autonomy/"
			}
		case workspacecanonicalise.OutcomeMixed:
			note = "BOTH dirs present — resolve manually"
		case workspacecanonicalise.OutcomeAlreadyCanonical:
			note = "already on .autonomy/"
		case workspacecanonicalise.OutcomeNoConvention:
			note = "no autonomy-context directory"
		case workspacecanonicalise.OutcomeError:
			note = "error: " + r.Error
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", r.ProjectID, r.Outcome, note); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Summary footer — operators care about how many actually
	// moved vs how many still need manual attention.
	migrated := workspacecanonicalise.CountLegacy(results)
	mixed := workspacecanonicalise.CountMixed(results)
	if workspaceCanonDryRun {
		fmt.Printf("\nDry-run summary: %d would migrate · %d mixed (manual) · %d total\n", migrated, mixed, len(results))
		if migrated > 0 {
			fmt.Println("Re-run without --dry-run to perform the renames.")
		}
	} else {
		fmt.Printf("\nSummary: %d migrated · %d mixed (manual) · %d total\n", migrated, mixed, len(results))
	}
	if mixed > 0 {
		fmt.Println("Mixed workspaces left untouched. Inspect them by hand and pick a winner.")
	}
	return nil
}

// resolveWorkspacesRoot picks the right path: explicit flag wins,
// then the env var the daemon also reads, then the documented
// default. Keeps the CLI usable without an explicit flag in
// single-node deployments.
func resolveWorkspacesRoot() string {
	if workspaceCanonRoot != "" {
		return workspaceCanonRoot
	}
	if v := os.Getenv("VORNIK_PROJECT_WORKSPACE_PATH"); v != "" {
		return v
	}
	// Same default the loader applies in config.Runtime.
	return "/var/lib/vornik/workspaces"
}
