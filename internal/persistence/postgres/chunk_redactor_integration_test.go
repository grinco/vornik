//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
)

// Slice 5c store half, against real postgres because every claim here is
// transactional and a fake cannot demonstrate any of them.
//
// Design: https://docs.vornik.io §4.2, §4.3

const redactTestProject = "redact-slice-5c-test"

type seededChunk struct {
	id      string
	content string
	hash    string
}

func seedChunk(t *testing.T, db *DB, id, content string) seededChunk {
	t.Helper()
	ctx := context.Background()
	hash := memory.ContentHash(content)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO project_memory_chunks (id, project_id, content, content_hash, source_name)
		VALUES ($1, $2, $3, $4, 'integration-test')`,
		id, redactTestProject, content, hash); err != nil {
		t.Fatalf("seed chunk %s: %v", id, err)
	}
	return seededChunk{id: id, content: content, hash: hash}
}

// setEmbedding gives the chunk a non-null vector so nulling it is observable.
func setEmbedding(t *testing.T, db *DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE project_memory_chunks SET embedding = $2 WHERE id = $1`,
		id, pgVectorLiteral(1024)); err != nil {
		t.Fatalf("set embedding on %s: %v", id, err)
	}
}

func seedCache(t *testing.T, db *DB, hash, model string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO embedding_cache (content_hash, model, embedding)
		VALUES ($1, $2, $3) ON CONFLICT (content_hash, model) DO NOTHING`,
		hash, model, pgVectorLiteral(1024)); err != nil {
		t.Fatalf("seed cache %s/%s: %v", hash, model, err)
	}
}

func cleanRedactTest(t *testing.T, db *DB, hashes ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_embed_queue WHERE project_id = $1`, redactTestProject)
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, redactTestProject)
		for _, h := range hashes {
			_, _ = db.ExecContext(ctx, `DELETE FROM embedding_cache WHERE content_hash = $1`, h)
		}
	})
}

type chunkState struct {
	content  string
	hash     string
	hasEmbed bool
}

func readChunk(t *testing.T, db *DB, id string) chunkState {
	t.Helper()
	var s chunkState
	if err := db.QueryRowContext(context.Background(), `
		SELECT content, content_hash, embedding IS NOT NULL
		  FROM project_memory_chunks WHERE id = $1`, id,
	).Scan(&s.content, &s.hash, &s.hasEmbed); err != nil {
		t.Fatalf("read chunk %s: %v", id, err)
	}
	return s
}

func queued(t *testing.T, db *DB, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memory_embed_queue WHERE chunk_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count queue for %s: %v", id, err)
	}
	return n > 0
}

func cacheRows(t *testing.T, db *DB, hash string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM embedding_cache WHERE content_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("count cache for %s: %v", hash, err)
	}
	return n
}

// THE WHOLE SLICE IN ONE TEST: every post-condition of a successful redaction, all
// of which must hold together. The sharpest is the nulled embedding — a chunk whose
// text no longer names the subject but whose vector still does is still matched by a
// similarity search for that person, which is not erasure from the retrieval surface.
func TestIntegrationRedactChunk_AppliesEveryPostConditionTogether(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewChunkRedactorRepository(db.DB)

	before := "Called jane@example.com about the scan; Peter Novak joined."
	after := "Called the client about the scan; Peter Novak joined."
	newHash := memory.ContentHash(after)
	cleanRedactTest(t, db, memory.ContentHash(before), newHash)

	c := seedChunk(t, db, "redact-c1", before)
	setEmbedding(t, db, c.id)
	// Two models' vectors for the PRE-redaction text: both must be evicted.
	seedCache(t, db, c.hash, "text-embedding-3-small")
	seedCache(t, db, c.hash, "text-embedding-3-large")

	// Sanity: the fixture is in the state the assertions assume.
	if !readChunk(t, db, c.id).hasEmbed || cacheRows(t, db, c.hash) != 2 {
		t.Fatal("precondition: chunk should have an embedding and two cached vectors")
	}

	got, err := repo.RedactChunk(ctx, c.id, c.hash, after)
	if err != nil {
		t.Fatalf("RedactChunk: %v", err)
	}
	if got.Outcome != datasubject.RedactionApplied {
		t.Fatalf("outcome = %q, want applied", got.Outcome)
	}
	if got.NewHash != newHash {
		t.Errorf("NewHash = %q, want the hash of the redacted text %q", got.NewHash, newHash)
	}

	st := readChunk(t, db, c.id)
	if st.content != after {
		t.Errorf("content not replaced: %q", st.content)
	}
	if st.hash != newHash {
		t.Errorf("content_hash must be recomputed with memory.ContentHash, got %q", st.hash)
	}
	if st.hasEmbed {
		t.Error("THE LEAK: the embedding must be nulled in the same transaction — otherwise a " +
			"vector search for the erased subject still matches this chunk")
	}
	if !queued(t, db, c.id) {
		t.Error("a re-embed must be enqueued, or the chunk stays permanently unsearchable " +
			"and the OTHER subjects' data is degraded")
	}
	if n := cacheRows(t, db, c.hash); n != 0 {
		t.Errorf("every pre-redaction cached vector must be evicted, %d survived", n)
	}
	// And the other subject's data is still there — the point of redacting.
	if !strings.Contains(st.content, "Peter Novak") {
		t.Error("the other subject's data must survive a redaction")
	}
}

