package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestChatAuditRepository_GetChatAuditsByTurnIDs — the batch origin-audit
// lookup added for the Outcome Inbox request card's channel-origin badge
// (design §5.5, review finding 3): resolves several rows in one query,
// keyed by ID, with turnIDs absent from the table simply missing from
// the map (not zero-valued).
func TestChatAuditRepository_GetChatAuditsByTurnIDs(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	created := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE id = ANY($1)")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "ts", "chat_id", "user_id", "project_id", "role_used", "model",
		}).
			AddRow("chat_1", created, "chat-x", "u1", "proj-y", "lead", "m").
			AddRow("chat_2", created, "chat-z", "u2", "proj-y", "lead", "m"))

	got, err := repo.GetChatAuditsByTurnIDs(context.Background(), []string{"chat_1", "chat_2", "chat_missing"})
	if err != nil {
		t.Fatalf("GetChatAuditsByTurnIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got["chat_1"].ChatID != "chat-x" || got["chat_2"].ChatID != "chat-z" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if _, ok := got["chat_missing"]; ok {
		t.Errorf("chat_missing should be absent from the map, not zero-valued")
	}
}

// TestChatAuditRepository_GetChatAuditsByTurnIDs_EmptyInput short-circuits
// without hitting the DB.
func TestChatAuditRepository_GetChatAuditsByTurnIDs_EmptyInput(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	got, err := repo.GetChatAuditsByTurnIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no SQL should have been issued: %v", err)
	}
}

// TestChatAuditRepository_GetChatAuditsByTurnIDs_QueryError propagates
// the DB error rather than returning a half-populated map.
func TestChatAuditRepository_GetChatAuditsByTurnIDs_QueryError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("WHERE id = ANY($1)")).
		WillReturnError(errors.New("boom"))

	_, err := repo.GetChatAuditsByTurnIDs(context.Background(), []string{"chat_1"})
	if err == nil {
		t.Fatal("expected propagated query error")
	}
}
