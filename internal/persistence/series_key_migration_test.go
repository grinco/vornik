package persistence

import (
	"strings"
	"testing"
)

// TestMigration190_SeriesKey pins the content of the recurring-series column
// and its index (2026-09-16-retrieval-recency-design.md §6).
//
// A content test rather than a behavioural one because this package has no
// database: the behaviour lives in the two driver lanes
// (internal/persistence/sqlite/series_key_migration_test.go and the
// integration-tagged Postgres twin). What this test guards is the thing those
// two cannot see — that the SQL text says what the design says, and keeps
// saying it after someone "tidies" the index.
func TestMigration190_SeriesKey(t *testing.T) {
	m := findMigrationForTest(t, 190)
	if m.Name != "project_memory_chunks_series_key" {
		t.Errorf("name = %q", m.Name)
	}
	for _, want := range []string{
		"ALTER TABLE project_memory_chunks ADD COLUMN IF NOT EXISTS series_key TEXT",
		"CREATE INDEX IF NOT EXISTS idx_memory_chunks_series",
		"WHERE series_key IS NOT NULL",
	} {
		if !strings.Contains(m.Up, want) {
			t.Errorf("Up missing %q, got %q", want, m.Up)
		}
	}

	// The column is NULLABLE with no default, and that is load-bearing rather
	// than incidental: ~34,000 existing rows are legitimately not part of any
	// series, and a NOT NULL with a sentinel default would make every one of
	// them a member of a series named by the sentinel — which the query-time
	// EXISTS would then happily compare against itself.
	if strings.Contains(m.Up, "series_key TEXT NOT NULL") || strings.Contains(m.Up, "series_key TEXT DEFAULT") {
		t.Errorf("series_key must stay nullable with no default; absent means 'not in a series': %q", m.Up)
	}

	// The exact index shape, in order. id is the last key because the recency
	// re-rank's EXISTS compares the row-value tuple (created_at, id) so that
	// two members ingested in the same second still order deterministically;
	// an index without it filters by prefix and re-checks the tiebreak off the
	// heap. Both drivers were measured on this shape (design §5.3) and a
	// review round's proposed OR rewrite was DECLINED on that measurement.
	// Reordering or dropping a key here invalidates the measurement silently,
	// so the literal is pinned.
	const wantIndex = "(project_id, series_key, created_at DESC, id DESC)"
	if !strings.Contains(m.Up, wantIndex) {
		t.Errorf("index key order must be %s — it serves the (created_at, id) tuple comparison "+
			"the query-time EXISTS uses as its same-second tiebreak, verified by EXPLAIN on both "+
			"drivers; got %q", wantIndex, m.Up)
	}

	// Reversible, index first: Postgres refuses to drop a column an index
	// depends on without CASCADE, and a CASCADE here would be a Down that
	// removes more than its Up added.
	idxDrop := strings.Index(m.Down, "DROP INDEX IF EXISTS idx_memory_chunks_series")
	colDrop := strings.Index(m.Down, "ALTER TABLE project_memory_chunks DROP COLUMN IF EXISTS series_key")
	if idxDrop < 0 || colDrop < 0 {
		t.Fatalf("Down must drop both the index and the column, got %q", m.Down)
	}
	if idxDrop > colDrop {
		t.Errorf("Down must drop the index BEFORE the column it indexes, got %q", m.Down)
	}
}