// The version guard, and its RECOVERY. The risk is not the guard failing to fire but
// a partial write happening anyway, so every column is checked.
func TestIntegrationRedactChunk_VersionGuardWritesNothing(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewChunkRedactorRepository(db.DB)

	before := "Called jane@example.com; Peter joined."
	cleanRedactTest(t, db, memory.ContentHash(before))
	c := seedChunk(t, db, "redact-guard", before)
	setEmbedding(t, db, c.id)
	seedCache(t, db, c.hash, "text-embedding-3-small")

	got, err := repo.RedactChunk(ctx, c.id, "a-stale-hash-from-planning", "Called the client.")
	if err != nil {
		t.Fatalf("a fired guard is not an error: %v", err)
	}
	if got.Outcome != datasubject.RedactionVersionChanged {
		t.Fatalf("outcome = %q, want version_changed", got.Outcome)
	}

	st := readChunk(t, db, c.id)
	if st.content != before || st.hash != c.hash {
		t.Error("content and hash must be untouched when the guard fires")
	}
	if !st.hasEmbed {
		t.Error("the embedding must NOT be nulled — that would leave the chunk unsearchable " +
			"for a write that never happened")
	}
	if queued(t, db, c.id) {
		t.Error("no re-embed may be enqueued for a write that did not happen")
	}
	if cacheRows(t, db, c.hash) != 1 {
		t.Error("the cache must not be evicted for a write that did not happen")
	}
}

// Collision (§4.2): the rewrite hashes to a chunk that already exists in the project.
// Nothing is written and the survivor is named, because the resolution depends on the
// plan and this layer cannot see it.
func TestIntegrationRedactChunk_CollisionReportsTheSurvivorAndWritesNothing(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewChunkRedactorRepository(db.DB)

	redacted := "Called the client; Peter joined."
	cleanRedactTest(t, db, memory.ContentHash("Called jane@example.com; Peter joined."),
		memory.ContentHash(redacted))

	victim := seedChunk(t, db, "redact-victim", "Called jane@example.com; Peter joined.")
	setEmbedding(t, db, victim.id)
	survivor := seedChunk(t, db, "redact-survivor", redacted)

	got, err := repo.RedactChunk(ctx, victim.id, victim.hash, redacted)
	if err != nil {
		t.Fatalf("RedactChunk: %v", err)
	}
	if got.Outcome != datasubject.RedactionCollision {
		t.Fatalf("outcome = %q, want collision", got.Outcome)
	}
	if got.SurvivorID != survivor.id {
		t.Errorf("SurvivorID = %q, want %q", got.SurvivorID, survivor.id)
	}
	// The victim is untouched: deleting it is the CALLER's decision, taken only after
	// checking whether the survivor is itself pending redaction.
	st := readChunk(t, db, victim.id)
	if st.content != victim.content || !st.hasEmbed {
		t.Error("the colliding chunk must be left exactly as it was for the caller to resolve")
	}
}

