package graph

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// SQLChunkLookup is the production ChunkLookup: a thin read over
// project_memory_chunks that enforces project scope, repo_scope
// (migration-75 semantics), and the chunk-retrieval visibility
// posture (published lifecycle, non-refuted validation) so a graph
// read can never surface a chunk that the chunk-search path would
// hide.
//
// It takes a persistence.DBTX rather than the typed postgres repo so
// the graph package doesn't have to import internal/persistence/postgres
// (which would couple a domain package to a backend package). The
// production wiring passes the same *sql.DB the other repos use.
type SQLChunkLookup struct {
	db persistence.DBTX
}

// NewSQLChunkLookup builds a ChunkLookup over a DBTX handle.
func NewSQLChunkLookup(db persistence.DBTX) *SQLChunkLookup {
	return &SQLChunkLookup{db: db}
}

var _ ChunkLookup = (*SQLChunkLookup)(nil)

// LookupChunks loads chunkIDs for projectID. repoScope applies the
// same filter the chunk searcher uses (see internal/memory/searcher.go
// SearchOptions.RepoScope): empty repoScope returns every scope;
// non-empty returns chunks whose repo_scope matches OR is the
// cross-cutting '*' OR is uncategorized NULL. Lifecycle is pinned to
// published + validation_status not refuted so the graph read posture
// matches chunk retrieval. Chunks outside projectID are simply never
// selected (the project_id = $1 predicate), which is the cross-project
// guard at the SQL layer.
func (l *SQLChunkLookup) LookupChunks(ctx context.Context, projectID string, chunkIDs []string, repoScope string) ([]MentionedChunk, error) {
	if l.db == nil {
		return nil, fmt.Errorf("SQLChunkLookup: nil db")
	}
	if projectID == "" {
		return nil, fmt.Errorf("SQLChunkLookup: projectID required")
	}
	if len(chunkIDs) == 0 {
		return nil, nil
	}

	args := []any{projectID}
	placeholders := make([]string, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}

	q := `
SELECT id, project_id, source_name, content, COALESCE(repo_scope, '')
FROM project_memory_chunks
WHERE project_id = $1
  AND id IN (` + strings.Join(placeholders, ",") + `)
  AND lifecycle_state = 'published'
  AND validation_status <> 'refuted'`

	if repoScope != "" {
		args = append(args, repoScope)
		q += fmt.Sprintf(`
  AND (repo_scope = $%d OR repo_scope = '*' OR repo_scope IS NULL)`, len(args))
	}

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("SQLChunkLookup query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]MentionedChunk, 0, len(chunkIDs))
	for rows.Next() {
		var c MentionedChunk
		if err := rows.Scan(&c.ChunkID, &c.ProjectID, &c.SourceName, &c.Content, &c.RepoScope); err != nil {
			return nil, fmt.Errorf("SQLChunkLookup scan: %w", err)
		}
		// Defensive: the SQL already pins project_id, but never trust
		// a returned row's project before handing chunk content back.
		if c.ProjectID != projectID {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
