package postgres

import (
	"context"
	"database/sql"
	"strings"
)

// MentionBackfillRepository reconstructs entity→chunk links the extraction
// pipeline failed to record.
//
// WHY THEY ARE MISSING. internal/memory/graph/pipeline.go wrote an
// entity_mentions row only when the extractor returned a usable character span,
// and discarded the error when the insert failed. An entity whose offsets the
// model omitted or got out of range therefore had no mention row for its entire
// life. Fixed prospectively 2026-08-21; this repairs what accumulated first.
//
// WHY IT IS REPAIRABLE AT ALL. knowledge_edges.source_chunks records which
// chunks evidenced an edge, and the relationship stage builds edges only from
// entities resolved in that chunk. So an edge citing a chunk that still EXISTS
// is direct evidence that the chunk mentioned both endpoints — the link is
// reconstructed from what the pipeline did record, not invented.
//
// Measured read-only on production 2026-08-21: 529 recoverable links covering
// 523 entities, which is essentially the whole population that had no mention
// but was still reachable through a live edge.
//
// The offsets are NOT reconstructable and are not guessed: the rows land with
// char_start 0 and char_end NULL, which is exactly what the fixed pipeline
// writes when the extractor returns no span.
type MentionBackfillRepository struct {
	db *sql.DB
}

// NewMentionBackfillRepository builds the repair over a Postgres handle. It
// takes *sql.DB rather than DBTX because the insert is a single idempotent
// statement — there is no multi-step invariant to hold a transaction open for.
func NewMentionBackfillRepository(db *sql.DB) *MentionBackfillRepository {
	return &MentionBackfillRepository{db: db}
}

// reconstructable is the shared FROM/WHERE for the preview and the repair, so
// the count an operator approves cannot describe a different set than the one
// written. $1 is the project filter (” means every project).
//
// Scoped to entities with NO mention at all. An entity that has some mentions
// but is missing one for a particular chunk is a smaller, different problem: it
// still reads as live to every consumer, so repairing it changes nothing and
// would widen a targeted repair into a general rewrite of the table.
const reconstructable = `
		FROM knowledge_entities ke
		JOIN knowledge_edges e ON (e.from_entity = ke.id OR e.to_entity = ke.id)
		JOIN project_memory_chunks c ON c.id = ANY(e.source_chunks)
		WHERE ($1 = '' OR ke.project_id = $1)
		  AND NOT EXISTS (SELECT 1 FROM entity_mentions em WHERE em.entity_id = ke.id)`

// CountReconstructableMentions previews the repair: how many links would be
// written, and how many entities they would rescue.
//
// Read-only. The entity count is the number that matters to an operator — those
// are rows that currently read as stranded to every deletion path and depend on
// the edge fallback to survive.
func (r *MentionBackfillRepository) CountReconstructableMentions(
	ctx context.Context, projectID string,
) (links, entities int, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT count(*), count(DISTINCT entity_id) FROM (
		    SELECT DISTINCT c.id AS chunk_id, ke.id AS entity_id`+reconstructable+`
		) x`, strings.TrimSpace(projectID)).Scan(&links, &entities)
	if err != nil {
		return 0, 0, mapDBError(err)
	}
	return links, entities, nil
}

// BackfillMentionsFromEdges writes the reconstructed links.
//
// Purely additive: it inserts rows and deletes nothing, so unlike the
// orphaned-entity prune it cannot destroy anything if the reasoning is wrong —
// the worst case is a mention that overstates the link, which keeps an entity
// alive rather than removing one. That asymmetry is why this needs no row lock
// and no request id, only the operator's confirmation and an audit row.
//
// ON CONFLICT DO NOTHING makes it idempotent and safe to re-run.
func (r *MentionBackfillRepository) BackfillMentionsFromEdges(
	ctx context.Context, projectID string,
) (int, error) {
	return execCount(ctx, r.db, `
		INSERT INTO entity_mentions (chunk_id, entity_id, char_start, char_end, surface)
		SELECT DISTINCT c.id, ke.id, 0, NULL::int, NULL::text`+reconstructable+`
		ON CONFLICT (chunk_id, entity_id, char_start) DO NOTHING`,
		strings.TrimSpace(projectID))
}
