package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
)

// The store half of Art 17 shared-row redaction (slice 5c).
//
// Everything that must move together moves in ONE transaction: the new content, the
// recomputed hash, a nulled embedding, a re-embed enqueued, and the pre-redaction
// embedding-cache entry evicted. A partial application here is the failure mode that
// matters, because each half-state is its own leak:
//
//   - content updated, embedding not nulled → a vector search for the erased person
//     STILL matches the chunk. The text no longer names them; the semantics the index
//     searches still do. That is not erasure from the retrieval surface, which is the
//     surface that matters for a RAG store.
//   - content updated, hash not recomputed → the row misreports its own content and
//     ChunkExistsByHash starts lying.
//   - content updated, cache not evicted → embedding_cache still holds the vector
//     for the pre-redaction text, keyed by its hash.
//
// see LLD § https://docs.vornik.io §4.2, §4.3, §5

// ChunkRedactorRepository implements datasubject.ChunkRedactor over postgres.
type ChunkRedactorRepository struct {
	db *sql.DB
}

// NewChunkRedactorRepository wires the redaction store. Returns nil for a nil handle
// so a caller that has no database cannot half-construct an erasure path.
func NewChunkRedactorRepository(db *sql.DB) *ChunkRedactorRepository {
	if db == nil {
		return nil
	}
	return &ChunkRedactorRepository{db: db}
}

var _ datasubject.ChunkRedactor = (*ChunkRedactorRepository)(nil)

// LoadChunk returns the chunk's current text and hash. The hash is the version token
// the subsequent write is guarded on, which is why both come from the same read.
func (r *ChunkRedactorRepository) LoadChunk(ctx context.Context, chunkID string) (string, string, error) {
	if r == nil || r.db == nil {
		return "", "", errors.New("postgres: LoadChunk requires a database handle")
	}
	var content, hash string
	err := r.db.QueryRowContext(ctx,
		`SELECT content, content_hash FROM project_memory_chunks WHERE id = $1`, chunkID,
	).Scan(&content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("postgres: memory chunk %q not found", chunkID)
	}
	if err != nil {
		return "", "", fmt.Errorf("postgres: load memory chunk %q: %w", chunkID, err)
	}
	return content, hash, nil
}

