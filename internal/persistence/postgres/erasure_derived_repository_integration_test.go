//go:build integration

package postgres

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/erasure"
)

var erasureSeedCounter uint64

// uniqueSuffix keeps each test's rows disjoint so the suite needs no truncation
// and can run alongside the other integration tests on the shared database.
func uniqueSuffix(prefix string) string {
	n := atomic.AddUint64(&erasureSeedCounter, 1)
	return fmt.Sprintf("%s_%s_%d", prefix, time.Now().UTC().Format("150405.000000000"), n)
}

// The Art 17 derived-data cascade, against real SQL.
//
// These belong in the integration lane rather than the unit lane because every
// claim being made is a claim about POSTGRES behaviour: array containment on
// source_chunks, the entity_mentions FK cascade, and the ON DELETE SET NULL on
// project_memory_quarantine that is the reason the quarantined copy survived an
// erasure in the first place. A mock would assert the statements I wrote, not
// the semantics I depend on.
//
// Production, read-only 2026-08-21: 3,795 entities with no surviving mention
// (456 PERSON, 254 VENDOR, all carrying embeddings) and 72 quarantine rows
// already detached from any chunk. Design §4.14.

// erasureFixture builds a small graph: two chunks, three entities and two
// edges, arranged so the interesting distinction is exercised — one entity is
// mentioned ONLY by the doomed chunk, one by both, and one only by the survivor.
type erasureFixture struct {
	artifact  string
	doomed    string // chunk id erased by the artifact predicate
	surviving string
	project   string
}

func seedErasureGraph(ctx context.Context, t *testing.T, db *DB) erasureFixture {
	t.Helper()
	f := erasureFixture{
		artifact:  uniqueSuffix("artifact"),
		doomed:    uniqueSuffix("chunk_doomed"),
		surviving: uniqueSuffix("chunk_survivor"),
		project:   uniqueSuffix("proj"),
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}

	exec(`INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path)
	      VALUES ($1,$2,'subject-upload.pdf','INPUT',$3)`,
		f.artifact, f.project, "/tmp/"+f.artifact)

	exec(`INSERT INTO project_memory_chunks (id, project_id, artifact_id, source_name, content, content_hash)
	      VALUES ($1,$2,$3,'doomed.txt','erased content',$4), ($5,$2,NULL,'survivor.txt','kept content',$6)`,
		f.doomed, f.project, f.artifact, uniqueSuffix("h"), f.surviving, uniqueSuffix("h"))

	for _, e := range []struct{ id, name string }{
		{"ent_only_doomed", "Only Doomed"},
		{"ent_both", "Both"},
		{"ent_only_survivor", "Only Survivor"},
	} {
		exec(`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
		      VALUES ($1,$2,'PERSON',$3)`, f.scoped(e.id), f.project, e.name+" "+f.project)
	}

	exec(`INSERT INTO entity_mentions (chunk_id, entity_id) VALUES ($1,$2),($1,$3),($4,$3),($4,$5)`,
		f.doomed, f.scoped("ent_only_doomed"), f.scoped("ent_both"),
		f.surviving, f.scoped("ent_only_survivor"))

	// edge_doomed is evidenced ONLY by the erased chunk; edge_shared is also
	// evidenced by the survivor and must keep its row with the erased id pruned.
	exec(`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
	      VALUES ($1,$2,$3,$4,'knows',$5), ($6,$2,$4,$7,'knows',$8)`,
		f.scoped("edge_doomed"), f.project, f.scoped("ent_only_doomed"), f.scoped("ent_both"),
		pq.Array([]string{f.doomed}),
		f.scoped("edge_shared"), f.scoped("ent_only_survivor"),
		pq.Array([]string{f.doomed, f.surviving}))

	exec(`INSERT INTO project_memory_quarantine
	        (id, project_id, source_artifact_id, content, content_hash, failed_gate)
	      VALUES ($1,$2,$3,'rejected text holding the subject''s data',$4,'secret_scan')`,
		f.scoped("quar"), f.project, f.artifact, uniqueSuffix("h"))

	return f
}

