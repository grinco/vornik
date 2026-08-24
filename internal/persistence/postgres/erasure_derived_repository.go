package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/erasure"
	"vornik.io/vornik/internal/graphsweep"
)

// ErasureDerivedRepository removes the data an erased chunk DERIVED: the
// knowledge-graph entities and edges built from it, and the pre-ingest copy
// held in project_memory_quarantine.
//
// WHY THIS IS A SEPARATE REPOSITORY. The graph repositories deliberately have
// no hard deletes — KnowledgeEntityRepository has no delete at all, and
// KnowledgeEdgeRepository can only quarantine. That is correct for every other
// caller: retention parks an edge whose evidence is gone, it does not destroy
// it. Article 17 is the single exception, so the hard deletes live here rather
// than being added to repositories the whole system reaches for, and every one
// of them takes the erasure request id and refuses without it.
//
// Design: 2026-07-29-gdpr-data-subject-rights-design.md §4.14.
type ErasureDerivedRepository struct {
	db *sql.DB
}

// NewErasureDerivedRepository takes *sql.DB rather than DBTX because
// DeleteOrphanedDerived opens its own transaction: the keep-or-delete decision
// for an entity must be evaluated against the state the delete itself observes,
// which means it owns the transaction boundary.
func NewErasureDerivedRepository(db *sql.DB) *ErasureDerivedRepository {
	return &ErasureDerivedRepository{db: db}
}

var _ erasure.DerivedStore = (*ErasureDerivedRepository)(nil)

// requireRequest is the gate on every hard delete in this file.
//
// Nothing else in the system deletes a graph row, so the authorisation is a
// required argument. A naming convention would be a hope; an argument that
// errors is a constraint.
func requireRequest(requestID, op string) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%s: refusing to hard-delete derived personal data without an "+
			"erasure request id — this is the only path in the system permitted to delete "+
			"knowledge-graph rows, and it carries the request that authorises it", op)
	}
	return nil
}

// CaptureDerivation records what the chunks about to be erased had derived.
//
// Taken BEFORE the delete because entity_mentions cascades with the chunk: once
// the chunks are gone the link between them and their entities no longer
// exists, and nothing can reconstruct which entities this erasure was
// responsible for. The chunk ids come back too — pruning an edge's
// source_chunks must remove the ids THIS request erased, not every id that
// happens not to resolve.
//
// The predicate mirrors the two ways erasure selects chunks
// (memory.Repository.DeleteByExtractedDocument and DeleteByArtifact), so the
// capture and the delete cannot drift apart on which rows they mean.
func (r *ErasureDerivedRepository) CaptureDerivation(
	ctx context.Context, artifactID string, documentIDs []string,
) (erasure.Derivation, error) {
	var out erasure.Derivation
	if strings.TrimSpace(artifactID) == "" {
		return out, fmt.Errorf("CaptureDerivation: artifact id required")
	}

	// ONE statement, so the chunk set and the entities mentioned by it come from
	// a single snapshot. Splitting it into two reads would be safe in every
	// interleaving — a mention that appears is captured, one that disappears
	// leaves an entity we then decline to delete — but "safe in every
	// interleaving I enumerated" is a weaker property than "atomic", for no
	// saving.
	//
	// The chunk predicate mirrors the two ways erasure selects chunks
	// (memory.Repository.DeleteByExtractedDocument and DeleteByArtifact), so the
	// capture and the delete cannot drift apart on which rows they mean. The
	// entity half is the same question graphsweep.CaptureEntities asks; it is
	// spelled here rather than called because keying by artifact and extracted
	// document is erasure's business, not the sweep's.
	const q = `
		WITH doomed AS (
		    SELECT id FROM project_memory_chunks
		    WHERE artifact_id = $1
		       OR derived_from_extracted_document_id = ANY($2)
		)
		SELECT
		    (SELECT COALESCE(array_agg(id), '{}') FROM doomed),
		    (SELECT COALESCE(array_agg(DISTINCT em.entity_id), '{}')
		       FROM entity_mentions em JOIN doomed d ON d.id = em.chunk_id)`

	var chunks, entities pq.StringArray
	if err := r.db.QueryRowContext(ctx, q, artifactID, pq.Array(documentIDs)).
		Scan(&chunks, &entities); err != nil {
		return out, mapDBError(err)
	}
	out.ChunkIDs = chunks
	out.EntityIDs = entities
	return out, nil
}