// RedactChunk replaces the chunk's content, guarded on expectedHash.
//
// The guard is free optimistic concurrency: `WHERE id = $1 AND content_hash = $2`
// affects zero rows precisely when the chunk changed between planning and execution.
// That returns RedactionVersionChanged and writes NOTHING, rather than overwriting a
// row we no longer understand.
func (r *ChunkRedactorRepository) RedactChunk(
	ctx context.Context, chunkID, expectedHash, newContent string,
) (datasubject.RedactionResult, error) {
	var zero datasubject.RedactionResult
	if r == nil || r.db == nil {
		return zero, errors.New("postgres: RedactChunk requires a database handle")
	}
	if chunkID == "" || expectedHash == "" {
		return zero, errors.New("postgres: RedactChunk needs a chunk id and the expected content hash")
	}

	// The same hashing function the ingest path uses, so the row's hash stays
	// consistent with what ChunkExistsByHash would compute for this text.
	newHash := memory.ContentHash(newContent)

	// Idempotency (§9): re-running a redaction over already-redacted text is a
	// no-op. Detected here rather than by writing identical values, because the
	// write would also null a perfectly good embedding and evict a live cache entry
	// for no reason.
	if newHash == expectedHash {
		return datasubject.RedactionResult{Outcome: datasubject.RedactionApplied, NewHash: newHash}, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, fmt.Errorf("postgres: begin redaction tx for %s: %w", chunkID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// The PRE-redaction source_name + content, read inside the same transaction: they
	// are what the pre-redaction embed input was built from, and therefore what the
	// surviving cache key is derived from. Read before the UPDATE below overwrites the
	// content. ErrNoRows cannot happen here — the guarded UPDATE would find no row
	// either and return RedactionVersionChanged — but it is tolerated rather than
	// promoted to a failure of the redaction itself.
	var preSourceName, preContent string
	if err := tx.QueryRowContext(ctx,
		`SELECT source_name, content FROM project_memory_chunks WHERE id = $1`, chunkID,
	).Scan(&preSourceName, &preContent); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, fmt.Errorf("postgres: read pre-redaction chunk fields for %s: %w", chunkID, err)
	}

	// Collision check (§4.2). content_hash is UNIQUE per (project_id, content_hash),
	// so two chunks differing only in the erased subject's data can redact to the
	// same text. Detected BEFORE the update so the unique index never raises, and so
	// the caller gets the survivor's id — the resolution depends on the plan, which
	// this layer cannot see.
	var survivorID string
	err = tx.QueryRowContext(ctx, `
		SELECT other.id
		  FROM project_memory_chunks AS other
		  JOIN project_memory_chunks AS target ON target.project_id = other.project_id
		 WHERE target.id = $1 AND other.content_hash = $2 AND other.id <> $1
		 LIMIT 1`, chunkID, newHash).Scan(&survivorID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, fmt.Errorf("postgres: check redaction hash collision for %s: %w", chunkID, err)
	}
	if survivorID != "" {
		return datasubject.RedactionResult{
			Outcome: datasubject.RedactionCollision, SurvivorID: survivorID,
		}, nil
	}

	// The content update and the nulled embedding are ONE statement, so there is no
	// instant at which the new text carries the old vector. Every vector query
	// already guards `embedding IS NOT NULL`, so a nulled chunk is absent from
	// similarity search rather than an error — it remains reachable by full-text
	// search throughout, because tsv is a generated column and updates itself.
	res, err := tx.ExecContext(ctx, `
		UPDATE project_memory_chunks
		   SET content = $3, content_hash = $4, embedding = NULL
		 WHERE id = $1 AND content_hash = $2`,
		chunkID, expectedHash, newContent, newHash)
	if err != nil {
		return zero, fmt.Errorf("postgres: redact memory chunk %s: %w", chunkID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return zero, fmt.Errorf("postgres: rows affected redacting %s: %w", chunkID, err)
	}
	if affected == 0 {
		// The version guard fired. Nothing has been written; the transaction is
		// rolled back by the deferred call and the chunk stays deferred.
		return datasubject.RedactionResult{Outcome: datasubject.RedactionVersionChanged}, nil
	}

	// Re-embed the redacted text. Without this the chunk stays permanently absent
	// from similarity search, which would quietly degrade the OTHER subjects' data
	// that the redaction existed to preserve.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_embed_queue (chunk_id, project_id)
		SELECT id, project_id FROM project_memory_chunks WHERE id = $1
		ON CONFLICT (chunk_id) DO UPDATE SET enqueued_at = NOW()`, chunkID); err != nil {
		return zero, fmt.Errorf("postgres: enqueue re-embed for %s: %w", chunkID, err)
	}

	// Evict the PRE-redaction vector (§4.4). The vector was computed over text that
	// still contained the subject, so leaving it retains data derived from what we were
	// asked to erase. Every model's copy goes: the operator may have re-embedded under
	// several models over the deployment's life.
	//
	// Keyed by the hash of the CONTEXTUALISED embed input, not by expectedHash. The
	// pre-2026-08-04 version evicted expectedHash (the raw content_hash) and therefore
	// deleted nothing: measured on the live deployment, 0 of 500 sampled chunks had a
	// cache row under content_hash and 500 of 500 had one under the embed-input hash.
	// The pre-redaction source_name and content are read above; both keys are evicted
	// because they coincide when the contextualisation prefix is empty.
	for _, key := range []string{memory.EmbedInputHash(preSourceName, preContent), expectedHash} {
		if key == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM embedding_cache WHERE content_hash = $1`, key); err != nil {
			return zero, fmt.Errorf("postgres: evict pre-redaction embedding cache for %s: %w", chunkID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("postgres: commit redaction of %s: %w", chunkID, err)
	}
	return datasubject.RedactionResult{Outcome: datasubject.RedactionApplied, NewHash: newHash}, nil
}
