//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/graphsweep"
)

// Retention PARKS what it strands; erasure deletes it.
//
// Ordinary retention hard-deletes chunks whose per-class TTL elapsed —
// always-on, every cycle — and left the knowledge-graph rows built from them
// published and queryable. That is how production accumulated 3,795 entities
// whose source chunks were already gone.
//
// The terminal state here is QUARANTINE, not deletion, and the difference is
// the point: retention removed a source because its TTL elapsed, which is not a
// subject's erasure request. Parking keeps the row auditable and the decision
// reversible while removing it from every retrieval path — real, because
// KnowledgeEntityRepository.List defaults to lifecycle_state 'published' and
// SimilarByEmbedding filters on it outright.

func TestGraphSweep_retentionQuarantinesRatherThanDeletes(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	captured, err := graphsweep.CaptureEntities(ctx, tx, []string{f.evicted})
	if err != nil {
		t.Fatalf("CaptureEntities: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	parked, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The row SURVIVES — parked, not destroyed. That is what distinguishes
	// this from the Art 17 sweep, which would have deleted it.
	state := func(table, id string) string {
		t.Helper()
		var st string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT lifecycle_state FROM `+table+` WHERE id = $1`, id).Scan(&st); err != nil {
			t.Fatalf("read %s %s: %v", table, id, err)
		}
		return st
	}

	if got := state("knowledge_entities", f.scoped("ev_only")); got != "quarantined" {
		t.Errorf("an entity no surviving chunk reaches must be PARKED, got %q", got)
	}
	if got := state("knowledge_edges", f.scoped("ev_edge")); got != "quarantined" {
		t.Errorf("an edge left without evidence must be PARKED, got %q", got)
	}
	// An entity a surviving chunk still mentions is untouched: retention
	// removed one source, not the entity's reason to exist.
	if got := state("knowledge_entities", f.scoped("ev_shared")); got != "published" {
		t.Errorf("an entity a surviving chunk still mentions must stay published, got %q", got)
	}
	if parked.Edges != 1 || parked.Entities != 1 {
		t.Errorf("parked = %+v, want 1 edge and 1 entity", parked)
	}
}

// Quarantine must be reversible — that is the whole reason retention parks
// rather than deletes. UpdateLifecycle moves the row back.
func TestGraphSweep_retentionQuarantineIsReversible(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	tx, _ := db.DB.BeginTx(ctx, nil)
	captured, _ := graphsweep.CaptureEntities(ctx, tx, []string{f.evicted})
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	if _, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	repo := NewKnowledgeEntityRepository(db.DB)
	if err := repo.UpdateLifecycle(ctx, f.scoped("ev_only"), "published"); err != nil {
		t.Fatalf("UpdateLifecycle: %v", err)
	}
	var st string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT lifecycle_state FROM knowledge_entities WHERE id = $1`,
		f.scoped("ev_only")).Scan(&st); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if st != "published" {
		t.Errorf("a parked row must be restorable, got %q", st)
	}
}

// Running the same retention pass twice must not double-count. The second pass
// finds the rows already parked and reports nothing changed — a sweep that
// re-reported the same rows every six hours would make the metric meaningless.
func TestGraphSweep_retentionQuarantineIsIdempotent(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	tx, _ := db.DB.BeginTx(ctx, nil)
	captured, _ := graphsweep.CaptureEntities(ctx, tx, []string{f.evicted})
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	first, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	second, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured)
	if err != nil {
		t.Fatalf("Quarantine (second pass): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if first.Total() == 0 {
		t.Fatal("the first pass must park something")
	}
	if second.Total() != 0 {
		t.Errorf("the second pass reported %+v; already-parked rows must not be "+
			"counted again, or every retention cycle re-reports the same work", second)
	}
}

// A chunk deletion that stranded nothing must park nothing — the cascade is
// keyed to what THIS prune removed, not a project-wide tidy-up.
func TestGraphSweep_retentionParksNothingWhenEvidenceSurvives(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	// Give the edge a second source that is NOT being pruned.
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE knowledge_edges SET source_chunks = $2 WHERE id = $1`,
		f.scoped("ev_edge"), pq.Array([]string{f.evicted, f.surviving})); err != nil {
		t.Fatalf("seed second source: %v", err)
	}

	tx, _ := db.DB.BeginTx(ctx, nil)
	captured, _ := graphsweep.CaptureEntities(ctx, tx, []string{f.evicted})
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	parked, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if parked.Edges != 0 {
		t.Errorf("an edge a surviving chunk still evidences must stay published, parked %+v", parked)
	}
	var st string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT lifecycle_state FROM knowledge_edges WHERE id = $1`,
		f.scoped("ev_edge")).Scan(&st); err != nil {
		t.Fatalf("read edge: %v", err)
	}
	if st != "published" {
		t.Errorf("edge lifecycle = %q, want published", st)
	}
}

// Parking bounds VISIBILITY; this bounds RETENTION.
//
// A quarantined entity still holds canonical_name, aliases, description and an
// embedding derived from content whose retention policy already expired. Art
// 5(1)(e) storage limitation is independent of Art 17 — the chunk's TTL WAS the
// storage-limitation decision — so the parked rows cannot live for ever on the
// grounds that nobody filed an erasure request. A review put this exactly
// right: hidden from retrieval is not deleted.
func TestGraphSweep_quarantineHorizonPurgesParkedRows(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	tx, _ := db.DB.BeginTx(ctx, nil)
	captured, _ := graphsweep.CaptureEntities(ctx, tx, []string{f.evicted})
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	if _, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	alive := func(table, id string) int {
		t.Helper()
		var n int
		if err := db.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// Inside the horizon the row stays: the window is a grace period for a
	// misconfigured TTL, and it has not elapsed.
	tx2, _ := db.DB.BeginTx(ctx, nil)
	early, err := graphsweep.PurgeQuarantined(ctx, tx2, f.project, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeQuarantined (inside horizon): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if early.Total() != 0 {
		t.Errorf("nothing may be purged inside the grace window, got %+v", early)
	}
	if alive("knowledge_entities", f.scoped("ev_only")) != 1 {
		t.Fatal("the parked entity must survive inside the horizon")
	}

	// Past it, the row goes.
	tx3, _ := db.DB.BeginTx(ctx, nil)
	late, err := graphsweep.PurgeQuarantined(ctx, tx3, f.project, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeQuarantined (past horizon): %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if late.Entities != 1 {
		t.Errorf("the parked entity must be purged once its grace elapsed, got %+v", late)
	}
	if alive("knowledge_entities", f.scoped("ev_only")) != 0 {
		t.Error("derived personal data cannot outlive the policy that expired its source")
	}
	// An entity a surviving chunk still reaches was never parked and is
	// untouched — the horizon purges the parked population, not the project.
	if alive("knowledge_entities", f.scoped("ev_shared")) != 1 {
		t.Error("a published entity must not be reached by the quarantine horizon")
	}
}

// A row parked before migration 167 has no clock, and is left alone rather than
// assigned an age it never had.
func TestGraphSweep_quarantineHorizonIgnoresRowsWithNoClock(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	if _, err := db.DB.ExecContext(ctx,
		`UPDATE knowledge_entities SET lifecycle_state = 'quarantined', quarantined_at = NULL
		 WHERE id = $1`, f.scoped("ev_only")); err != nil {
		t.Fatalf("seed clockless parked row: %v", err)
	}

	tx, _ := db.DB.BeginTx(ctx, nil)
	counts, err := graphsweep.PurgeQuarantined(ctx, tx, f.project, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeQuarantined: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if counts.Entities != 0 {
		t.Errorf("a row with no quarantined_at must not be purged on a guessed age, got %+v", counts)
	}
	var n int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM knowledge_entities WHERE id = $1`, f.scoped("ev_only")).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Error("the clockless parked row must survive")
	}
}

// An entity that regained evidence while parked must not be purged on the
// strength of a decision taken days earlier.
func TestGraphSweep_quarantineHorizonKeepsAReEvidencedEntity(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	// Parked with an elapsed clock, but a live chunk mentions it again.
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE knowledge_entities
		SET lifecycle_state = 'quarantined', quarantined_at = now() - interval '90 days'
		WHERE id = $1`, f.scoped("ev_shared")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, _ := db.DB.BeginTx(ctx, nil)
	counts, err := graphsweep.PurgeQuarantined(ctx, tx, f.project, time.Now())
	if err != nil {
		t.Fatalf("PurgeQuarantined: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if counts.Entities != 0 {
		t.Errorf("an entity a surviving chunk still mentions must not be purged, got %+v", counts)
	}
}

// The purge's edge count must equal the edge rows that actually disappear.
//
// Two populations vanish: edges deleted explicitly for having no evidence, and
// edges taken by the foreign-key cascade when their entity goes. They are
// counted in separate statements, so the risk is double-counting an edge that
// is in both — which would overstate a deletion of personal data in an audit
// number. This asserts the reported count against a before/after row count
// rather than against my own arithmetic.
func TestGraphSweep_quarantinePurgeCountsEachEdgeOnce(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	// ev_only will be purged. Give it a SECOND edge that still cites the
	// surviving chunk, so one of its edges is evidence-less (explicit delete)
	// and one is not (cascade only).
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		 VALUES ($1,$2,$3,$4,'employs',$5)`,
		f.scoped("edge_cascade"), f.project, f.scoped("ev_only"), f.scoped("ev_shared"),
		pq.Array([]string{f.surviving})); err != nil {
		t.Fatalf("seed cascade edge: %v", err)
	}

	tx, _ := db.DB.BeginTx(ctx, nil)
	captured, _ := graphsweep.CaptureEntities(ctx, tx, []string{f.evicted})
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	if _, err := graphsweep.Quarantine(ctx, tx, []string{f.evicted}, captured); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Age the parking so the horizon has elapsed.
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE knowledge_entities SET quarantined_at = now() - interval '90 days'
		WHERE project_id = $1 AND lifecycle_state = 'quarantined'`, f.project); err != nil {
		t.Fatalf("age parking: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE knowledge_edges SET quarantined_at = now() - interval '90 days'
		WHERE project_id = $1 AND lifecycle_state = 'quarantined'`, f.project); err != nil {
		t.Fatalf("age parking: %v", err)
	}

	edgeCount := func() int {
		t.Helper()
		var n int
		if err := db.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM knowledge_edges WHERE project_id = $1`, f.project).Scan(&n); err != nil {
			t.Fatalf("count edges: %v", err)
		}
		return n
	}
	before := edgeCount()

	tx2, _ := db.DB.BeginTx(ctx, nil)
	counts, err := graphsweep.PurgeQuarantined(ctx, tx2, f.project, time.Now())
	if err != nil {
		t.Fatalf("PurgeQuarantined: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got, want := before-edgeCount(), counts.Edges; got != want {
		t.Errorf("%d edge rows disappeared but the purge reported %d — the explicit "+
			"delete and the FK-cascade count must not overlap", got, want)
	}
	if counts.Edges == 0 {
		t.Error("the fixture should have removed at least one edge; the assertion above " +
			"would pass vacuously otherwise")
	}
}

// The exact double-count shape a review named: a PARKED edge between TWO
// parked entities, so it is both an explicit delete target (no evidence) and a
// cascade target (both endpoints going).
//
// It cannot be counted twice, and the reason is ordering rather than a
// predicate: the explicit delete runs FIRST, so by the time the cascade count
// runs the row no longer exists to be counted. Asserted against a before/after
// row count rather than against that argument.
func TestGraphSweep_quarantinePurgeCountsAParkedEdgeBetweenTwoParkedEntitiesOnce(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	// Park both endpoints of ev_edge with an elapsed clock, and park the edge
	// itself with no evidence.
	for _, id := range []string{f.scoped("ev_only"), f.scoped("ev_shared")} {
		if _, err := db.DB.ExecContext(ctx, `
			UPDATE knowledge_entities
			SET lifecycle_state = 'quarantined', quarantined_at = now() - interval '90 days'
			WHERE id = $1`, id); err != nil {
			t.Fatalf("park entity %s: %v", id, err)
		}
	}
	// Remove every mention so neither is still evidenced.
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE project_id = $1`, f.project); err != nil {
		t.Fatalf("clear chunks: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE knowledge_edges
		SET lifecycle_state = 'quarantined', quarantined_at = now() - interval '90 days',
		    source_chunks = '{}'
		WHERE id = $1`, f.scoped("ev_edge")); err != nil {
		t.Fatalf("park edge: %v", err)
	}

	var before int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM knowledge_edges WHERE project_id = $1`, f.project).Scan(&before); err != nil {
		t.Fatalf("count edges: %v", err)
	}

	tx, _ := db.DB.BeginTx(ctx, nil)
	counts, err := graphsweep.PurgeQuarantined(ctx, tx, f.project, time.Now())
	if err != nil {
		t.Fatalf("PurgeQuarantined: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var after int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM knowledge_edges WHERE project_id = $1`, f.project).Scan(&after); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if before-after != counts.Edges {
		t.Errorf("%d edge rows disappeared but the purge reported %d — an edge that is "+
			"both evidence-less and cascade-bound must be counted once",
			before-after, counts.Edges)
	}
	if counts.Edges == 0 {
		t.Error("the fixture must remove at least one edge or this asserts nothing")
	}
}

// Entities parked by the EXTRACTION pipeline, not by retention, must not be
// swept by the retention horizon.
//
// The resolver parks ambiguous candidates as 'quarantined' at insert time, for
// operator review — production holds 4,731 of them. They carry no
// quarantined_at, and the purge's NULL guard is what keeps them out. A review
// suggested backfilling quarantined_at from updated_at for "clockless" rows;
// that would have hard-deleted this entire population, which was never
// retention-parked and means something completely different.
func TestGraphSweep_quarantineHorizonSparesAmbiguousExtractionEntities(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	ambiguous := f.scoped("ent_ambiguous")
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO knowledge_entities (id, project_id, type, canonical_name, lifecycle_state, created_at, updated_at)
		VALUES ($1,$2,'PERSON',$3,'quarantined', now() - interval '400 days', now() - interval '400 days')`,
		ambiguous, f.project, "Ambiguous "+f.project); err != nil {
		t.Fatalf("seed ambiguous entity: %v", err)
	}
	// Nothing evidences it, and it is far older than any horizon — the only
	// thing keeping it alive is the absence of a quarantined_at stamp.
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE project_id = $1`, f.project); err != nil {
		t.Fatalf("clear chunks: %v", err)
	}

	tx, _ := db.DB.BeginTx(ctx, nil)
	counts, err := graphsweep.PurgeQuarantined(ctx, tx, f.project, time.Now())
	if err != nil {
		t.Fatalf("PurgeQuarantined: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	_ = counts

	var n int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM knowledge_entities WHERE id = $1`, ambiguous).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Error("an entity parked by the extraction pipeline for operator review must " +
			"never be swept by the RETENTION horizon — it was not retention-parked, and " +
			"production holds 4,731 of them")
	}
}
