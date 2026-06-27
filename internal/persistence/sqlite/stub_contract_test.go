package sqlite_test

// Coverage-gap sweep (2026-06-18). Five repositories are Postgres-only
// by design — their durable round-trip contract is proved in the
// Postgres integration sweep. On SQLite they are deliberate degenerate
// stubs: either they return an explicit "not supported" sentinel
// (reminders, cross-project calls) or they silently no-op so a
// single-process deployment keeps authoritative state in memory
// (channel sessions, telegram poller offset, profile-use audit).
//
// These tests LOCK that intentional behaviour. The risk they guard
// against is a future change that half-implements the SQLite side and
// silently swaps "loud sentinel error" or "safe no-op" for "looks
// like it worked but didn't persist" — which would corrupt the
// in-memory authority assumption callers rely on. If you make one of
// these backends durable on SQLite, delete its stub case here and add
// the repo to repotest_test.go's shared-suite list instead.

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func TestReminderRepository_SQLiteUnsupported(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewReminderRepository(newTestDB(t).DB)

	if err := repo.Insert(ctx, &persistence.Reminder{ID: "r1"}); !errors.Is(err, sqlite.ErrSQLiteRemindersUnsupported) {
		t.Fatalf("Insert: expected ErrSQLiteRemindersUnsupported, got %v", err)
	}
	if _, err := repo.Get(ctx, "r1"); !errors.Is(err, sqlite.ErrSQLiteRemindersUnsupported) {
		t.Fatalf("Get: expected ErrSQLiteRemindersUnsupported, got %v", err)
	}
	if _, err := repo.List(ctx, persistence.ReminderListFilter{}); !errors.Is(err, sqlite.ErrSQLiteRemindersUnsupported) {
		t.Fatalf("List: expected ErrSQLiteRemindersUnsupported, got %v", err)
	}
	n, err := repo.CountPendingByOperator(ctx, "op")
	if !errors.Is(err, sqlite.ErrSQLiteRemindersUnsupported) || n != 0 {
		t.Fatalf("CountPendingByOperator: expected (0, ErrSQLiteRemindersUnsupported), got (%d, %v)", n, err)
	}
}

func TestCrossProjectCallRepository_SQLiteUnsupported(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewCrossProjectCallRepository(newTestDB(t).DB)

	if err := repo.Create(ctx, &persistence.CrossProjectCall{ID: "c1"}); !errors.Is(err, sqlite.ErrSQLiteNotSupported) {
		t.Fatalf("Create: expected ErrSQLiteNotSupported, got %v", err)
	}
	if _, err := repo.Get(ctx, "c1"); !errors.Is(err, sqlite.ErrSQLiteNotSupported) {
		t.Fatalf("Get: expected ErrSQLiteNotSupported, got %v", err)
	}
	if _, err := repo.List(ctx, persistence.CPCListFilter{}); !errors.Is(err, sqlite.ErrSQLiteNotSupported) {
		t.Fatalf("List: expected ErrSQLiteNotSupported, got %v", err)
	}
	if err := repo.MarkRunning(ctx, "c1"); !errors.Is(err, sqlite.ErrSQLiteNotSupported) {
		t.Fatalf("MarkRunning: expected ErrSQLiteNotSupported, got %v", err)
	}
}

func TestChannelSessionRepository_SQLiteNoOp(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewChannelSessionRepository(newTestDB(t).DB)

	// Save is a no-op; Load reports ErrNotFound so callers fall back to a
	// fresh empty session.
	if err := repo.Save(ctx, "webchat", "s1", "proj", []byte(`[]`)); err != nil {
		t.Fatalf("Save (no-op): expected nil, got %v", err)
	}
	if _, err := repo.Load(ctx, "webchat", "s1"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Load: expected ErrNotFound (nothing persisted), got %v", err)
	}
	if err := repo.Delete(ctx, "webchat", "s1"); err != nil {
		t.Fatalf("Delete (no-op): expected nil, got %v", err)
	}
}

func TestTelegramPollerStateRepository_SQLiteNoOp(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewTelegramPollerStateRepository(newTestDB(t).DB)

	if err := repo.Set(ctx, &persistence.TelegramPollerState{BotID: "bot", Offset: 5}); err != nil {
		t.Fatalf("Set (no-op): expected nil, got %v", err)
	}
	if _, err := repo.Get(ctx, "bot"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Get: expected ErrNotFound (nothing persisted), got %v", err)
	}
}

func TestProfileUseAuditRepository_SQLiteNoOp(t *testing.T) {
	ctx := context.Background()
	repo := sqlite.NewProfileUseAuditRepository(newTestDB(t).DB)

	if err := repo.Insert(ctx, &persistence.ProfileUseAudit{OperatorID: "op"}); err != nil {
		t.Fatalf("Insert (no-op): expected nil, got %v", err)
	}
	got, err := repo.ListForOperator(ctx, "op", persistence.ProfileUseAuditQuery{})
	if err != nil {
		t.Fatalf("ListForOperator: expected nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListForOperator: expected empty (nothing persisted), got %d rows", len(got))
	}
	if err := repo.DeleteAllForOperator(ctx, "op"); err != nil {
		t.Fatalf("DeleteAllForOperator (no-op): expected nil, got %v", err)
	}
}
