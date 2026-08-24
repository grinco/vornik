//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/graphsweep"
)

// Hard eviction leaves the knowledge graph behind — the same defect as Article
// 17 erasure, in the second entry point.
//
// `vornikctl memory evict` names "GDPR / privacy-driven 'forget this' requests"
// in its own help. Before 2026-08-21 it deleted the chunk, let entity_mentions
// cascade, and left the entities and edges built from that chunk queryable —
// so a privacy request was answered with a partial deletion. Worse, the
// foreign key on project_memory_quarantine.released_chunk_id is ON DELETE SET
// NULL, so the pre-ingest copy of the chunk's full text survived with its
// pointer nulled.
//
// These exercise internal/graphsweep directly against real Postgres. The
// eviction path composes the same primitive inside its own transaction, so what
// is proven here is what runs there: array containment on source_chunks, the
// mentions FK cascade, and the SET NULL that kept the rejected text.

// evictionFixture is one chunk with everything derived from it, plus a second
// chunk that must survive untouched.
type evictionFixture struct {
	project   string
	evicted   string
	surviving string
}

func seedEvictionGraph(ctx context.Context, t *testing.T, db *DB) evictionFixture {
	t.Helper()
	f := evictionFixture{
		project:   uniqueSuffix("evproj"),
		evicted:   uniqueSuffix("ev_chunk"),
		surviving: uniqueSuffix("ev_keep"),
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}

	exec(`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash)
	      VALUES ($1,$2,'evicted.txt','the text being evicted',$3),
	             ($4,$2,'kept.txt','unrelated text',$5)`,
		f.evicted, f.project, uniqueSuffix("h"), f.surviving, uniqueSuffix("h"))

	for _, id := range []string{"ev_only", "ev_shared"} {
		exec(`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
		      VALUES ($1,$2,'PERSON',$3)`, f.scoped(id), f.project, id+" "+f.project)
	}
	exec(`INSERT INTO entity_mentions (chunk_id, entity_id) VALUES ($1,$2),($1,$3),($4,$3)`,
		f.evicted, f.scoped("ev_only"), f.scoped("ev_shared"), f.surviving)

	exec(`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
	      VALUES ($1,$2,$3,$4,'knows',$5)`,
		f.scoped("ev_edge"), f.project, f.scoped("ev_only"), f.scoped("ev_shared"),
		pq.Array([]string{f.evicted}))

	// The pre-ingest copy: rejected by a gate, later released into the chunk
	// now being evicted. It holds the same text.
	exec(`INSERT INTO project_memory_quarantine
	        (id, project_id, content, content_hash, failed_gate, released_chunk_id, released_at)
	      VALUES ($1,$2,'the text being evicted',$3,'secret_scan',$4,NOW())`,
		f.scoped("ev_quar"), f.project, uniqueSuffix("h"), f.evicted)

	return f
}

func (f evictionFixture) scoped(id string) string { return id + "_" + f.project }

func (f evictionFixture) count(ctx context.Context, t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The eviction transaction, in the order HardEvict runs it: capture, purge the
// quarantined copy, delete the chunk, sweep.
func TestGraphSweep_evictionRemovesWhatTheChunkDerived(t *testing.T) {
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
	if len(captured) != 2 {
		t.Fatalf("the evicted chunk mentions two entities, captured %v", captured)
	}

	quarantined, err := graphsweep.DeleteQuarantinedForChunks(ctx, tx, []string{f.evicted})
	if err != nil {
		t.Fatalf("DeleteQuarantinedForChunks: %v", err)
	}
	if quarantined != 1 {
		t.Errorf("purged %d quarantine rows, want 1 — the FK is SET NULL, so an "+
			"eviction that stops at the chunk keeps the rejected text", quarantined)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.evicted); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	counts, err := graphsweep.Sweep(ctx, tx, []string{f.evicted}, captured)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The entity only the evicted chunk mentioned is gone.
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ev_only")); n != 0 {
		t.Error("an entity mentioned only by the evicted chunk must go with it")
	}
	// The one a surviving chunk still mentions stays — an eviction removes what
	// it evicted, not the project's graph.
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE id = $1`,
		f.scoped("ev_shared")); n != 1 {
		t.Error("an entity a surviving chunk still mentions must be KEPT")
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_edges WHERE id = $1`,
		f.scoped("ev_edge")); n != 0 {
		t.Error("an edge whose only evidence was the evicted chunk must go")
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM project_memory_quarantine WHERE id = $1`,
		f.scoped("ev_quar")); n != 0 {
		t.Error("the pre-ingest copy of the evicted text must be gone, not merely detached")
	}
	if counts.Entities != 1 {
		t.Errorf("Entities = %d, want 1", counts.Entities)
	}
	if counts.Edges != 1 {
		t.Errorf("Edges = %d, want 1", counts.Edges)
	}
	// The surviving chunk is untouched.
	if n := f.count(ctx, t, db, `SELECT count(*) FROM project_memory_chunks WHERE id = $1`,
		f.surviving); n != 1 {
		t.Error("the unrelated chunk must survive the eviction")
	}
}

// Evicting a chunk that derived nothing must not widen into a project sweep.
func TestGraphSweep_evictionWithNoDerivedRowsTouchesNothing(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)

	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The surviving chunk mentions ev_shared, which the evicted chunk also
	// mentions — so evicting the SURVIVING one must keep everything.
	captured, err := graphsweep.CaptureEntities(ctx, tx, []string{f.surviving})
	if err != nil {
		t.Fatalf("CaptureEntities: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM project_memory_chunks WHERE id = $1`, f.surviving); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}
	counts, err := graphsweep.Sweep(ctx, tx, []string{f.surviving}, captured)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if counts.Total() != 0 {
		t.Errorf("nothing lost its evidence, so nothing may be swept: %+v", counts)
	}
	if n := f.count(ctx, t, db, `SELECT count(*) FROM knowledge_entities WHERE project_id = $1`,
		f.project); n != 2 {
		t.Errorf("both entities are still mentioned by the remaining chunk, %d of 2 survive", n)
	}
}
