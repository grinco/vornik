package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// A mention whose chunk row is gone is not listed. Postgres gets this from
// its JOIN on project_memory_chunks; SQLite listed the stranded row until
// 2026-09-09 because it read entity_mentions alone (backlog 2026-09-05,
// backend-contract coverage design §8). SQLite-only because the shared suite
// cannot seed the case: on Postgres the foreign key refuses an orphan, while
// this driver runs with foreign_keys OFF (sqlite.go), so an orphan is a real
// state here — a chunk deleted by any path that does not sweep mentions.
func TestEntityMention_ListByEntityDropsAMentionWhoseChunkIsGone(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seed := seedChunkForSuite(db)
	if err := seed(ctx, "chunk-live", "proj", "body", false, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repo := sqlite.NewEntityMentionRepository(db.DB)
	for _, chunk := range []string{"chunk-live", "chunk-ghost"} {
		if err := repo.Insert(ctx, &persistence.EntityMention{ChunkID: chunk, EntityID: "ent-1", CharStart: 0}); err != nil {
			t.Fatalf("Insert %s: %v", chunk, err)
		}
	}
	got, err := repo.ListByEntity(ctx, "ent-1", 10)
	if err != nil {
		t.Fatalf("ListByEntity: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "chunk-live" {
		t.Fatalf("want only the mention whose chunk exists, got %+v", got)
	}
}
