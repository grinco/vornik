//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/lib/pq"
)

// Reconstructing the entity→chunk links the extractor never recorded.
//
// The pipeline wrote a mention only when the extractor returned a usable
// character span, so live entities exist with no mention row at all. That makes
// them read as stranded to every deletion path — prunable as orphans, and
// destroyable by erasing a DIFFERENT chunk that did mention them.
//
// knowledge_edges.source_chunks is what makes the repair possible: an edge
// citing a chunk that still exists is evidence that the chunk mentioned both
// endpoints, because the relationship stage builds edges only from entities
// resolved in that chunk.
//
// Measured read-only on production 2026-08-21: 529 recoverable links covering
// 523 entities.

func TestMentionBackfill_reconstructsLinksFromLiveEdges(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)
	repo := NewMentionBackfillRepository(db.DB)

	// A span-less extraction: the entity exists and an edge from the SURVIVING
	// chunk cites it, but no mention row was ever written.
	spanless := f.scoped("ent_spanless")
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
		 VALUES ($1,$2,'PERSON',$3)`, spanless, f.project, "Spanless "+f.project); err != nil {
		t.Fatalf("seed span-less entity: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		 VALUES ($1,$2,$3,$4,'employs',$5)`,
		f.scoped("edge_live"), f.project, spanless, f.scoped("ev_shared"),
		pq.Array([]string{f.surviving})); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}

	links, entities, err := repo.CountReconstructableMentions(ctx, f.project)
	if err != nil {
		t.Fatalf("CountReconstructableMentions: %v", err)
	}
	if links != 1 || entities != 1 {
		t.Fatalf("preview = %d links / %d entities, want 1 and 1", links, entities)
	}
	// The preview must not write: it is the operator's decision point.
	if n := f.count(ctx, t, db,
		`SELECT count(*) FROM entity_mentions WHERE entity_id = $1`, spanless); n != 0 {
		t.Fatal("the preview must be read-only")
	}

	inserted, err := repo.BackfillMentionsFromEdges(ctx, f.project)
	if err != nil {
		t.Fatalf("BackfillMentionsFromEdges: %v", err)
	}
	if inserted != 1 {
		t.Errorf("wrote %d links, want 1", inserted)
	}

	// The link now exists, pointing at the chunk the edge cited, with the
	// offsets absent rather than fabricated.
	var chunkID string
	var charStart int
	var charEnd, surface *string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT chunk_id, char_start, char_end::text, surface FROM entity_mentions
		 WHERE entity_id = $1`, spanless).Scan(&chunkID, &charStart, &charEnd, &surface); err != nil {
		t.Fatalf("read reconstructed mention: %v", err)
	}
	if chunkID != f.surviving {
		t.Errorf("the link must point at the chunk the edge cited, got %q", chunkID)
	}
	if charStart != 0 || charEnd != nil || surface != nil {
		t.Errorf("offsets are not reconstructable and must not be guessed: "+
			"start=%d end=%v surface=%v", charStart, charEnd, surface)
	}

	// Idempotent: a second run adds nothing.
	again, err := repo.BackfillMentionsFromEdges(ctx, f.project)
	if err != nil {
		t.Fatalf("second BackfillMentionsFromEdges: %v", err)
	}
	if again != 0 {
		t.Errorf("re-running wrote %d more rows; the repair must be idempotent", again)
	}
}

// An entity that already has a mention is left alone, and one whose only edge
// cites a DEAD chunk is not resurrected — that entity is genuinely stranded and
// belongs to the prune, not the repair.
func TestMentionBackfill_leavesMentionedAndTrulyStrandedEntitiesAlone(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)
	repo := NewMentionBackfillRepository(db.DB)

	// An entity whose only edge cites a chunk that no longer exists.
	stranded := f.scoped("ent_stranded")
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
		 VALUES ($1,$2,'PERSON',$3)`, stranded, f.project, "Stranded "+f.project); err != nil {
		t.Fatalf("seed stranded entity: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		 VALUES ($1,$2,$3,$4,'employs',$5)`,
		f.scoped("edge_dead"), f.project, stranded, f.scoped("ev_shared"),
		pq.Array([]string{"chunk-that-never-existed"})); err != nil {
		t.Fatalf("seed dead edge: %v", err)
	}

	links, _, err := repo.CountReconstructableMentions(ctx, f.project)
	if err != nil {
		t.Fatalf("CountReconstructableMentions: %v", err)
	}
	if links != 0 {
		t.Errorf("nothing is reconstructable here: ev_shared and ev_only already have "+
			"mentions, and the stranded entity's chunk is gone (got %d)", links)
	}

	if _, err := repo.BackfillMentionsFromEdges(ctx, f.project); err != nil {
		t.Fatalf("BackfillMentionsFromEdges: %v", err)
	}
	if n := f.count(ctx, t, db,
		`SELECT count(*) FROM entity_mentions WHERE entity_id = $1`, stranded); n != 0 {
		t.Error("an entity whose evidence is genuinely gone must not be given a link " +
			"to a chunk that does not exist")
	}
}

