// Package graphsweep removes the knowledge-graph rows that deleted memory
// chunks leave without evidence.
//
// WHY IT IS ITS OWN PACKAGE. Deleting a chunk cascades entity_mentions by
// foreign key and stops. knowledge_entities and knowledge_edges have no foreign
// key to chunks at all, so every path that deletes a chunk has to sweep them
// explicitly or leave the derived rows behind. Three such paths exist —
// Article 17 erasure, the operator backfill, and hard eviction — and the first
// two were written independently. The keep rule was got WRONG in one of them
// (see StillEvidenced), which is the argument for one implementation rather
// than three that agree until they don't.
//
// It is a leaf: database/sql and lib/pq only, so internal/memory and
// internal/persistence/postgres can both use it without a cycle. Postgres-only
// by construction — the pruning uses array operators — which matches both
// callers, since project_memory_chunks lives in pgvector.
//
// Design of record: https://docs.vornik.io §4.14.
package graphsweep

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// StillEvidenced is the keep rule: an entity stays when a SURVIVING chunk still
// reaches it. Written against the alias `ke`, with $1 bound to an entity-id
// array where the caller scopes by id.
//
// The second route is not belt-and-braces. internal/memory/graph/pipeline.go
// writes an entity_mentions row only when the extracted candidate carries a
// valid character span, and it discards the error when the insert fails — so a
// live entity can have no mention row for its whole life. Measured read-only on
// production 2026-08-21: of 3,796 entities with no mention, 522 were still
// referenced by an edge citing a chunk that EXISTS. A mention-only predicate
// would have destroyed all 522.
//
// The inner EXISTS tests that the cited chunk is still there, so an edge whose
// source_chunks holds only dead ids does not count as evidence.
const StillEvidenced = `
		    EXISTS (SELECT 1 FROM entity_mentions em WHERE em.entity_id = ke.id)
		 OR EXISTS (
		        SELECT 1 FROM knowledge_edges e
		        WHERE (e.from_entity = ke.id OR e.to_entity = ke.id)
		          AND EXISTS (
		              SELECT 1 FROM project_memory_chunks c
		              WHERE c.id = ANY(e.source_chunks)))`

// Counts is what a sweep removed.
type Counts struct {
	// Entities deleted because no surviving chunk reaches them.
	Entities int
	// Edges deleted because the sweep emptied their evidence, PLUS those
	// removed with an entity by foreign-key cascade. Both are equally gone and
	// the report is the only place that will say so.
	Edges int
}

// Total is every graph row removed.
func (c Counts) Total() int { return c.Entities + c.Edges }