func (f erasureFixture) scoped(id string) string { return id + "_" + f.project }

func (f erasureFixture) count(ctx context.Context, t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The whole cascade, end to end: capture while the chunks exist, delete them,
// sweep. What must survive is as load-bearing as what must go.
func TestErasureDerivedRepository_sweepsOnlyWhatTheErasedChunksDerived(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	captured, err := repo.CaptureDerivation(ctx, f.artifact, nil)
	if err != nil {
		t.Fatalf("CaptureDerivation: %v", err)
	}
	if len(captured.ChunkIDs) != 1 || captured.ChunkIDs[0] != f.doomed {
		t.Fatalf("capture must name the chunks being erased: %v", captured.ChunkIDs)
	}
	if len(captured.EntityIDs) != 2 {
		t.Fatalf("capture must return the doomed chunk's two entities, got %v", captured.EntityIDs)
	}

	// Erase the chunks the way the service does; entity_mentions cascades.
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	counts, err := repo.DeleteOrphanedDerived(ctx, "req-int-1", captured)
	if err != nil {
		t.Fatalf("DeleteOrphanedDerived: %v", err)
	}

	// The entity only the erased chunk mentioned is gone.
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ent_only_doomed")); n != 0 {
		t.Error("an entity mentioned only by erased chunks must be deleted")
	}
	// The entity a SURVIVING chunk still mentions is another document's data.
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ent_both")); n != 1 {
		t.Error("an entity a surviving chunk still mentions must be KEPT — it is not this subject's alone")
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ent_only_survivor")); n != 1 {
		t.Error("an entity outside the captured set must not be touched")
	}
	if counts.Entities != 1 {
		t.Errorf("Entities = %d, want 1", counts.Entities)
	}

	// The edge with no evidence left is deleted, NOT quarantined: quarantine
	// keeps the predicate and both entity references derived from the subject.
	var state string
	err = db.DB.QueryRowContext(ctx,
		`SELECT lifecycle_state FROM knowledge_edges WHERE id = $1`, f.scoped("edge_doomed")).Scan(&state)
	if err == nil {
		t.Errorf("an evidence-less edge must be DELETED under Art 17, found lifecycle_state=%q", state)
	}

	// The edge a surviving chunk still evidences keeps its row, minus the
	// erased id.
	var remaining pq.StringArray
	if err := db.DB.QueryRowContext(ctx,
		`SELECT source_chunks FROM knowledge_edges WHERE id = $1`, f.scoped("edge_shared")).
		Scan(&remaining); err != nil {
		t.Fatalf("the still-evidenced edge must survive: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != f.surviving {
		t.Errorf("source_chunks must lose exactly the erased id, got %v", remaining)
	}
}

// project_memory_quarantine holds the chunk's full text, and its FK is
// ON DELETE SET NULL — so an erasure that stops at chunks leaves the most
// sensitive copy behind with the pointer nulled.
func TestErasureDerivedRepository_purgesTheQuarantinedCopy(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	n, err := repo.DeleteQuarantinedForArtifact(ctx, "req-int-2", f.artifact)
	if err != nil {
		t.Fatalf("DeleteQuarantinedForArtifact: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d quarantine rows, want 1", n)
	}
	if got := f.count(ctx, t, db,
		`SELECT count(*) FROM project_memory_quarantine WHERE id = $1`, f.scoped("quar")); got != 0 {
		t.Error("the rejected-content copy of the subject's data must be gone")
	}
}

