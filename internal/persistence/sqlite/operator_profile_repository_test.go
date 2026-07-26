package sqlite

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestOperatorProfileRepository_RoundTrip proves SQLite does not acknowledge
// update_operator_profile while silently discarding it. CE defaults to SQLite,
// so this is the persistence contract behind both the dispatcher tool and UI.
func TestOperatorProfileRepository_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Connect(ctx, Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	repo := NewOperatorProfileRepository(db.DB)
	want := &persistence.OperatorProfile{
		OperatorID: "slack:U123",
		Structured: []byte(`{"verbosity":"concise"}`),
		Notes:      "prefers short answers",
	}
	if err := repo.Upsert(ctx, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.Get(ctx, want.OperatorID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OperatorID != want.OperatorID || string(got.Structured) != string(want.Structured) || got.Notes != want.Notes {
		t.Fatalf("round trip got %+v, want %+v", got, want)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not populated: %+v", got)
	}

	rows, err := repo.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].OperatorID != want.OperatorID {
		t.Fatalf("List got %+v", rows)
	}

	if err := repo.Delete(ctx, want.OperatorID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, want.OperatorID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Get after Delete error=%v, want ErrNotFound", err)
	}
}

func TestOperatorProfileRepository_DeleteRejectsEmptyOperatorID(t *testing.T) {
	db, err := Connect(context.Background(), Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	repo := NewOperatorProfileRepository(db.DB)
	if err := repo.Delete(context.Background(), ""); err == nil {
		t.Fatal("Delete empty operator ID succeeded, want validation error")
	}
}
