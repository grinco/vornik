package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// PackageContributionRepository is the Postgres half of the package provenance
// store — agent-extension-package-design §3.
//
// The contract is pinned against both drivers by the repotest suite. The only
// difference below is placeholder syntax; the refusals and the ordering are
// the same statements, because a provenance store that behaved differently on
// two drivers would make uninstall's guarantee a function of the deployment.
type PackageContributionRepository struct{ db DBTX }

// NewPackageContributionRepository constructs the repository.
func NewPackageContributionRepository(db DBTX) *PackageContributionRepository {
	return &PackageContributionRepository{db: db}
}

// RecordContributions inserts a package's rows in one transaction.
//
// Plain INSERT, never upsert. An upsert would let a second install silently
// take ownership of a row another package holds, which is exactly the conflict
// the planner refuses — and the planner's read cannot bound a race between two
// concurrent installs. The primary key is the enforcement point; this is the
// door it is behind.
func (r *PackageContributionRepository) RecordContributions(ctx context.Context, rows []persistence.PackageContribution) error {
	if len(rows) == 0 {
		return nil
	}

	// ONE statement with N value tuples, not a transaction: this
	// repository holds a DBTX (which may already BE a transaction the
	// caller owns), so it has no BeginTx to reach for. A single INSERT is
	// atomic anyway — and atomicity is the property that matters here,
	// because a batch that half-applied would leave files on disk with
	// provenance for only some of them, and the uninstall that follows
	// cannot clean up what it cannot see.
	//
	// The SQLite half uses an explicit transaction for the same reason,
	// and the shared repotest suite pins that both refuse the same way.
	const cols = 7
	values := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*cols)

	for i, row := range rows {
		if row.Kind == "" || row.RowID == "" || row.Package == "" {
			return fmt.Errorf("contribution needs kind, row id and package: %+v", row)
		}
		if row.ContentHashAtInstall == "" {
			// Without it the row is provenance that cannot answer the one
			// question provenance exists for.
			return fmt.Errorf("contribution %s/%s carries no content hash", row.Kind, row.RowID)
		}
		at := row.InstalledAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		base := i * cols
		values = append(values, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7))
		args = append(args, row.Kind, row.RowID, row.Package, row.PackageVersion, row.Path, row.ContentHashAtInstall, at.UTC())
	}

	// Plain INSERT, never ON CONFLICT: an upsert would let a second
	// install silently take ownership of a row another package holds,
	// which is exactly the conflict the planner refuses — and the
	// planner's read cannot bound a race between two concurrent installs.
	// The primary key is the enforcement point; this is the door it is
	// behind.
	query := `
INSERT INTO package_contributions
    (kind, row_id, package, package_version, path, content_hash_at_install, installed_at)
VALUES ` + strings.Join(values, ", ")

	if _, err := r.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("record %d contribution(s): %w", len(rows), err)
	}
	return nil
}

// ContributionsByPackage returns one package's rows, ordered by path.
func (r *PackageContributionRepository) ContributionsByPackage(ctx context.Context, pkg string) ([]persistence.PackageContribution, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT kind, row_id, package, package_version, path, content_hash_at_install, installed_at
  FROM package_contributions
 WHERE package = $1
 ORDER BY path`, pkg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanContributions(rows)
}

// ContributionOwner returns the package that owns a (kind, row_id).
func (r *PackageContributionRepository) ContributionOwner(ctx context.Context, kind, rowID string) (string, bool, error) {
	var pkg string
	err := r.db.QueryRowContext(ctx,
		`SELECT package FROM package_contributions WHERE kind = $1 AND row_id = $2`, kind, rowID).Scan(&pkg)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return pkg, true, nil
}

// ListPackages summarises the installed packages.
func (r *PackageContributionRepository) ListPackages(ctx context.Context) ([]persistence.InstalledPackage, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT package, MIN(package_version), COUNT(*), MIN(installed_at)
  FROM package_contributions
 GROUP BY package
 ORDER BY package`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.InstalledPackage
	for rows.Next() {
		var p persistence.InstalledPackage
		if err := rows.Scan(&p.Package, &p.Version, &p.Rows, &p.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteContributions removes a package's rows.
func (r *PackageContributionRepository) DeleteContributions(ctx context.Context, pkg string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM package_contributions WHERE package = $1`, pkg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanContributions(rows *sql.Rows) ([]persistence.PackageContribution, error) {
	var out []persistence.PackageContribution
	for rows.Next() {
		var c persistence.PackageContribution
		if err := rows.Scan(&c.Kind, &c.RowID, &c.Package, &c.PackageVersion, &c.Path, &c.ContentHashAtInstall, &c.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