// Querier is a read surface. Both *sql.Tx and *sql.DB satisfy it, which is
// correct for CaptureEntities: a capture is a read and is safe either way.
//
// Sweep deliberately does NOT take this. It requires a *sql.Tx concretely,
// because its steps are only safe composed: a failure after the source_chunks
// prune leaves edges citing chunks that no longer exist, and a failure after
// the edge delete leaves entities stranded. Expressing that as a doc comment
// over an interface *sql.DB also satisfies would make the unsafe call compile.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CaptureEntities returns the entities mentioned by the given chunks.
//
// It MUST run before the chunks are deleted. entity_mentions cascades with the
// chunk, so afterwards the link is gone and nothing can reconstruct which
// entities a deletion was responsible for. The captured set narrows what the
// sweep CONSIDERS; it decides nothing — StillEvidenced does that, against live
// state.
func CaptureEntities(ctx context.Context, q Querier, chunkIDs []string) ([]string, error) {
	if len(chunkIDs) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT em.entity_id FROM entity_mentions em
		WHERE em.chunk_id = ANY($1)`, pq.Array(chunkIDs))
	if err != nil {
		return nil, fmt.Errorf("graphsweep: capture entities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("graphsweep: scan entity id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Sweep removes the graph rows the deleted chunks left without evidence.
//
// The *sql.Tx is required by the SIGNATURE, not by a convention: the steps are
// only safe composed, so pruning source_chunks and then failing would leave
// edges citing chunks that no longer exist. The chunks must ALREADY be deleted
// within that transaction (or before it).
//
// The lock in step 0 is load-bearing and was added after measuring its absence.
// Under READ COMMITTED each statement takes its own snapshot, so a mention
// inserted and committed by a concurrent ingest after the DELETE's snapshot is
// invisible to it — the entity is then deleted AND the fresh mention cascaded
// away, silently. Reproduced against Postgres 2026-08-21. FOR UPDATE is the one
// row mode that conflicts with the FOR KEY SHARE an inserter takes for the
// entity_mentions foreign key, so an ingest in flight either committed before
// the lock or waits behind the sweep. FOR NO KEY UPDATE does NOT conflict and
// would look like a fix while changing nothing. ORDER BY id gives two
// concurrent sweeps a consistent lock order so they queue rather than deadlock.
func Sweep(ctx context.Context, tx *sql.Tx, chunkIDs, entityIDs []string) (Counts, error) {
	var counts Counts
	if len(entityIDs) == 0 && len(chunkIDs) == 0 {
		return counts, nil
	}

	entities := pq.Array(entityIDs)
	chunks := pq.Array(chunkIDs)

	// 0. Lock the candidates before anything reads their mentions.
	if len(entityIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			SELECT id FROM knowledge_entities WHERE id = ANY($1) ORDER BY id FOR UPDATE`,
			entities); err != nil {
			return counts, fmt.Errorf("graphsweep: lock entities: %w", err)
		}
	}

	// 1. Drop the deleted chunks from the source_chunks of the edges they
	//    evidenced. Scoped by overlap to the ids THIS deletion removed —
	//    dropping every id that no longer resolves would also repair damage
	//    left by unrelated deletions, which is a wholesale operation wearing a
	//    keyed one's clothes.
	if len(chunkIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE knowledge_edges e
			SET source_chunks = ARRAY(
			    SELECT c FROM unnest(e.source_chunks) AS c
			    WHERE c <> ALL($1::text[])
			)
			WHERE e.source_chunks && $1::text[]`, chunks); err != nil {
			return counts, fmt.Errorf("graphsweep: prune source chunks: %w", err)
		}
	}

	if len(entityIDs) == 0 {
		return counts, nil
	}

	// 2. Delete edges this deletion left with no evidence at all, scoped to the
	//    captured entities so an edge that was already evidence-less for some
	//    unrelated reason is left alone rather than destroyed as a side effect.
	edgesGone, err := execCount(ctx, tx, `
		DELETE FROM knowledge_edges
		WHERE cardinality(source_chunks) = 0
		  AND (from_entity = ANY($1) OR to_entity = ANY($1))`, entities)
	if err != nil {
		return counts, fmt.Errorf("graphsweep: delete evidence-less edges: %w", err)
	}
	counts.Edges = edgesGone

	// 3. Count the edges that entity deletion will take by FK cascade, BEFORE
	//    deleting. They are as gone as the ones above and the report must say
	//    so; afterwards there is nothing left to count. The predicate is kept
	//    identical to step 4's so the count cannot describe a different set
	//    than the one removed. Step 2 already ran, so nothing is counted twice.
	var cascaded int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM knowledge_edges e
		WHERE (e.from_entity = ANY($1) OR e.to_entity = ANY($1))
		  AND EXISTS (
		    SELECT 1 FROM knowledge_entities ke
		    WHERE ke.id = ANY($1)
		      AND (ke.id = e.from_entity OR ke.id = e.to_entity)
		      AND NOT (`+StillEvidenced+`))`, entities).Scan(&cascaded); err != nil {
		return counts, fmt.Errorf("graphsweep: count cascading edges: %w", err)
	}
	counts.Edges += cascaded

	// 4. Delete captured entities no surviving chunk reaches. Evaluated here,
	//    against a snapshot taken after step 0's lock — that pairing is the
	//    guarantee, not either half alone.
	entitiesGone, err := execCount(ctx, tx, `
		DELETE FROM knowledge_entities ke
		WHERE ke.id = ANY($1)
		  AND NOT (`+StillEvidenced+`)`, entities)
	if err != nil {
		return counts, fmt.Errorf("graphsweep: delete unevidenced entities: %w", err)
	}
	counts.Entities = entitiesGone

	return counts, nil
}

// DeleteQuarantinedForChunks removes the pre-ingest copies of chunks being
// deleted.
//
// project_memory_quarantine holds the chunk's full CONTENT — text an ingest
// gate rejected — alongside failed_gate and failure_detail, and its foreign key
// to project_memory_chunks is ON DELETE SET NULL. So deleting a released chunk
// nulls the pointer and KEEPS the text, which for a deletion performed to make
// data go away is the copy that matters most: content that failed a gate is
// disproportionately likely to be the sensitive kind.
//
// The FK rule stays SET NULL — quarantine rows outlive their chunk by design,
// and a released chunk may be pruned later without destroying the record that
// something was rejected. What changes is that a deletion meant to be permanent
// removes them explicitly.
func DeleteQuarantinedForChunks(ctx context.Context, tx *sql.Tx, chunkIDs []string) (int, error) {
	if len(chunkIDs) == 0 {
		return 0, nil
	}
	n, err := execCount(ctx, tx, `
		DELETE FROM project_memory_quarantine WHERE released_chunk_id = ANY($1)`,
		pq.Array(chunkIDs))
	if err != nil {
		return 0, fmt.Errorf("graphsweep: delete quarantined copies: %w", err)
	}
	return n, nil
}

