package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// countingDBTX wraps a real DBTX and counts QueryContext calls, so a
// test can prove a batch method issues exactly one round-trip instead
// of one per item (the N+1 the Outcome Inbox design's origin-badge
// lookup exists to avoid — §5.5, review finding 3).
type countingDBTX struct {
	inner      persistence.DBTX
	queryCalls int
}

func (c *countingDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	c.queryCalls++
	return c.inner.QueryContext(ctx, query, args...)
}
func (c *countingDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return c.inner.QueryRowContext(ctx, query, args...)
}
func (c *countingDBTX) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return c.inner.ExecContext(ctx, query, args...)
}

// TestChatAuditRepository_GetChatAuditsByTurnIDs_SingleRoundTrip inserts
// several rows then resolves them all through one GetChatAuditsByTurnIDs
// call, asserting via the counting wrapper that exactly one QueryContext
// call served the whole batch.
func TestChatAuditRepository_GetChatAuditsByTurnIDs_SingleRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seed := sqlite.NewChatAuditRepository(db.DB)

	entries := []*persistence.ChatAuditEntry{
		{ID: "chat_a", ChatID: "telegram:1", UserID: "u1", ProjectID: "p1", RoleUsed: "dispatcher", Model: "m"},
		{ID: "chat_b", ChatID: "telegram:2", UserID: "u2", ProjectID: "p1", RoleUsed: "dispatcher", Model: "m"},
		{ID: "chat_c", ChatID: "web-chat:uuid", UserID: "u3", ProjectID: "p1", RoleUsed: "dispatcher", Model: "m"},
	}
	for _, e := range entries {
		if err := seed.Insert(ctx, e); err != nil {
			t.Fatalf("Insert %s: %v", e.ID, err)
		}
	}

	counting := &countingDBTX{inner: db.DB}
	repo := sqlite.NewChatAuditRepository(counting)

	got, err := repo.GetChatAuditsByTurnIDs(ctx, []string{"chat_a", "chat_b", "chat_c", "chat_ghost"})
	if err != nil {
		t.Fatalf("GetChatAuditsByTurnIDs: %v", err)
	}
	if counting.queryCalls != 1 {
		t.Errorf("QueryContext called %d times, want exactly 1 (single round-trip for the whole batch)", counting.queryCalls)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	if got["chat_a"].ChatID != "telegram:1" || got["chat_b"].ChatID != "telegram:2" || got["chat_c"].ChatID != "web-chat:uuid" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if _, ok := got["chat_ghost"]; ok {
		t.Error("chat_ghost should be absent from the map (no such row), not zero-valued")
	}
}

// TestChatAuditRepository_GetChatAuditsByTurnIDs_EmptyInput short-circuits
// without hitting the DB at all.
func TestChatAuditRepository_GetChatAuditsByTurnIDs_EmptyInput(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChatAuditRepository(db.DB)

	got, err := repo.GetChatAuditsByTurnIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
}
