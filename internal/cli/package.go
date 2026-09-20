package cli

// `vornikctl package install|list|uninstall` — one artifact an operator can
// hand someone, and one lifecycle to install it with.
//
// The design is https://docs.vornik.io
// design.md. Slice 1 is workflows and roles.
//
// TWO RULES DECIDE THE SHAPE OF THIS FILE.
//
// It writes the DEPLOYED tree, not the source tree. An installer that writes
// ~/vornik/configs installs nothing, because the daemon reads only the
// deployed copy — the single most repeated operator-facing defect in this
// repository's history, and a new write path is exactly where it would recur.
// The path comes from resolveConfigsDir, the same helper every other deployed-
// tree writer uses, rather than being rebuilt here.
//
// And provenance is recorded PER CONTRIBUTED ROW, WITH THE CONTENT HASH AT
// INSTALL TIME. Uninstall is the hard half, and the hash is what makes its
// refusal enforceable rather than merely asserted.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentpackage"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

var (
	packageConfigsDir string
	packageDryRun     bool

	packageCmd = &cobra.Command{
		Use:   "package",
		Short: "Install, list and uninstall Vornik extension packages",
		Long: `A package is a manifest plus a payload tree — workflows and roles in
this release — that an operator can hand someone and install in one step.

It is NOT a marketplace and NOT a plugin API: nothing here loads code,
and every contribution is data the daemon already knows how to validate.
Trust is the operator's, exactly as it is for a workflow YAML today.`,
	}

	packageInstallCmd = &cobra.Command{
		Use:   "install <dir-or-tarball>",
		Short: "Install a package into the deployed configs tree",
		Long: `Validate a package, write its contributions into the DEPLOYED configs
tree, and record provenance for each one with its content hash.

A conflict is a refusal, not a merge: two packages contributing the same
workflow id is an operator decision, not something an installer resolves
by ordering. Every conflict is reported in one pass.

The daemon picks the new config up on its next reload.`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageInstall,
	}

	packageListCmd = &cobra.Command{
		Use:   "list",
		Short: "List installed packages and what each contributed",
		RunE:  runPackageList,
	}

	packageUninstallCmd = &cobra.Command{
		Use:   "uninstall <package>",
		Short: "Remove a package's contributions",
		Long: `Remove every row a package installed, provided each is still
byte-for-byte what the package wrote.

A file the operator EDITED stops the uninstall and is named: an operator
who tuned a contributed workflow must not lose the tuning to a package
lifecycle. A file the operator DELETED is not an error — its provenance
row is simply cleared.`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageUninstall,
	}
)

func init() {
	packageCmd.PersistentFlags().StringVar(&packageConfigsDir, "configs-dir", "", "Deployed configs directory (default: VORNIK_CONFIGS_DIR or ~/.config/vornik/configs)")
	packageInstallCmd.Flags().BoolVar(&packageDryRun, "dry-run", false, "Print the would-be writes without touching disk or the provenance store")
	packageCmd.AddCommand(packageInstallCmd, packageListCmd, packageUninstallCmd)
	rootCmd.AddCommand(packageCmd)
}

// packageContext is everything the three verbs share.
type packageContext struct {
	configsDir string
	repo       persistence.PackageContributionRepository
	close      func()
}

func openPackageContext(ctx context.Context) (*packageContext, error) {
	dir := packageConfigsDir
	if dir == "" {
		dir = resolveConfigsDir("")
	}
	if dir == "" {
		return nil, fmt.Errorf("could not resolve the deployed configs directory; pass --configs-dir")
	}

	cfg, _, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if backend.Repos == nil || backend.Repos.PackageContributions == nil {
		_ = backend.Close()
		return nil, fmt.Errorf("this database backend has no package provenance store")
	}
	return &packageContext{
		configsDir: dir,
		repo:       backend.Repos.PackageContributions,
		close:      func() { _ = backend.Close() },
	}, nil
}

