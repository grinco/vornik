//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
)

// The Postgres half of migration 190 — recurring-series membership on
// project_memory_chunks (2026-09-16-retrieval-recency-design.md §6).
//
// `go test ./...` is the SQLite lane and proves nothing about this one. The
// two drivers get the column by different mechanisms entirely (a numbered
// migration here, schemaSQL plus an additive reconciler there), so a change
// that lands on one and not the other passes CI's default lane and breaks in
// production. This test is what makes the pgvector lane able to tell.
//
// Run with: make test-integration, or
// go test -tags=integration ./internal/persistence/postgres/... -run SeriesKey
func TestIntegrationMigration190_SeriesKeyColumn(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var dataType, nullable string
	var def *string
	if err := db.QueryRowContext(ctx, `
		SELECT data_type, is_nullable, column_default
		  FROM information_schema.columns
		 WHERE table_name = 'project_memory_chunks' AND column_name = 'series_key'`,
	).Scan(&dataType, &nullable, &def); err != nil {
		t.Fatalf("project_memory_chunks.series_key missing after migrations: %v", err)
	}
	if dataType != "text" {
		t.Errorf("series_key data_type = %q, want text", dataType)
	}
	// NULLABLE with no default is the design's backfill story: absent means
	// "not part of a series", which is true of every row written before this
	// migration. A sentinel default would enrol all ~34,000 of them in one
	// series whose members supersede each other.
	if nullable != "YES" {
		t.Errorf("series_key is_nullable = %q, want YES — a NOT NULL column would need a "+
			"backfill the design deliberately does not have", nullable)
	}
	if def != nil {
		t.Errorf("series_key column_default = %q, want none", *def)
	}
}

// The index, asserted by its definition rather than merely its existence.
// Key order is load-bearing: id is last so the index SERVES the row-value
// tuple comparison (n.created_at, n.id) > (c.created_at, c.id) that the
// query-time EXISTS uses to break ties between two members ingested in the
// same second. Postgres was measured pushing the whole ROW(...) > ROW(...)
// into the index condition on this shape, and a review round's proposed OR
// rewrite was declined on that measurement (design §5.3). An index that merely
// exists under the right name would let a reshaped one pass.
func TestIntegrationMigration190_SeriesIndexShape(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var indexdef string
	if err := db.QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE tablename = 'project_memory_chunks' AND indexname = 'idx_memory_chunks_series'`,
	).Scan(&indexdef); err != nil {
		t.Fatalf("idx_memory_chunks_series missing after migrations: %v", err)
	}
	normalised := strings.Join(strings.Fields(indexdef), " ")
	for _, want := range []string{
		"(project_id, series_key, created_at DESC, id DESC)",
		"WHERE (series_key IS NOT NULL)",
	} {
		if !strings.Contains(normalised, want) {
			t.Errorf("idx_memory_chunks_series is not the expected shape: missing %q in %q", want, normalised)
		}
	}
}

// Column-shape parity with SQLite, asserted by using it: a chunk carrying a
// series_key round-trips, and one without reads back NULL rather than ”.
// The two drivers' NULL semantics are what the partial index and the EXISTS
// both key on, so a divergence here is the kind that only shows up in the lane
// nobody ran.
func TestIntegrationMigration190_SeriesKeyRoundTrips(t *testing.T) {
	ctx := context.Background()
	db := newIntegrationDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const withSeries = "proj-m190-series-chunk"
	const withoutSeries = "proj-m190-plain-chunk"
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM project_memory_chunks WHERE id IN ($1, $2)`, withSeries, withoutSeries)
	})
	_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id IN ($1, $2)`, withSeries, withoutSeries)

	// source_name and content_hash are NOT NULL here and absent from the SQLite
	// slim schema's requirements — the first thing this test caught, and a
	// reminder that "the same table" is two different column sets across the
	// two drivers.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash, series_key)
		VALUES ($1, 'proj-m190', 'czech-news-2026-09-16.md', 'todays headlines', 'h-m190-a', 'czech-news'),
		       ($2, 'proj-m190', 'a-one-off-note.md',        'a one-off note',   'h-m190-b', NULL)`,
		withSeries, withoutSeries); err != nil {
		t.Fatalf("insert chunks: %v", err)
	}

	var got *string
	if err := db.QueryRowContext(ctx,
		`SELECT series_key FROM project_memory_chunks WHERE id = $1`, withSeries).Scan(&got); err != nil {
		t.Fatalf("read series_key: %v", err)
	}
	if got == nil || *got != "czech-news" {
		t.Errorf("series_key = %v, want czech-news", got)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT series_key FROM project_memory_chunks WHERE id = $1`, withoutSeries).Scan(&got); err != nil {
		t.Fatalf("read absent series_key: %v", err)
	}
	if got != nil {
		t.Errorf("series_key on a non-series chunk = %q, want NULL", *got)
	}
}