// A MERGED edge — one row whose source_chunks accumulated across several chunks
// — must repair the link for every chunk it cites, not just the first.
//
// A reviewer read the merge as evidence that the repair is unsound: source_chunks
// accumulates, so (the argument went) a cited chunk need not have mentioned both
// endpoints. It does accumulate, and the inference survives, because every id
// entering the array does so through one upsert of one chunk's own proposals,
// and validateProposals drops any proposal naming an entity that chunk did not
// resolve (pinned by TestValidateProposals_dropsEndpointsNotResolvedInThisChunk).
// Union of arrays whose members each satisfy the property preserves it.
//
// Production 2026-08-21: 4 of the 529 recoverable links come from multi-source
// edges, so restricting the repair to single-source edges would have cost almost
// nothing — and encoded a reason that was not the real one.
func TestMentionBackfill_repairsEveryChunkAMergedEdgeCites(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	f := seedEvictionGraph(ctx, t, db)
	repo := NewMentionBackfillRepository(db.DB)

	// A second surviving chunk, so the merged edge can cite two live chunks.
	second := uniqueSuffix("ev_second")
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO project_memory_chunks (id, project_id, source_name, content, content_hash)
		 VALUES ($1,$2,'second.txt','more text',$3)`,
		second, f.project, uniqueSuffix("h")); err != nil {
		t.Fatalf("seed second chunk: %v", err)
	}

	spanless := f.scoped("ent_spanless")
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_entities (id, project_id, type, canonical_name)
		 VALUES ($1,$2,'PERSON',$3)`, spanless, f.project, "Spanless "+f.project); err != nil {
		t.Fatalf("seed span-less entity: %v", err)
	}
	// One edge row, two source chunks — the shape UpsertEdge produces after the
	// same triple is extracted from both.
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO knowledge_edges (id, project_id, from_entity, to_entity, predicate, source_chunks)
		 VALUES ($1,$2,$3,$4,'employs',$5)`,
		f.scoped("edge_merged"), f.project, spanless, f.scoped("ev_shared"),
		pq.Array([]string{f.surviving, second})); err != nil {
		t.Fatalf("seed merged edge: %v", err)
	}

	links, entities, err := repo.CountReconstructableMentions(ctx, f.project)
	if err != nil {
		t.Fatalf("CountReconstructableMentions: %v", err)
	}
	if links != 2 || entities != 1 {
		t.Fatalf("a merged edge citing two live chunks yields two links for one "+
			"entity; got %d links / %d entities", links, entities)
	}

	if _, err := repo.BackfillMentionsFromEdges(ctx, f.project); err != nil {
		t.Fatalf("BackfillMentionsFromEdges: %v", err)
	}

	for _, chunkID := range []string{f.surviving, second} {
		if n := f.count(ctx, t, db,
			`SELECT count(*) FROM entity_mentions WHERE entity_id = $1 AND chunk_id = $2`,
			spanless, chunkID); n != 1 {
			t.Errorf("no link reconstructed for chunk %s; every chunk a merged edge "+
				"cites resolved both endpoints", chunkID)
		}
	}
}