// A chunk in ANOTHER project with the same redacted text is not a collision —
// content_hash is unique per (project_id, content_hash), so cross-project identical
// text is legitimate and must not block the redaction.
func TestIntegrationRedactChunk_IdenticalTextInAnotherProjectIsNotACollision(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewChunkRedactorRepository(db.DB)

	redacted := "Called the client; Peter joined. Cross-project uniqueness check."
	before := "Called jane@example.com; Peter joined. Cross-project uniqueness check."
	cleanRedactTest(t, db, memory.ContentHash(before), memory.ContentHash(redacted))

	c := seedChunk(t, db, "redact-xproj", before)
	// Same text, different project.
	otherHash := memory.ContentHash(redacted)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO project_memory_chunks (id, project_id, content, content_hash, source_name)
		VALUES ('redact-xproj-other', 'some-other-project', $1, $2, 'integration-test')`,
		redacted, otherHash); err != nil {
		t.Fatalf("seed other-project chunk: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE id = 'redact-xproj-other'`)
	})

	got, err := repo.RedactChunk(ctx, c.id, c.hash, redacted)
	if err != nil {
		t.Fatalf("RedactChunk: %v", err)
	}
	if got.Outcome != datasubject.RedactionApplied {
		t.Fatalf("identical text in a different project must not collide, got %q (survivor %q)",
			got.Outcome, got.SurvivorID)
	}
}

// Idempotency (§9): re-running a redaction over already-redacted text is a no-op, and
// specifically must NOT null a good embedding or evict a live cache entry.
func TestIntegrationRedactChunk_ReRedactingIsANoOp(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewChunkRedactorRepository(db.DB)

	text := "Called the client; Peter joined. Already redacted."
	cleanRedactTest(t, db, memory.ContentHash(text))
	c := seedChunk(t, db, "redact-idem", text)
	setEmbedding(t, db, c.id)
	seedCache(t, db, c.hash, "text-embedding-3-small")

	got, err := repo.RedactChunk(ctx, c.id, c.hash, text)
	if err != nil {
		t.Fatalf("RedactChunk: %v", err)
	}
	if got.Outcome != datasubject.RedactionApplied {
		t.Fatalf("outcome = %q, want applied (no-op)", got.Outcome)
	}
	st := readChunk(t, db, c.id)
	if !st.hasEmbed {
		t.Error("a no-op must not null a perfectly good embedding")
	}
	if cacheRows(t, db, c.hash) != 1 {
		t.Error("a no-op must not evict a live cache entry")
	}
	if queued(t, db, c.id) {
		t.Error("a no-op must not enqueue a pointless re-embed")
	}
}

// LoadChunk gives the caller the version token and the text in one read, so the two
// cannot disagree.
func TestIntegrationLoadChunk_ReturnsTextAndVersionTokenTogether(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewChunkRedactorRepository(db.DB)
	text := "Called jane@example.com about the load path."
	cleanRedactTest(t, db, memory.ContentHash(text))
	c := seedChunk(t, db, "redact-load", text)

	gotContent, gotHash, err := repo.LoadChunk(context.Background(), c.id)
	if err != nil {
		t.Fatalf("LoadChunk: %v", err)
	}
	if gotContent != text || gotHash != c.hash {
		t.Errorf("LoadChunk = (%q, %q), want (%q, %q)", gotContent, gotHash, text, c.hash)
	}
	if _, _, err := repo.LoadChunk(context.Background(), "no-such-chunk"); err == nil {
		t.Error("a missing chunk must be an error, not empty text that would be 'redacted' to nothing")
	}
}

// tsv must remain a GENERATED column, or full-text search silently stops tracking
// redactions and the erased name stays findable through it (§4.1).
func TestIntegrationMemoryChunks_TSVIsStillAGeneratedColumn(t *testing.T) {
	db := newIntegrationDB(t)
	var generated string
	if err := db.QueryRowContext(context.Background(), `
		SELECT is_generated FROM information_schema.columns
		 WHERE table_name = 'project_memory_chunks' AND column_name = 'tsv'`).Scan(&generated); err != nil {
		t.Fatalf("read tsv column metadata: %v", err)
	}
	if generated != "ALWAYS" {
		t.Errorf("tsv must stay GENERATED ALWAYS (got %q) — redaction relies on full-text "+
			"search updating itself, and a maintained column would need explicit refresh", generated)
	}
}