func runPackageInstall(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	pc, err := openPackageContext(ctx)
	if err != nil {
		return err
	}
	defer pc.close()

	src, err := agentpackage.Open(args[0])
	if err != nil {
		return err
	}
	defer src.Close()

	plan, err := agentpackage.PlanInstall(src.Manifest, agentpackage.Environment{
		ReadPayload: src.ReadPayload,
		DeployedExists: func(rel string) bool {
			_, statErr := os.Stat(filepath.Join(pc.configsDir, rel))
			return statErr == nil
		},
		ClaimedBy: func(kind agentpackage.Kind, rowID string) (string, bool) {
			owner, ok, ownerErr := pc.repo.ContributionOwner(ctx, string(kind), rowID)
			if ownerErr != nil {
				// Fail CLOSED. An unreadable provenance store cannot say
				// a row is unclaimed, and installing over one that is
				// claimed is the case this check exists to prevent.
				fmt.Fprintf(os.Stderr, "warning: provenance lookup for %s %q failed (%v); treating it as claimed\n", kind, rowID, ownerErr)
				return "unknown (provenance lookup failed)", true
			}
			return owner, ok
		},
	})
	if err != nil {
		return err
	}

	fmt.Printf("Package %s %s contributes %d row(s):\n", src.Manifest.Package, src.Manifest.Version, len(plan.Items))
	for _, it := range plan.Items {
		fmt.Printf("  %-8s %-28s → %s\n", it.Kind, it.RowID, filepath.Join(pc.configsDir, it.TargetPath))
	}
	if packageDryRun {
		fmt.Println("\n--dry-run: nothing was written.")
		return nil
	}

	// Files first, then provenance. The other order would record rows for
	// files that a failed write never produced, and the uninstall that
	// followed would refuse on files that were never there.
	written := make([]string, 0, len(plan.Items))
	for _, it := range plan.Items {
		target := filepath.Join(pc.configsDir, it.TargetPath)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return rollbackInstall(written, fmt.Errorf("create %s: %w", filepath.Dir(target), err))
		}
		// O_EXCL, not O_TRUNC: the planner already refused an existing
		// file, and this is the check that holds if something appeared
		// between the plan and the write.
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return rollbackInstall(written, fmt.Errorf("write %s: %w", target, err))
		}
		if _, err := f.Write(it.Bytes); err != nil {
			_ = f.Close()
			return rollbackInstall(written, fmt.Errorf("write %s: %w", target, err))
		}
		if err := f.Close(); err != nil {
			return rollbackInstall(written, fmt.Errorf("write %s: %w", target, err))
		}
		written = append(written, target)
	}

	rows := make([]persistence.PackageContribution, 0, len(plan.Items))
	for _, c := range plan.Contributions() {
		rows = append(rows, persistence.PackageContribution{
			Kind: string(c.Kind), RowID: c.RowID, Package: c.Package,
			PackageVersion: src.Manifest.Version, Path: c.Path,
			ContentHashAtInstall: c.ContentHashAtInstall, InstalledAt: time.Now().UTC(),
		})
	}
	if err := pc.repo.RecordContributions(ctx, rows); err != nil {
		return rollbackInstall(written, fmt.Errorf("record provenance: %w", err))
	}

	fmt.Printf("\nInstalled %s %s. Run `vornikctl config reload` (or wait for the daemon's next reload) to activate it.\n",
		src.Manifest.Package, src.Manifest.Version)
	return nil
}

// rollbackInstall removes the files an aborted install had already written.
//
// Best effort, and it SAYS SO when it fails. A half-written install with no
// provenance is the state hardest to recover from, because `uninstall` cannot
// see rows that were never recorded — so the operator gets the list either way.
func rollbackInstall(written []string, cause error) error {
	var stuck []string
	for _, p := range written {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			stuck = append(stuck, p)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%w (and these files could not be rolled back — remove them by hand: %v)", cause, stuck)
	}
	return cause
}

func runPackageList(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	pc, err := openPackageContext(ctx)
	if err != nil {
		return err
	}
	defer pc.close()

	packages, err := pc.repo.ListPackages(ctx)
	if err != nil {
		return err
	}
	if len(packages) == 0 {
		fmt.Println("No packages installed.")
		return nil
	}
	for _, p := range packages {
		fmt.Printf("%s %s — %d row(s), installed %s\n", p.Package, p.Version, p.Rows, p.InstalledAt.Format(time.RFC3339))
		rows, err := pc.repo.ContributionsByPackage(ctx, p.Package)
		if err != nil {
			return err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
		for _, r := range rows {
			fmt.Printf("    %-8s %-28s %s%s\n", r.Kind, r.RowID, r.Path, editedMarker(pc.configsDir, r))
		}
	}
	return nil
}

// editedMarker reports, in the listing, that a contributed file no longer
// matches what the package wrote — so an operator learns it before the
// uninstall refuses rather than from the refusal.
func editedMarker(configsDir string, r persistence.PackageContribution) string {
	body, err := os.ReadFile(filepath.Join(configsDir, r.Path))
	if os.IsNotExist(err) {
		return "  [deleted]"
	}
	if err != nil {
		return "  [unreadable]"
	}
	if agentpackage.ContentHash(body) != r.ContentHashAtInstall {
		return "  [edited]"
	}
	return ""
}

func runPackageUninstall(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	pc, err := openPackageContext(ctx)
	if err != nil {
		return err
	}
	defer pc.close()

	pkg := args[0]
	stored, err := pc.repo.ContributionsByPackage(ctx, pkg)
	if err != nil {
		return err
	}
	if len(stored) == 0 {
		return fmt.Errorf("no package %q is installed", pkg)
	}

	rows := make([]agentpackage.Contribution, 0, len(stored))
	for _, s := range stored {
		rows = append(rows, agentpackage.Contribution{
			Package: s.Package, Kind: agentpackage.Kind(s.Kind), RowID: s.RowID,
			Path: s.Path, ContentHashAtInstall: s.ContentHashAtInstall,
		})
	}

	plan, planErr := agentpackage.PlanUninstall(pkg, rows, func(rel string) ([]byte, bool) {
		body, readErr := os.ReadFile(filepath.Join(pc.configsDir, rel))
		if readErr != nil {
			return nil, false
		}
		return body, true
	})
	for _, msg := range plan.Messages {
		fmt.Println(" ", msg)
	}
	if planErr != nil {
		return planErr
	}

	for _, r := range plan.Remove {
		if err := os.Remove(filepath.Join(pc.configsDir, r.Path)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", r.Path, err)
		}
	}
	// Provenance last, mirroring install: a cleared row for a file still on
	// disk is an orphan nothing can find again.
	n, err := pc.repo.DeleteContributions(ctx, pkg)
	if err != nil {
		return fmt.Errorf("clear provenance for %q: %w", pkg, err)
	}

	fmt.Printf("\nUninstalled %s: %d file(s) removed, %d provenance row(s) cleared. Reload to deactivate.\n",
		pkg, len(plan.Remove), n)
	return nil
}
