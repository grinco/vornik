package memory

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// EvictProjectEmbeddingCache deletes the embedding_cache rows derived from one
// project's memory chunks. Returns how many keys it deleted rows for.
//
// WHY IT CANNOT BE A TABLE IN ProjectDataTables. That list drives the wipe by
// `DELETE FROM <table> WHERE project_id = $1`, and embedding_cache is keyed
// `(content_hash, model)` with no project_id column — there is nothing for a
// project-scoped DELETE to match on. So the vector computed from a project's
// text survived the project. Production held 78,176 cache rows when this was
// filed (2026-08-21).
//
// Every OTHER deletion path already evicts — DeleteByArtifact,
// DeleteByExtractedDocument, the 5c redaction transaction and HardEvict — which
// is what made the project wipe the outlier rather than the norm.
//
// CALL IT BEFORE THE CHUNKS GO. The keys are computed from source_name and
// content; afterwards there is nothing left to compute them from. It takes the
// caller's DBTX so it runs inside the wipe's transaction: the chunk and its
// vector go together or neither goes.
//
// THE SHARED-HASH CASE, stated rather than discovered. project_memory_chunks is
// UNIQUE on (project_id, content_hash), so a hash another project also holds
// means an IDENTICAL chunk there, not a collision. Deleting the shared cache row
// costs that project one re-embed on its next cache miss; its chunk row is
// untouched. That is the acceptable direction: a re-embed is cost, a retained
// vector derived from deleted content is not. The same trade is already
// documented and tested for DeleteByExtractedDocument.
func EvictProjectEmbeddingCache(ctx context.Context, tx persistence.DBTX, projectID string) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("evict project embedding cache: empty projectID")
	}

	// The shared seam, not a fifth copy: it returns the recorded embed-input
	// hash, the recomputed one for rows written before that column existed, and
	// the raw content_hash. Per-site duplication is exactly how the WRONG key
	// ended up in all four of the earlier paths (2026-08-04 review).
	hashes, err := chunkCacheKeys(ctx, tx,
		`SELECT embed_input_hash, source_name, content, content_hash FROM project_memory_chunks
		  WHERE project_id = $1`, projectID)
	if err != nil {
		return 0, fmt.Errorf("read chunk cache keys for project %s: %w", projectID, err)
	}

	evicted := 0
	for _, h := range hashes {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM embedding_cache WHERE content_hash = $1`, h)
		if err != nil {
			return evicted, fmt.Errorf("evict embedding cache for project %s: %w", projectID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			evicted += int(n)
		}
	}
	return evicted, nil
}