// execCount runs a statement and reports rows affected. A driver that cannot
// say how many rows it touched leaves the count unknown, and reporting a number
// there would be a guess about deleted data.
func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// QuarantineCounts is what a retention pass parked.
type QuarantineCounts struct {
	// Edges left with no surviving evidence, moved to lifecycle_state
	// 'quarantined'.
	Edges int
	// Entities no surviving chunk reaches, moved to 'quarantined'.
	Entities int
}

// Total is every derived row parked.
func (c QuarantineCounts) Total() int { return c.Edges + c.Entities }

// Quarantine is the RETENTION ending for rows whose evidence a chunk deletion
// just removed. It is not the terminal state: it starts a clock. It is the same cascade Sweep performs and a different
// ending, and the difference is the point.
//
// Retention deletes a chunk because its TTL elapsed. The graph rows built from
// it are then unevidenced, but they are not a subject's erasure request: parking
// them keeps the row auditable and the decision reversible (UpdateLifecycle
// moves it back), while removing it from every retrieval path — which is real,
// not cosmetic, because KnowledgeEntityRepository.List defaults to
// lifecycle_state 'published' and SimilarByEmbedding filters on it outright, so
// a quarantined entity stops seeding query expansion and stops matching graph
// search.
//
// PARKING BOUNDS VISIBILITY, NOT RETENTION, which is why quarantined_at is
// stamped here. A parked entity still holds canonical_name, aliases,
// description and an embedding derived from content whose retention policy has
// already expired, and Art 5(1)(e) storage limitation is independent of Art 17:
// the chunk's TTL WAS the storage-limitation decision. Keeping its derived rows
// for ever defeats the policy that deleted the chunk — hidden from retrieval is
// not deleted, since a stored row is still processing under Art 4(2). The
// retention sweeper hard-deletes rows parked longer than the configured
// horizon; the window exists so a misconfigured TTL is recoverable, and then
// the row goes.
//
// Article 17 erasure uses Sweep instead and DELETES immediately, because a
// quarantined row still holds the predicate and both entity references derived
// from the subject's data, and an erasure request does not get a grace period. The two callers of one cascade want different endings, and
// this is where that is expressed rather than one inheriting the other's.
//
// Design: memory-knowledge-graph-design.md §5.1 (the chunk-lifecycle cascade
// table) and 2026-07-29-gdpr-data-subject-rights-design.md §4.14.
func Quarantine(ctx context.Context, tx *sql.Tx, chunkIDs, entityIDs []string) (QuarantineCounts, error) {
	var counts QuarantineCounts
	if len(chunkIDs) == 0 {
		return counts, nil
	}

	chunks := pq.Array(chunkIDs)

	// 1. Drop the deleted chunks from the source_chunks of the edges they
	//    evidenced — the same keyed prune Sweep performs, for the same reason:
	//    dropping every id that no longer resolves would also repair damage
	//    from unrelated deletions.
	//
	//    This step is IRREVERSIBLE and deliberately so. The rollback design
	//    (2026-06-05 supersession-revert) set a review gate requiring
	//    source_chunks pruning to be "deferred or made reversible", because a
	//    lifecycle cascade that pruned provenance would leave restored chunks
	//    with dead graph projections. That gate does not bind here, and the
	//    reason is checkable rather than asserted: RollbackTo restores with
	//    UPDATE project_memory_chunks SET validation_status = ... WHERE
	//    project_id = $1, so it reverts supersession on rows that still EXIST
	//    and inserts nothing. No path resurrects a hard-deleted chunk, so
	//    pruning its id is exactly as irreversible as the deletion that
	//    occasioned it and creates no inconsistency. What the gate protects is
	//    the ENDING, and the ending here — the quarantine below — is
	//    reversible.
	//
	//    If a restore-by-INSERT is ever added to the rollback path, this
	//    reasoning expires and the prune must become deferred or reversible,
	//    as the gate originally required.
	if _, err := tx.ExecContext(ctx, `
		UPDATE knowledge_edges e
		SET source_chunks = ARRAY(
		    SELECT c FROM unnest(e.source_chunks) AS c
		    WHERE c <> ALL($1::text[])
		)
		WHERE e.source_chunks && $1::text[]`, chunks); err != nil {
		return counts, fmt.Errorf("graphsweep: prune source chunks: %w", err)
	}

	if len(entityIDs) == 0 {
		return counts, nil
	}
	entities := pq.Array(entityIDs)

	// 2. Park the edges this deletion left with no evidence at all. Scoped to
	//    the captured entities, and skipping rows already parked so the count
	//    reports what CHANGED rather than what matched.
	edges, err := execCount(ctx, tx, `
		UPDATE knowledge_edges
		SET lifecycle_state = 'quarantined', quarantined_at = now()
		WHERE cardinality(source_chunks) = 0
		  AND lifecycle_state <> 'quarantined'
		  AND (from_entity = ANY($1) OR to_entity = ANY($1))`, entities)
	if err != nil {
		return counts, fmt.Errorf("graphsweep: quarantine evidence-less edges: %w", err)
	}
	counts.Edges = edges

	// 3. Park the entities no surviving chunk reaches. Same keep rule as the
	//    erasure sweep — an entity a live chunk still evidences by mention or
	//    by edge stays published, because retention removed one source, not the
	//    entity's reason to exist.
	ents, err := execCount(ctx, tx, `
		UPDATE knowledge_entities ke
		SET lifecycle_state = 'quarantined', quarantined_at = now(), updated_at = now()
		WHERE ke.id = ANY($1)
		  AND ke.lifecycle_state <> 'quarantined'
		  AND NOT (`+StillEvidenced+`)`, entities)
	if err != nil {
		return counts, fmt.Errorf("graphsweep: quarantine unevidenced entities: %w", err)
	}
	counts.Entities = ents

	return counts, nil
}

