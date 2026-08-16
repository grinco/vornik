package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"vornik.io/vornik/internal/datasubject"
)

// DataSubjectKGIndex is the knowledge-graph read side of the on-demand KG
// binder (GDPR design §4.2 binder 3, increment 4).
//
// WHY ITS OWN QUERIES AND NOT KnowledgeEntityRepository.List. That method
// matches `canonical_name ILIKE` only. For every other caller that is right —
// they are looking up an entity they already know the name of. Here it would be
// a coverage hole with legal weight: an entity stored as "J. Doe" with alias
// "Jane Doe" is exactly the person a request names, and missing it means an
// Art 17 erasure that reports success while her data remains. So this searches
// aliases too, and does it here rather than by widening a query eight other
// callers depend on.
type DataSubjectKGIndex struct {
	db *sql.DB
}

// NewDataSubjectKGIndex wires the index over an open postgres handle.
func NewDataSubjectKGIndex(db *sql.DB) *DataSubjectKGIndex {
	return &DataSubjectKGIndex{db: db}
}

// kgPersonType is the closed-vocabulary entity type the extractor writes for a
// person. Only PERSON entities are searched: the subject axis is about people,
// and widening to every type would propose every vendor and product whose name
// happens to contain the subject's.
const kgPersonType = "PERSON"

// FindPersonEntities returns published PERSON entities in projectID whose
// canonical name OR one of their aliases contains name.
//
// Published only, matching every other reader of this table: a superseded or
// draft entity is not what the deployment currently believes, and binding a
// subject to one would record a link the graph itself no longer stands behind.
func (r *DataSubjectKGIndex) FindPersonEntities(ctx context.Context, projectID, name string, limit int) ([]datasubject.KGEntity, error) {
	if projectID == "" || name == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, canonical_name, COALESCE(aliases, '[]'::jsonb)::text
		  FROM knowledge_entities
		 WHERE project_id = $1
		   AND type = $2
		   AND lifecycle_state = 'published'
		   AND (
		         canonical_name ILIKE $3
		         OR (jsonb_typeof(aliases) = 'array'
		             AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(aliases) AS alias
		                          WHERE alias ILIKE $3))
		       )
		 ORDER BY canonical_name ASC, id ASC
		 LIMIT $4`,
		projectID, kgPersonType, "%"+name+"%", limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []datasubject.KGEntity
	for rows.Next() {
		var e datasubject.KGEntity
		var aliasesJSON string
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.CanonicalName, &aliasesJSON); err != nil {
			return nil, err
		}
		// A malformed aliases blob costs the operator the alias display, not
		// the candidate: the entity still matched and still needs deciding.
		_ = json.Unmarshal([]byte(aliasesJSON), &e.Aliases)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MentionChunks returns the memory-chunk ids the entity is mentioned in. A
// positive limit caps the result; zero returns every distinct chunk.
//
// DISTINCT because entity_mentions carries one row per OCCURRENCE (with
// char offsets), and several mentions in one chunk are still one row of
// personal data to link.
func (r *DataSubjectKGIndex) MentionChunks(ctx context.Context, entityID string, limit int) ([]string, error) {
	if entityID == "" {
		return nil, nil
	}
	query := `
		SELECT DISTINCT chunk_id FROM entity_mentions
		 WHERE entity_id = $1
		 ORDER BY chunk_id ASC`
	args := []any{entityID}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Projects lists the projects whose graph holds PERSON entities, so a
// resolution with no --project covers every graph rather than a guess.
func (r *DataSubjectKGIndex) Projects(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT project_id FROM knowledge_entities
		 WHERE type = $1 AND lifecycle_state = 'published'
		 ORDER BY project_id ASC`, kgPersonType)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
