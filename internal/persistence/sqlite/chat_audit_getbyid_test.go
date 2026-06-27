package sqlite_test

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestChatAuditRepository_GetByID covers the lookup the steering notifier uses
// to resolve a task's originating channel+session from its ChatTurnID.
func TestChatAuditRepository_GetByID(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChatAuditRepository(db.DB)
	ctx := context.Background()

	want := &persistence.ChatAuditEntry{
		ID:        "chat_turn_1",
		ChatID:    "slack:T1/C2#169.42",
		UserID:    "slack:U123",
		ProjectID: "p1",
		RoleUsed:  "dispatcher",
		Model:     "m",
	}
	if err := repo.Insert(ctx, want); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.GetByID(ctx, "chat_turn_1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ChatID != want.ChatID || got.UserID != want.UserID || got.ProjectID != want.ProjectID {
		t.Errorf("round-trip mismatch: got %+v", got)
	}

	// Missing → ErrNotFound; empty id → ErrNotFound.
	if _, err := repo.GetByID(ctx, "nope"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("missing id: want ErrNotFound, got %v", err)
	}
	if _, err := repo.GetByID(ctx, ""); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("empty id: want ErrNotFound, got %v", err)
	}
}
