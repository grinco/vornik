package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunPackageContributionSuite pins the extension-package provenance store on
// BOTH backends — agent-extension-package-design §3.
//
// A provenance store that behaved differently on two drivers would make
// uninstall's guarantee a function of the deployment, which is the one thing
// this table exists to prevent.
func RunPackageContributionSuite(t *testing.T, repo persistence.PackageContributionRepository) {
	runPackageOwnershipCases(t, repo)
	runPackageBatchCases(t, repo)
	runPackageListingCases(t, repo)
}

// runPackageOwnershipCases covers the writes and the one-owner invariant —
// the half that protects uninstall from removing another package's file.
func runPackageOwnershipCases(t *testing.T, repo persistence.PackageContributionRepository) {
	ctx := context.Background()

	row := func(pkg, kind, id, hash string) persistence.PackageContribution {
		return persistence.PackageContribution{
			Kind: kind, RowID: id, Package: pkg, PackageVersion: "1.2.0",
			Path: kind + "s/" + id + ".md", ContentHashAtInstall: hash,
			InstalledAt: time.Now().UTC(),
		}
	}

	t.Run("a package's rows round-trip with their hashes", func(t *testing.T) {
		pkg := uniqueID("pkg")
		want := []persistence.PackageContribution{
			row(pkg, "workflow", uniqueID("triage"), "hash-a"),
			row(pkg, "role", uniqueID("lead"), "hash-b"),
		}
		wantOK(t, "RecordContributions", repo.RecordContributions(ctx, want))

		got, err := repo.ContributionsByPackage(ctx, pkg)
		wantOK(t, "ContributionsByPackage", err)
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2", len(got))
		}
		for _, g := range got {
			if g.ContentHashAtInstall == "" {
				t.Fatalf("row %+v lost its hash — provenance that cannot answer the question it exists for", g)
			}
			if g.PackageVersion != "1.2.0" || g.Path == "" {
				t.Fatalf("row %+v did not round-trip", g)
			}
		}
	})

	t.Run("one deployed row has one owner", func(t *testing.T) {
		// The planner refuses a claimed row by reading first, but two
		// concurrent installs race past that read. The primary key is the
		// enforcement point, and this is the test that says so.
		id := uniqueID("shared")
		first := uniqueID("pkg-a")
		second := uniqueID("pkg-b")
		wantOK(t, "first install", repo.RecordContributions(ctx, []persistence.PackageContribution{row(first, "workflow", id, "hash-a")}))

		if err := repo.RecordContributions(ctx, []persistence.PackageContribution{row(second, "workflow", id, "hash-b")}); err == nil {
			t.Fatal("a second package claimed a row the first owns")
		}

		owner, ok, err := repo.ContributionOwner(ctx, "workflow", id)
		wantOK(t, "ContributionOwner", err)
		if !ok || owner != first {
			t.Fatalf("owner = %q (%v), want %q — the failed install must not have taken ownership", owner, ok, first)
		}
	})

}