// PurgeQuarantined hard-deletes graph rows parked longer than the horizon.
//
// This is what makes retention's quarantine a bounded state rather than an
// indefinite one. Parking removes a row from every retrieval path; it does not
// remove the personal data in it — canonical_name, aliases, description and an
// embedding derived from content whose retention policy already expired. Art
// 5(1)(e) storage limitation is independent of Art 17, and the chunk's TTL WAS
// the storage-limitation decision, so the derived rows cannot outlive it for
// ever on the grounds that nobody filed an erasure request.
//
// The horizon is a grace period for OPERATOR ERROR, not for the data: a
// misconfigured TTL noticed within it can be undone with UpdateLifecycle. After
// it, the row goes.
//
// Only rows with a quarantined_at stamp are eligible. A row parked before
// migration 167, or by some other path, has no clock and is left alone rather
// than assigned an age it never had — deleting on a guessed timestamp would be
// the same overreach in the other direction.
//
// Re-checks StillEvidenced before deleting. An entity that regained evidence
// while parked (a re-ingest of the same content) must not be deleted on the
// strength of a decision taken days earlier.
func PurgeQuarantined(
	ctx context.Context, tx *sql.Tx, projectID string, before time.Time,
) (Counts, error) {
	var counts Counts
	if strings.TrimSpace(projectID) == "" {
		return counts, fmt.Errorf("graphsweep: purge quarantined: project id required")
	}

	// Edges first: deleting entities cascades their edges, and counting them
	// afterwards would be impossible. These are edges parked for having no
	// evidence, whose grace has run out.
	edges, err := execCount(ctx, tx, `
		DELETE FROM knowledge_edges
		WHERE project_id = $1
		  AND lifecycle_state = 'quarantined'
		  AND quarantined_at IS NOT NULL
		  AND quarantined_at < $2
		  AND cardinality(source_chunks) = 0`, projectID, before)
	if err != nil {
		return counts, fmt.Errorf("graphsweep: purge quarantined edges: %w", err)
	}
	counts.Edges = edges

	// Then entities, re-checking that nothing re-evidenced them in the interim.
	// The FK cascade takes any remaining edges with them; those are counted
	// here so the total does not understate what left the database.
	var cascaded int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM knowledge_edges e
		WHERE e.project_id = $1
		  AND EXISTS (
		    SELECT 1 FROM knowledge_entities ke
		    WHERE (ke.id = e.from_entity OR ke.id = e.to_entity)
		      AND ke.project_id = $1
		      AND ke.lifecycle_state = 'quarantined'
		      AND ke.quarantined_at IS NOT NULL
		      AND ke.quarantined_at < $2
		      AND NOT (`+StillEvidenced+`))`, projectID, before).Scan(&cascaded); err != nil {
		return counts, fmt.Errorf("graphsweep: count cascading quarantined edges: %w", err)
	}
	counts.Edges += cascaded

	entities, err := execCount(ctx, tx, `
		DELETE FROM knowledge_entities ke
		WHERE ke.project_id = $1
		  AND ke.lifecycle_state = 'quarantined'
		  AND ke.quarantined_at IS NOT NULL
		  AND ke.quarantined_at < $2
		  AND NOT (`+StillEvidenced+`)`, projectID, before)
	if err != nil {
		return counts, fmt.Errorf("graphsweep: purge quarantined entities: %w", err)
	}
	counts.Entities = entities

	return counts, nil
}