// Edges removed by the entity FK cascade are as gone as the ones deleted
// directly, and the report is the only evidence left once the rows are.
func TestErasureDerivedRepository_countsEdgesRemovedByCascade(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	captured, err := repo.CaptureDerivation(ctx, f.artifact, nil)
	if err != nil {
		t.Fatalf("CaptureDerivation: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	counts, err := repo.DeleteOrphanedDerived(ctx, "req-int-3", captured)
	if err != nil {
		t.Fatalf("DeleteOrphanedDerived: %v", err)
	}
	// edge_doomed goes for having no evidence. Nothing cascades here — the one
	// deleted entity's only edge is the one already removed — so a count above
	// one would mean double counting, which is the failure mode the
	// count-before-delete step exists to avoid.
	if counts.Edges != 1 {
		t.Errorf("Edges = %d, want 1 (no edge may be counted twice)", counts.Edges)
	}
}

// Capture must run BEFORE the delete. Afterwards the mentions have cascaded and
// there is nothing left to compute the derivation from — this test states the
// cost of getting the order wrong rather than leaving it to a comment.
func TestErasureDerivedRepository_captureAfterDeleteFindsNothing(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	captured, err := repo.CaptureDerivation(ctx, f.artifact, nil)
	if err != nil {
		t.Fatalf("CaptureDerivation: %v", err)
	}
	if !captured.Empty() {
		t.Fatalf("expected an empty capture after the chunks are gone, got %+v", captured)
	}
	// And an empty capture sweeps nothing, rather than widening its predicate.
	counts, err := repo.DeleteOrphanedDerived(ctx, "req-int-4", captured)
	if err != nil {
		t.Fatalf("DeleteOrphanedDerived: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("an empty capture must sweep nothing, got %+v", counts)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE project_id = $1`,
		f.project); n != 3 {
		t.Errorf("no entity may be removed on an empty capture, %d of 3 remain", n)
	}
}

var _ erasure.DerivedStore = (*ErasureDerivedRepository)(nil)

// The keep-or-delete decision is re-evaluated inside the delete's own
// transaction, not read from the captured set (§4.14, review F1).
//
// Ingestion runs concurrently with erasure. If an entity captured as "this
// erasure's" gains a mention from a newly-ingested chunk before the sweep runs,
// deleting it destroys ANOTHER document's data — the failure that matters, in
// the direction that matters. This test stages exactly that window: capture,
// erase, then a new mention arrives, then sweep.
func TestErasureDerivedRepository_keepsAnEntityAConcurrentIngestMentioned(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	captured, err := repo.CaptureDerivation(ctx, f.artifact, nil)
	if err != nil {
		t.Fatalf("CaptureDerivation: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	// The concurrent ingest: a surviving chunk now mentions the entity that,
	// at capture time, only the erased chunk mentioned.
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO entity_mentions (chunk_id, entity_id) VALUES ($1,$2)`,
		f.surviving, f.scoped("ent_only_doomed")); err != nil {
		t.Fatalf("simulate concurrent ingest: %v", err)
	}

	counts, err := repo.DeleteOrphanedDerived(ctx, "req-int-5", captured)
	if err != nil {
		t.Fatalf("DeleteOrphanedDerived: %v", err)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ent_only_doomed")); n != 1 {
		t.Fatal("an entity a concurrently-ingested chunk now mentions must be KEPT — " +
			"trusting the captured set here would destroy another document's data")
	}
	if counts.Entities != 0 {
		t.Errorf("Entities = %d, want 0", counts.Entities)
	}
}

// ---------- backfill (§5.5) ----------

// The 3,795 rows already stranded before the cascade existed. The backfill is
// a different operation from an erasure and these tests hold that line: it
// takes no request id, and it must not be reachable from an erasure path.
func TestErasureDerivedRepository_backfillCountsAndPrunesStrandedEntities(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	// Strand the graph the way every pre-2026-08-21 deletion did: drop the
	// chunks, let entity_mentions cascade, and leave everything derived.
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE project_id = $1`, f.project); err != nil {
		t.Fatalf("strand the graph: %v", err)
	}

	counts, err := repo.CountOrphanedEntities(ctx, f.project)
	if err != nil {
		t.Fatalf("CountOrphanedEntities: %v", err)
	}
	total := 0
	for _, c := range counts {
		total += c.Count
	}
	if total != 3 {
		t.Fatalf("all three entities are now unmentioned, counted %d", total)
	}
	// Counting must not delete: the dry run is the operator's decision point.
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE project_id = $1`,
		f.project); n != 3 {
		t.Fatalf("the preview must be read-only, %d of 3 entities remain", n)
	}

	entities, edges, err := repo.PruneOrphanedEntities(ctx, f.project)
	if err != nil {
		t.Fatalf("PruneOrphanedEntities: %v", err)
	}
	if entities != 3 {
		t.Errorf("pruned %d entities, want 3", entities)
	}
	if edges != 2 {
		t.Errorf("reported %d cascading edges, want 2 — an edge removed by the FK "+
			"cascade is as gone as one deleted directly and the audit must say so", edges)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_edges WHERE project_id = $1`,
		f.project); n != 0 {
		t.Errorf("%d edge(s) survived their entities", n)
	}
}

// The backfill is scoped to a project when asked. An operator cleaning one
// project must not silently sweep every other one.
func TestErasureDerivedRepository_backfillRespectsProjectScope(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	mine := seedErasureGraph(ctx, t, db)
	theirs := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	for _, f := range []erasureFixture{mine, theirs} {
		if _, err := db.DB.ExecContext(ctx,
			`DELETE FROM project_memory_chunks WHERE project_id = $1`, f.project); err != nil {
			t.Fatalf("strand: %v", err)
		}
	}

	if _, _, err := repo.PruneOrphanedEntities(ctx, mine.project); err != nil {
		t.Fatalf("PruneOrphanedEntities: %v", err)
	}
	if n := mine.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE project_id = $1`,
		theirs.project); n != 3 {
		t.Errorf("another project's rows were swept: %d of 3 remain", n)
	}
}

// The backfill must never delete a MENTIONED entity, whatever else is stranded
// around it — that is the same keep-or-delete rule the erasure sweep applies.
func TestErasureDerivedRepository_backfillKeepsMentionedEntities(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	// Only the doomed chunk goes, so ent_both and ent_only_survivor keep a
	// mention through the surviving chunk.
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	entities, _, err := repo.PruneOrphanedEntities(ctx, f.project)
	if err != nil {
		t.Fatalf("PruneOrphanedEntities: %v", err)
	}
	if entities != 1 {
		t.Errorf("pruned %d, want only the single unmentioned entity", entities)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE project_id = $1`,
		f.project); n != 2 {
		t.Errorf("a mentioned entity was pruned: %d of 2 remain", n)
	}
}

// The concurrency guarantee, against a real concurrent writer.
//
// TestErasureDerivedRepository_keepsAnEntityAConcurrentIngestMentioned covers
// the easy half — a mention that COMMITTED before the sweep started. This
// covers the half that was actually broken: an ingest in flight, holding an
// uncommitted mention, that commits while the sweep is running.
//
// Measured against Postgres 2026-08-21, with only the in-transaction NOT EXISTS
// and no lock: the entity was deleted and the fresh mention cascaded away
// silently, no error. Under READ COMMITTED each statement takes its own
// snapshot, so a mention committed after the DELETE's snapshot is invisible to
// it. The SELECT ... FOR UPDATE in step 0 is what closes it — it is the one row
// mode that conflicts with the FOR KEY SHARE an inserter takes for the foreign
// key, so the sweep waits and its next snapshot sees the mention.
func TestErasureDerivedRepository_waitsForAnIngestInFlight(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	captured, err := repo.CaptureDerivation(ctx, f.artifact, nil)
	if err != nil {
		t.Fatalf("CaptureDerivation: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	// An ingest in flight: the mention exists but is NOT yet committed when the
	// sweep begins.
	ingest, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin ingest: %v", err)
	}
	if _, err := ingest.ExecContext(ctx,
		`INSERT INTO entity_mentions (chunk_id, entity_id) VALUES ($1,$2)`,
		f.surviving, f.scoped("ent_only_doomed")); err != nil {
		t.Fatalf("ingest insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := repo.DeleteOrphanedDerived(ctx, "req-int-6", captured)
		done <- err
	}()

	// The sweep must still be waiting on the lock. If it has already returned,
	// it decided the entity's fate without seeing an ingest it was obliged to
	// wait for.
	select {
	case err := <-done:
		_ = ingest.Rollback()
		t.Fatalf("the sweep finished while an ingest held an uncommitted mention "+
			"(err=%v) — it cannot have waited for the lock", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := ingest.Commit(); err != nil {
		t.Fatalf("commit ingest: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("DeleteOrphanedDerived: %v", err)
	}

	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ent_only_doomed")); n != 1 {
		t.Error("an entity mentioned by an ingest that committed during the sweep must be KEPT")
	}
	if n := f.count(ctx, t, db,
		`SELECT count(*) FROM entity_mentions WHERE entity_id = $1`,
		f.scoped("ent_only_doomed")); n != 1 {
		t.Error("the freshly-ingested mention must survive — it was cascaded away silently before the lock")
	}
}

// A missing mention row does NOT mean the entity is stranded.
//
// internal/memory/graph/pipeline.go writes a mention only when the extracted
// candidate carries a valid character span, and swallows the error if the
// insert fails. So a live entity can exist with no mention row at all, while
// still being reached through an edge whose source chunk is present.
//
// Measured read-only on production 2026-08-21: of 3,796 entities with no
// mention, 522 are still referenced by an edge citing a chunk that EXISTS. A
// mention-only predicate would have deleted all 522 — live data, under a
// command whose whole promise is that it only removes what is already stranded.
func TestErasureDerivedRepository_keepsAnEntityEvidencedOnlyByALiveEdge(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	// A span-less extraction: the entity exists and an edge from the SURVIVING
	// chunk cites it, but no mention row was ever written for it.
	spanless := f.scoped("ent_spanless")
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
		 VALUES ($1,$2,'PERSON',$3)`, spanless, f.project, "Spanless "+f.project); err != nil {
		t.Fatalf("seed span-less entity: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		 VALUES ($1,$2,$3,$4,'employs',$5)`,
		f.scoped("edge_live"), f.project, spanless, f.scoped("ent_only_survivor"),
		pq.Array([]string{f.surviving})); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}

	// The backfill must leave it alone: its source chunk is still there.
	entities, _, err := repo.PruneOrphanedEntities(ctx, f.project)
	if err != nil {
		t.Fatalf("PruneOrphanedEntities: %v", err)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		spanless); n != 1 {
		t.Fatalf("an entity evidenced by an edge citing a LIVE chunk must be kept; "+
			"the mention-only predicate would have deleted 522 such rows in production "+
			"(pruned %d)", entities)
	}

	// And once that chunk goes, it is genuinely stranded and may be pruned.
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE project_id = $1`, f.project); err != nil {
		t.Fatalf("delete chunks: %v", err)
	}
	if _, _, err := repo.PruneOrphanedEntities(ctx, f.project); err != nil {
		t.Fatalf("PruneOrphanedEntities: %v", err)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		spanless); n != 0 {
		t.Error("with no surviving chunk on either route the entity is stranded and must go")
	}
}

// The same keep rule binds the erasure sweep. An entity captured from an erased
// chunk's mentions can still be evidenced by an edge from a chunk that survives
// — the relationship stage writes edges for candidates that never got a mention
// row — and deleting it would take live evidence with it.
func TestErasureDerivedRepository_sweepKeepsAnEntityALiveEdgeEvidences(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedErasureGraph(ctx, t, db)
	repo := NewErasureDerivedRepository(db.DB)

	// ent_only_doomed is mentioned only by the doomed chunk, but an edge from
	// the SURVIVING chunk also cites it.
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		 VALUES ($1,$2,$3,$4,'employs',$5)`,
		f.scoped("edge_live"), f.project, f.scoped("ent_only_doomed"),
		f.scoped("ent_only_survivor"), pq.Array([]string{f.surviving})); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}

	captured, err := repo.CaptureDerivation(ctx, f.artifact, nil)
	if err != nil {
		t.Fatalf("CaptureDerivation: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.doomed); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	if _, err := repo.DeleteOrphanedDerived(ctx, "req-int-7", captured); err != nil {
		t.Fatalf("DeleteOrphanedDerived: %v", err)
	}

	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ent_only_doomed")); n != 1 {
		t.Error("an entity a surviving chunk still evidences through an edge must be kept")
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_edges WHERE id = $1`,
		f.scoped("edge_live")); n != 1 {
		t.Error("the live edge must survive with it")
	}
}
