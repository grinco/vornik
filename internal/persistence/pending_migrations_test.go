package persistence

import (
	"sort"
	"testing"
)

// TestPendingMigrations_SortsInversions is the regression for the migration
// runner's old apply loop: it iterated DefaultMigrations in SLICE order and
// skipped any migration whose Version <= MAX(applied), so a migration that
// appeared in the slice AFTER a higher-numbered migration was silently
// dropped. DefaultMigrations has two such inversions (v26 before v25, v74
// before v73); pendingMigrations must return them, in version order, with
// nothing skipped when the applied set is empty.
func TestPendingMigrations_SortsInversions(t *testing.T) {
	got := pendingMigrations(map[int]bool{}, DefaultMigrations)

	if len(got) != len(DefaultMigrations) {
		t.Fatalf("with nothing applied, pending should return every migration: got %d, want %d (something was skipped)",
			len(got), len(DefaultMigrations))
	}

	// Strictly ascending by version — proves the inversions are resolved
	// and inter-migration dependencies are respected.
	for i := 1; i < len(got); i++ {
		if got[i].Version <= got[i-1].Version {
			t.Fatalf("pending not strictly ascending at index %d: %d after %d",
				i, got[i].Version, got[i-1].Version)
		}
	}

	// The two previously-skipped inversions must be present.
	have := make(map[int]bool, len(got))
	for _, m := range got {
		have[m.Version] = true
	}
	for _, v := range []int{25, 73} {
		if !have[v] {
			t.Errorf("previously-skipped inversion v%d must be present in pending; it was dropped by the old slice-order loop", v)
		}
	}
}

// TestPendingMigrations_ReappliesGaps: a migration whose version is BELOW the
// max applied but that was never actually recorded (a gap — e.g. v23 added
// after the DB had already migrated past it, or an inversion that left v25
// unrecorded) must be re-applied. The old "Version > MAX(applied)" check
// skipped it; the applied-set check re-applies it. Re-application is safe
// because migration DDL is IF NOT EXISTS-guarded.
func TestPendingMigrations_ReappliesGaps(t *testing.T) {
	// v1,2,24 applied; v3,4,25 pending. v25 is below the max applied (24)
	// but missing — it must still be returned (in version order).
	applied := map[int]bool{1: true, 2: true, 24: true}
	all := []Migration{
		{Version: 1, Name: "one"},
		{Version: 2, Name: "two"},
		{Version: 24, Name: "twentyfour"},
		{Version: 26, Name: "twentysix"}, // slice inversion: 26 before 25
		{Version: 25, Name: "twentyfive"},
		{Version: 3, Name: "three"},
		{Version: 4, Name: "four"},
	}

	got := pendingMigrations(applied, all)

	var versions []int
	for _, m := range got {
		versions = append(versions, m.Version)
	}
	want := []int{3, 4, 25, 26}
	if !equalInts(versions, want) {
		t.Fatalf("pending = %v, want %v (gap v25 below max-applied 24 must be re-applied; result must be version-sorted)", versions, want)
	}
}

// TestPendingMigrations_RespectsAppliedSet: anything in the applied set is
// never returned, even if it appears out of slice order.
func TestPendingMigrations_RespectsAppliedSet(t *testing.T) {
	applied := map[int]bool{2: true, 3: true, 25: true}
	all := []Migration{
		{Version: 1, Name: "one"},
		{Version: 26, Name: "twentysix"},
		{Version: 25, Name: "twentyfive"},
		{Version: 3, Name: "three"},
		{Version: 2, Name: "two"},
	}
	got := pendingMigrations(applied, all)

	var versions []int
	for _, m := range got {
		versions = append(versions, m.Version)
	}
	want := []int{1, 26}
	if !equalInts(versions, want) {
		t.Fatalf("pending = %v, want %v (applied {2,3,25} must be excluded; result version-sorted)", versions, want)
	}
}

// TestDefaultMigrations_NoUnexpectedInversion documents the two known
// inversions so a future re-order either fixes them or updates this list.
// pendingMigrations makes them harmless, but a sorted slice is still
// easier to reason about.
func TestDefaultMigrations_KnownInversionsShape(t *testing.T) {
	type inv struct{ lo, hi int }
	var invs []inv
	highest := 0
	for _, m := range DefaultMigrations {
		if m.Version < highest {
			invs = append(invs, inv{lo: m.Version, hi: highest})
		}
		if m.Version > highest {
			highest = m.Version
		}
	}
	// Known inversions as of this commit. If you reorder DefaultMigrations
	// to fix one, drop it here; pendingMigrations keeps either way safe.
	want := []inv{{25, 26}, {73, 74}}
	if !sort.SliceIsSorted(invs, func(i, j int) bool { return invs[i].lo < invs[j].lo }) {
		t.Fatalf("inversions not sorted by lo: %v", invs)
	}
	if len(invs) != len(want) {
		t.Fatalf("inversions = %v, want %v (if you reordered DefaultMigrations, update this list)", invs, want)
	}
	for i := range want {
		if invs[i] != want[i] {
			t.Fatalf("inversions = %v, want %v (if you reordered DefaultMigrations, update this list)", invs, want)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
