package persistence

import (
	"context"
	"time"
)

// PackageContribution is one config row an extension package installed, with
// the hash of what it wrote — agent-extension-package-design §3.
//
// The hash is the whole point. Uninstall is the hard half of the lifecycle,
// and the hash is what makes its refusal ENFORCEABLE rather than merely
// asserted: a row id alone says "this package put something here", and cannot
// say whether what is there now is still what the package put. The skill
// registry's ledger records materialised filenames and no hashes, so
// inheriting its shape would inherit a guarantee nothing can check.
type PackageContribution struct {
	// Kind is "workflow" or "role" in slice 1.
	Kind string
	// RowID is the id the contributed file registers under.
	RowID string
	// Package owns this row. One deployed file has ONE owner, which is why
	// (Kind, RowID) is the key rather than (Package, Kind, RowID).
	Package string
	// PackageVersion is what was installed, recorded so `package list` can
	// answer "which version is deployed" without re-reading the payload.
	PackageVersion string
	// Path is relative to the deployed configs dir.
	Path string
	// ContentHashAtInstall is never updated after the insert. A package
	// shipping a new version uninstalls and installs again, and an UPDATE
	// here would silently re-bless an operator's edit as the package's own
	// content — destroying the only evidence uninstall has.
	ContentHashAtInstall string
	InstalledAt          time.Time
}

// PackageContributionRepository is the provenance store for extension
// packages. ONE store, not two: a skill could arrive through `vornikctl skill
// import` or through a package and be recorded in two places that both claim
// the same deployed file, so the PACKAGE table is authoritative for anything a
// package contributed and a conflicting id is refused at install.
type PackageContributionRepository interface {
	// RecordContributions inserts a package's rows atomically. It fails if
	// any (kind, row_id) is already claimed — the same refusal the install
	// planner makes, enforced where two concurrent installs would race past
	// the planner's read.
	RecordContributions(ctx context.Context, rows []PackageContribution) error

	// ContributionsByPackage returns one package's rows, ordered by path so
	// an operator reads the same list twice.
	ContributionsByPackage(ctx context.Context, pkg string) ([]PackageContribution, error)

	// ContributionOwner returns the package that owns a (kind, row_id), if
	// any. This is the planner's conflict check.
	ContributionOwner(ctx context.Context, kind, rowID string) (string, bool, error)

	// ListPackages returns the installed packages, each with its version and
	// row count.
	ListPackages(ctx context.Context) ([]InstalledPackage, error)

	// DeleteContributions removes a package's rows. Called only after the
	// uninstall plan has been applied to disk.
	DeleteContributions(ctx context.Context, pkg string) (int64, error)
}

// InstalledPackage summarises one installed package.
type InstalledPackage struct {
	Package     string
	Version     string
	Rows        int
	InstalledAt time.Time
}
