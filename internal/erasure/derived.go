package erasure

import (
	"context"
	"fmt"
	"strings"
)

// DerivedCounts reports what an erasure removed BEYOND the chunks themselves.
//
// It is part of the erasure report on purpose. Once these rows are gone there is
// nothing left to show a regulator, so the report is the audit artifact — an
// erasure that silently omitted derived rows is exactly how this defect stayed
// invisible while 3,795 entities accumulated in production.
type DerivedCounts struct {
	// Entities deleted because no surviving chunk mentions them any more.
	Entities int
	// Edges deleted because every chunk that evidenced them was erased.
	Edges int
	// Quarantined counts pre-ingest copies removed — rows in
	// project_memory_quarantine, which hold the chunk's full text.
	Quarantined int
}

// Total is every derived row removed.
func (d DerivedCounts) Total() int { return d.Entities + d.Edges + d.Quarantined }

// Derivation is what the chunks an erasure is about to delete had derived,
// captured while they still exist.
//
// ChunkIDs are carried alongside the entities because pruning an edge's
// source_chunks has to remove the ids THIS request erased. The alternative —
// dropping every id that no longer resolves — would also repair damage left by
// unrelated deletions, which is a wholesale operation wearing an erasure's
// clothes.
type Derivation struct {
	EntityIDs []string
	ChunkIDs  []string
}

// Empty reports whether the chunks derived nothing, in which case the sweep has
// no work and must not fall back to a broader predicate.
func (d Derivation) Empty() bool { return len(d.EntityIDs) == 0 && len(d.ChunkIDs) == 0 }

// DerivedStore removes the data derived from erased chunks.
//
// WHY IT EXISTS. internal/erasure deleted storage bytes, extraction rows and
// memory chunks, and touched none of the knowledge graph. A chunk delete
// cascades entity_mentions by FK and stops; knowledge_entities and
// knowledge_edges have no FK to chunks at all, and project_memory_quarantine's
// is ON DELETE SET NULL — so it keeps the chunk's full CONTENT with the pointer
// nulled. All of it survived an erasure reported as complete, and the graph rows
// are on the retrieval path: internal/memory/query_expander.go seeds expansion
// from knowledge_entities, and internal/memory/graph/searcher.go searches
// entities, edges and mentions.
//
// Every method takes the erasure request id. Nothing else in this system deletes
// graph rows, so the authorisation is a required ARGUMENT rather than a naming
// convention — a convention is a hope, an argument is a constraint.
type DerivedStore interface {
	// CaptureDerivation records what the chunks this erasure is about to remove
	// had derived, taken BEFORE the delete — afterwards the mentions have
	// cascaded and there is nothing left to compute it from. The same
	// collect-first shape DeleteByExtractedDocument already uses for the
	// embedding cache. Keyed by artifact and extracted documents because the
	// service holds both; it narrows what the sweep CONSIDERS and does not
	// decide anything's fate.
	CaptureDerivation(ctx context.Context, artifactID string, documentIDs []string) (Derivation, error)

	// DeleteOrphanedDerived deletes, in one transaction: the erased chunks'
	// ids from the source_chunks of edges they evidenced, then edges left with
	// no evidence, then captured entities that no surviving chunk mentions.
	// The mention check MUST be re-evaluated inside that transaction rather
	// than trusting the captured set — ingestion runs concurrently, and
	// deleting an entity a newly-ingested chunk mentions would destroy another
	// document's data.
	DeleteOrphanedDerived(ctx context.Context, requestID string, d Derivation) (DerivedCounts, error)

	// DeleteQuarantinedForArtifact removes the pre-ingest copies held for an
	// artifact. Keyed to the artifact, not to a chunk: a quarantine row exists
	// precisely BECAUSE its content never became a chunk.
	DeleteQuarantinedForArtifact(ctx context.Context, requestID, artifactID string) (int, error)
}

// eraseDerived removes everything derived from the erased chunks.
//
// ORDER. The caller captures entity ids before deleting chunks (mentions
// cascade with them, so the link is gone afterwards), then calls this with the
// captured set. The final keep-or-delete decision happens inside the store's
// transaction against live state.
//
// SCOPE. Keyed to what this request erased, never wholesale. A global "delete
// every entity with no mentions" would also remove rows this request did not
// derive — including entities that predate mention tracking or were created by
// the resolver without mentions. Cleaning those is an operator-run backfill with
// its own audit (`vornikctl memory prune-orphaned-entities`), not a side effect
// of one subject's request.
//
// Design: 2026-07-29-gdpr-data-subject-rights-design.md §4.14.
func eraseDerived(ctx context.Context, store DerivedStore, requestID, artifactID string,
	captured Derivation, chunksDeleted int,
) (DerivedCounts, error) {
	var counts DerivedCounts

	if store == nil {
		return counts, nil // retention callers may not wire it; Erase stays usable
	}
	if strings.TrimSpace(requestID) == "" {
		return counts, fmt.Errorf("refusing to erase derived data: no erasure request id. " +
			"These deletes remove personal data and are the only legitimate graph deletion " +
			"in the system, so they carry the request that authorised them")
	}

	// The sweep runs only when chunks were actually erased AND they derived
	// something. No chunks means nothing was derived from them; an empty
	// derivation means the sweep has no keyed scope, and running it anyway is
	// how a keyed delete degrades into the global orphan hunt this deliberately
	// is not.
	if chunksDeleted > 0 && !captured.Empty() {
		got, err := store.DeleteOrphanedDerived(ctx, requestID, captured)
		if err != nil {
			return DerivedCounts{}, fmt.Errorf("erasure: delete orphaned derived rows: %w — "+
				"the chunks are gone but data derived from them remains; re-run to finish", err)
		}
		counts.Entities += got.Entities
		counts.Edges += got.Edges
	}

	// More chunks were erased than the capture saw, so some arrived between the
	// two and their derived rows are not in the sweep's scope.
	//
	// The capture and the chunk deletes are separate transactions — the deletes
	// belong to memory.Repository, which also cleans the embedding cache — so a
	// document ingesting into the same artifact during an erasure lands in this
	// window. Narrow, and not silent: an erasure that quietly covered less than
	// it deleted is precisely the defect this file exists to fix, so it is
	// reported with the remedy rather than absorbed.
	if chunksDeleted > len(captured.ChunkIDs) {
		return counts, fmt.Errorf("erasure: %d chunk(s) were deleted but only %d were "+
			"captured before the delete, so data derived from the difference was NOT swept — "+
			"content ingested into %s while it was being erased. The chunks are gone; run "+
			"'vornikctl memory prune-orphaned-entities --project <id>' to finish, and "+
			"re-run this erasure once ingestion into the artifact is quiescent",
			chunksDeleted, len(captured.ChunkIDs), artifactID)
	}

	// Quarantined copies are keyed to the artifact and exist even when no chunk
	// was ever created, so this runs regardless of the chunk count.
	n, err := store.DeleteQuarantinedForArtifact(ctx, requestID, artifactID)
	if err != nil {
		return DerivedCounts{}, fmt.Errorf("erasure: delete quarantined copies for %s: %w — "+
			"the rejected-content copy of the subject's data remains; re-run to finish",
			artifactID, err)
	}
	counts.Quarantined += n

	return counts, nil
}