// runPackageBatchCases covers the all-or-nothing batch and the validation a
// provenance row must pass before it is stored.
func runPackageBatchCases(t *testing.T, repo persistence.PackageContributionRepository) {
	ctx := context.Background()

	row := func(pkg, kind, id, hash string) persistence.PackageContribution {
		return persistence.PackageContribution{
			Kind: kind, RowID: id, Package: pkg, PackageVersion: "1.2.0",
			Path: kind + "s/" + id + ".md", ContentHashAtInstall: hash,
			InstalledAt: time.Now().UTC(),
		}
	}

	t.Run("a rejected row rolls back the whole install", func(t *testing.T) {
		// A partial install leaves files on disk with provenance for only
		// some of them, and the uninstall that follows cannot clean up
		// what it cannot see.
		taken := uniqueID("taken")
		owner := uniqueID("pkg-owner")
		wantOK(t, "seed", repo.RecordContributions(ctx, []persistence.PackageContribution{row(owner, "workflow", taken, "hash-a")}))

		newcomer := uniqueID("pkg-new")
		fresh := uniqueID("fresh")
		err := repo.RecordContributions(ctx, []persistence.PackageContribution{
			row(newcomer, "role", fresh, "hash-c"),
			row(newcomer, "workflow", taken, "hash-d"),
		})
		if err == nil {
			t.Fatal("RecordContributions accepted a batch containing a claimed row")
		}
		if _, ok, _ := repo.ContributionOwner(ctx, "role", fresh); ok {
			t.Fatal("the first row of a rejected batch was left behind")
		}
	})

	t.Run("a row with no hash is refused", func(t *testing.T) {
		pkg := uniqueID("pkg-nohash")
		bad := row(pkg, "workflow", uniqueID("w"), "")
		if err := repo.RecordContributions(ctx, []persistence.PackageContribution{bad}); err == nil {
			t.Fatal("RecordContributions accepted a row with no content hash")
		}
	})

	t.Run("an unowned row reports no owner rather than an error", func(t *testing.T) {
		owner, ok, err := repo.ContributionOwner(ctx, "workflow", uniqueID("never-installed"))
		wantOK(t, "ContributionOwner", err)
		if ok || owner != "" {
			t.Fatalf("owner = %q (%v), want none", owner, ok)
		}
	})

}

// runPackageListingCases covers the read and delete surface.
func runPackageListingCases(t *testing.T, repo persistence.PackageContributionRepository) {
	ctx := context.Background()

	row := func(pkg, kind, id, hash string) persistence.PackageContribution {
		return persistence.PackageContribution{
			Kind: kind, RowID: id, Package: pkg, PackageVersion: "1.2.0",
			Path: kind + "s/" + id + ".md", ContentHashAtInstall: hash,
			InstalledAt: time.Now().UTC(),
		}
	}

	t.Run("list summarises each package once", func(t *testing.T) {
		pkg := uniqueID("pkg-list")
		wantOK(t, "seed", repo.RecordContributions(ctx, []persistence.PackageContribution{
			row(pkg, "workflow", uniqueID("w"), "hash-a"),
			row(pkg, "role", uniqueID("r"), "hash-b"),
		}))

		list, err := repo.ListPackages(ctx)
		wantOK(t, "ListPackages", err)
		var found *persistence.InstalledPackage
		for i := range list {
			if list[i].Package == pkg {
				found = &list[i]
			}
		}
		if found == nil {
			t.Fatalf("package %q missing from %d listed", pkg, len(list))
		}
		if found.Rows != 2 {
			t.Fatalf("Rows = %d, want 2", found.Rows)
		}
		if found.Version != "1.2.0" {
			t.Fatalf("Version = %q", found.Version)
		}
	})

	t.Run("delete removes a package's rows and no others", func(t *testing.T) {
		victim := uniqueID("pkg-victim")
		bystander := uniqueID("pkg-bystander")
		keptID := uniqueID("kept")
		wantOK(t, "seed victim", repo.RecordContributions(ctx, []persistence.PackageContribution{
			row(victim, "workflow", uniqueID("w"), "hash-a"),
			row(victim, "role", uniqueID("r"), "hash-b"),
		}))
		wantOK(t, "seed bystander", repo.RecordContributions(ctx, []persistence.PackageContribution{
			row(bystander, "workflow", keptID, "hash-c"),
		}))

		n, err := repo.DeleteContributions(ctx, victim)
		wantOK(t, "DeleteContributions", err)
		if n != 2 {
			t.Fatalf("deleted %d rows, want 2", n)
		}
		got, err := repo.ContributionsByPackage(ctx, victim)
		wantOK(t, "ContributionsByPackage", err)
		if len(got) != 0 {
			t.Fatalf("%d rows survived the delete", len(got))
		}
		if _, ok, _ := repo.ContributionOwner(ctx, "workflow", keptID); !ok {
			t.Fatal("deleting one package removed another package's row")
		}
	})

	t.Run("recording nothing is not an error", func(t *testing.T) {
		wantOK(t, "RecordContributions(empty)", repo.RecordContributions(ctx, nil))
	})
}
