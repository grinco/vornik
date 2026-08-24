package persistence

import "testing"

// The eviction tombstones must be deleted BEFORE their run headers.
//
// memory_eviction_audit.run_id is ON DELETE RESTRICT, so removing a header
// while its tombstones still reference it fails — and because the project wipe
// runs every table in one transaction, that failure aborts the whole wipe. The
// ordering is a contract, not tidiness.
//
// RESTRICT is deliberate: SET NULL would silently orphan the tombstones and
// take the derived-erasure counts with them, which is the evidence the run
// header exists to preserve. The same reasoning pins data_subjects under the
// request ledger.
func TestProjectDataTables_EvictionTombstonesPrecedeRuns(t *testing.T) {
	audit, runs := -1, -1
	for i, table := range ProjectDataTables {
		switch table {
		case "memory_eviction_audit":
			audit = i
		case "memory_eviction_runs":
			runs = i
		}
	}
	if audit < 0 {
		t.Fatal("memory_eviction_audit is not in ProjectDataTables — a project wipe would leave its tombstones")
	}
	if runs < 0 {
		t.Fatal("memory_eviction_runs is not in ProjectDataTables — a project wipe would leave orphan run headers")
	}
	if audit > runs {
		t.Errorf("memory_eviction_audit is at %d and memory_eviction_runs at %d: the tombstones "+
			"must go FIRST or the ON DELETE RESTRICT foreign key aborts the whole project wipe",
			audit, runs)
	}
}
