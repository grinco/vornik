package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// PackageContributionRepository is the SQLite half of the package provenance
// store — agent-extension-package-design §3.
type PackageContributionRepository struct{ db *sql.DB }

// NewPackageContributionRepository constructs the repository.
func NewPackageContributionRepository(db *sql.DB) *PackageContributionRepository {
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
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, row := range rows {
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
		_, err := tx.ExecContext(ctx, `
INSERT INTO package_contributions
    (kind, row_id, package, package_version, path, content_hash_at_install, installed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.Kind, row.RowID, row.Package, row.PackageVersion, row.Path, row.ContentHashAtInstall, at.UTC())
		if err != nil {
			return fmt.Errorf("record contribution %s/%s: %w", row.Kind, row.RowID, err)
		}
	}
	return tx.Commit()
}

// ContributionsByPackage returns one package's rows, ordered by path.
func (r *PackageContributionRepository) ContributionsByPackage(ctx context.Context, pkg string) ([]persistence.PackageContribution, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT kind, row_id, package, package_version, path, content_hash_at_install, installed_at
  FROM package_contributions
 WHERE package = ?
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
		`SELECT package FROM package_contributions WHERE kind = ? AND row_id = ?`, kind, rowID).Scan(&pkg)
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
		var (
			p         persistence.InstalledPackage
			installed any
		)
		// installed_at scans through `any`, not time.Time: SQLite's driver
		// infers a column's Go type from its DECLARED type, and MIN() is an
		// expression with no declared type — so the value arrives as the
		// stored string and a time.Time destination fails. Postgres has no
		// such gap, which is exactly the kind of divergence the shared
		// repotest suite exists to catch, and did.
		if err := rows.Scan(&p.Package, &p.Version, &p.Rows, &installed); err != nil {
			return nil, err
		}
		at, err := coerceSQLiteTime(installed)
		if err != nil {
			return nil, fmt.Errorf("package %q installed_at: %w", p.Package, err)
		}
		p.InstalledAt = at
		out = append(out, p)
	}
	return out, rows.Err()
}

// sqliteTimeLayouts are the shapes SQLite stores a TIMESTAMP in: what
// CURRENT_TIMESTAMP writes, and what the driver writes back for a time.Time
// bound as a parameter.
var sqliteTimeLayouts = []string{
	// What the driver writes for a time.Time bound as a parameter: Go's
	// own time.Time.String(), zone abbreviation and all.
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
}

// coerceSQLiteTime turns whatever an aggregate handed back into a time.
func coerceSQLiteTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), nil
	case nil:
		return time.Time{}, nil
	case []byte:
		return parseSQLiteTime(string(t))
	case string:
		return parseSQLiteTime(t)
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp value %T", v)
	}
}

func parseSQLiteTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

// DeleteContributions removes a package's rows.
func (r *PackageContributionRepository) DeleteContributions(ctx context.Context, pkg string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM package_contributions WHERE package = ?`, pkg)
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