// DeleteOrphanedDerived removes the graph rows left without evidence.
//
// ONE TRANSACTION, and the entity decision is re-checked inside it. The capture
// narrows WHICH entities are considered; whether each one goes is decided
// against live state at delete time, because ingestion runs concurrently and
// deleting an entity that a newly-ingested chunk mentions would destroy another
// document's data. Getting this wrong fails in the worse direction, so the
// decision is never made from a stale read.
//
// Scope is the captured set throughout. A predicate like "every entity with no
// mentions" would also sweep rows this request did not derive — see §5.5, which
// makes cleaning those an operator-run backfill with its own audit.
func (r *ErasureDerivedRepository) DeleteOrphanedDerived(
	ctx context.Context, requestID string, d erasure.Derivation,
) (erasure.DerivedCounts, error) {
	var counts erasure.DerivedCounts
	if err := requireRequest(requestID, "DeleteOrphanedDerived"); err != nil {
		return counts, err
	}
	if d.Empty() {
		return counts, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("DeleteOrphanedDerived: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// The sweep itself is shared with hard eviction, which deletes the same
	// derived rows for the same reason — see internal/graphsweep. What is
	// specific to Article 17 is the authorisation above, not the SQL.
	got, err := graphsweep.Sweep(ctx, tx, d.ChunkIDs, d.EntityIDs)
	if err != nil {
		return erasure.DerivedCounts{}, fmt.Errorf("DeleteOrphanedDerived: %w — "+
			"the chunks are gone but data derived from them remains; re-run to finish", err)
	}
	counts.Entities = got.Entities
	counts.Edges = got.Edges

	if err := tx.Commit(); err != nil {
		return erasure.DerivedCounts{}, fmt.Errorf("DeleteOrphanedDerived: commit: %w", err)
	}
	return counts, nil
}

// DeleteQuarantinedForArtifact removes the pre-ingest copies held for an
// artifact.
//
// This outranks the graph rows for an erasure claim. project_memory_quarantine
// holds the chunk's full CONTENT — the text a gate rejected on ingest —
// alongside failed_gate and failure_detail, and its foreign key to
// project_memory_chunks is ON DELETE SET NULL, so erasing the chunk nulls the
// pointer and keeps the text. Content that failed an ingest gate is
// disproportionately likely to be the sensitive kind.
//
// Keyed on source_artifact_id, not on any chunk: a quarantine row exists
// precisely BECAUSE its content never became a chunk. It must therefore run
// before the artifacts row is deleted, which would SET NULL this column and
// strip the only handle onto these rows — EraseIncludingArtifact's ordering
// (derived cascade, then bytes, then the row) is what guarantees that.
func (r *ErasureDerivedRepository) DeleteQuarantinedForArtifact(
	ctx context.Context, requestID, artifactID string,
) (int, error) {
	if err := requireRequest(requestID, "DeleteQuarantinedForArtifact"); err != nil {
		return 0, err
	}
	if strings.TrimSpace(artifactID) == "" {
		return 0, fmt.Errorf("DeleteQuarantinedForArtifact: artifact id required")
	}
	return execCount(ctx, r.db, `
		DELETE FROM project_memory_quarantine WHERE source_artifact_id = $1`, artifactID)
}

// execCount runs a statement and reports rows affected, mapping driver errors.
func execCount(ctx context.Context, q DBTX, query string, args ...any) (int, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

// ---------- backfill (§5.5) ----------
//
// Everything below cleans rows that are ALREADY stranded — entities whose
// source chunks were deleted before the cascade above existed. It is separate
// from the erasure path in every way that matters, and deliberately so:
//
//   - It takes no request id, because it satisfies no request. Attributing
//     these rows to an erasure they were never part of would put a false claim
//     in the audit trail.
//   - It is an operator action with its own admin_audit row, not a migration.
//     A migration that deletes personal data leaves no decision and no audit
//     trail, and "the upgrade did it" is not an answer to a regulator.
//   - It is never called from an erasure. The erasure sweep stays keyed to what
//     that request derived; folding a global orphan hunt into it would delete
//     rows the request had nothing to do with, under the request's authority.

// OrphanedEntityCount is one entity type's share of the stranded rows.
type OrphanedEntityCount struct {
	Type  string
	Count int
	// WithEmbedding counts those carrying a vector. They are the ones still
	// reachable by semantic search, so the split is what tells an operator
	// whether this is a live exposure or dead weight.
	WithEmbedding int
}

// CountOrphanedEntities reports entities no surviving chunk evidences, grouped
// by type.
//
// Read-only, and the dry run's whole substance: the operator sees the count and
// its composition before anything is deleted. Production 2026-08-21 measured
// 3,795 — 456 PERSON and 254 VENDOR among them, all carrying embeddings.
//
// An empty projectID means every project.
func (r *ErasureDerivedRepository) CountOrphanedEntities(
	ctx context.Context, projectID string,
) ([]OrphanedEntityCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT ke.type, count(*), count(ke.embedding)
		FROM knowledge_entities ke
		WHERE ($1 = '' OR ke.project_id = $1)
		  AND NOT (`+graphsweep.StillEvidenced+`)
		GROUP BY ke.type
		ORDER BY count(*) DESC`, strings.TrimSpace(projectID))
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []OrphanedEntityCount
	for rows.Next() {
		var c OrphanedEntityCount
		if err := rows.Scan(&c.Type, &c.Count, &c.WithEmbedding); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneOrphanedEntities deletes what CountOrphanedEntities reported.
//
// The predicate is re-evaluated here rather than taking ids from the dry run:
// between a preview and an operator's decision, ingestion may have given an
// entity a mention, and deleting it on the strength of a stale read is the same
// mistake the erasure sweep refuses to make. Cascaded edges are counted before
// the delete, since afterwards there is nothing left to count.
func (r *ErasureDerivedRepository) PruneOrphanedEntities(
	ctx context.Context, projectID string,
) (entities, edges int, err error) {
	project := strings.TrimSpace(projectID)

	// Batched, because the lock this needs is the same FOR UPDATE that blocks
	// an ingest from recording a mention. The erasure sweep locks one
	// document's entities — a handful. This locks every stranded row in the
	// project, which is 3,274 on production. Holding that in one transaction
	// stalls ingestion for as long as the delete takes, so a backfill run for
	// hygiene would become an outage.
	//
	// Each batch is its own transaction and the operation is idempotent, so
	// stopping between batches is safe and partial progress is real progress.
	// Counts accumulate across batches and are returned even on failure — the
	// audit row has to record what actually happened, not what was attempted.
	for round := 0; ; round++ {
		if round > pruneMaxRounds {
			return entities, edges, fmt.Errorf("PruneOrphanedEntities: still finding "+
				"candidates after %d rounds of %d — refusing to loop further. %d entities "+
				"and %d edges were deleted; re-run to continue",
				pruneMaxRounds, pruneBatchSize, entities, edges)
		}
		found, gone, cascaded, err := r.pruneBatch(ctx, project)
		if err != nil {
			return entities, edges, err
		}
		entities += gone
		edges += cascaded
		// Loop on candidates FOUND, not on rows deleted. A batch whose every
		// candidate gained evidence between the lock and the delete removes
		// nothing while leaving other rows stranded; stopping there would end
		// the run short of the job. Those rows no longer match the predicate,
		// so the next round moves on rather than re-reading them.
		if found == 0 {
			return entities, edges, nil
		}
	}
}

const (
	// pruneBatchSize bounds how many entity rows one transaction locks.
	pruneBatchSize = 500
	// pruneMaxRounds stops a runaway loop if the predicate somehow keeps
	// matching rows the delete does not remove. A backstop, not a limit: 500 ×
	// 2000 is far beyond any real stranded population.
	pruneMaxRounds = 2000
)

// pruneBatch deletes up to pruneBatchSize stranded entities in one transaction.
//
// SKIP LOCKED rather than waiting: a candidate row another transaction holds is
// one an ingest is currently referencing, which is exactly a row this backfill
// should leave alone. Skipping it costs nothing — the operation is idempotent
// and the next run picks it up if it really is stranded — and it keeps the
// backfill from queueing behind live ingestion.
func (r *ErasureDerivedRepository) pruneBatch(
	ctx context.Context, project string,
) (found, entities, edges int, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// Lock the candidates before the predicate is evaluated for the delete, for
	// the reason spelled out in DeleteOrphanedDerived: under READ COMMITTED the
	// NOT EXISTS alone lets a concurrently-committed mention be missed, and the
	// entity is then deleted with that mention cascaded away. The delete below
	// re-evaluates against a snapshot taken after this lock, so a row that
	// gained a mention while the operator was reading the preview is kept.
	rows, err := tx.QueryContext(ctx, `
		SELECT ke.id FROM knowledge_entities ke
		WHERE ($1 = '' OR ke.project_id = $1)
		  AND NOT (`+graphsweep.StillEvidenced+`)
		ORDER BY ke.id
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, project, pruneBatchSize)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: lock candidates: %w", mapDBError(err))
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: scan candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: read candidates: %w", err)
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return 0, 0, 0, nil
	}

	locked := pq.Array(ids)

	// Count what the FK cascade will take before it takes it; afterwards there
	// is nothing left to count.
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM knowledge_edges e
		WHERE e.from_entity = ANY($1) OR e.to_entity = ANY($1)`, locked).Scan(&edges); err != nil {
		return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: count cascading edges: %w", mapDBError(err))
	}

	// The predicate is re-evaluated here, not taken from the locked set: this
	// snapshot postdates the lock, so a mention that landed in between is seen.
	entities, err = execCount(ctx, tx, `
		DELETE FROM knowledge_entities ke
		WHERE ke.id = ANY($1)
		  AND NOT (`+graphsweep.StillEvidenced+`)`, locked)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("PruneOrphanedEntities: commit: %w", err)
	}
	return len(ids), entities, edges, nil
}
