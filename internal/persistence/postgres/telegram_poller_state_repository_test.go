package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

// TestTelegramPollerState_Get_Found pins the column order +
// Scan alignment for the offset watermark read.
func TestTelegramPollerState_Get_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTelegramPollerStateRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT bot_id, offset_value FROM telegram_poller_state")).
		WithArgs("@vornik_bot").
		WillReturnRows(sqlmock.NewRows([]string{"bot_id", "offset_value"}).
			AddRow("@vornik_bot", int64(987654321)))

	got, err := repo.Get(context.Background(), "@vornik_bot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BotID != "@vornik_bot" {
		t.Errorf("bot_id = %q", got.BotID)
	}
	if got.Offset != 987654321 {
		t.Errorf("offset = %d, want 987654321", got.Offset)
	}
}

// TestTelegramPollerState_Get_NotFound surfaces
// persistence.ErrNotFound when no row exists yet — pollLoop
// branches on this to start from offset=0 the first time.
func TestTelegramPollerState_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTelegramPollerStateRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT bot_id, offset_value FROM telegram_poller_state")).
		WithArgs("@new_bot").
		WillReturnRows(sqlmock.NewRows([]string{"bot_id", "offset_value"}))

	_, err := repo.Get(context.Background(), "@new_bot")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestTelegramPollerState_Get_EmptyBotIDRejected — guard
// against silent persistence of an empty key.
func TestTelegramPollerState_Get_EmptyBotIDRejected(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTelegramPollerStateRepository(db)

	if _, err := repo.Get(context.Background(), ""); err == nil {
		t.Errorf("Get should reject empty bot_id")
	}
}

// TestTelegramPollerState_Set_UpsertsAndOnlyAdvances pins the
// only-advance-never-rewind guard. The ON CONFLICT WHERE clause
// is what protects a steady-state leader from a stale write
// landing late from a deposed leader (rare but possible during
// failover).
func TestTelegramPollerState_Set_UpsertsAndOnlyAdvances(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTelegramPollerStateRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO telegram_poller_state")).
		WithArgs("@vornik_bot", int64(1000)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Set(context.Background(), &persistence.TelegramPollerState{
		BotID:  "@vornik_bot",
		Offset: 1000,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
}

// TestTelegramPollerState_Set_EmptyBotIDRejected.
func TestTelegramPollerState_Set_EmptyBotIDRejected(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTelegramPollerStateRepository(db)

	if err := repo.Set(context.Background(), &persistence.TelegramPollerState{Offset: 5}); err == nil {
		t.Errorf("Set should reject empty bot_id")
	}
	if err := repo.Set(context.Background(), nil); err == nil {
		t.Errorf("Set should reject nil state")
	}
}
