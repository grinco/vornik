package sqlite_test

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

func TestA2APushConfigRepository(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewA2APushConfigRepository(db.DB)
	ctx := context.Background()

	// Missing → ErrNotFound.
	if _, err := repo.Get(ctx, "task_x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}

	// Set + Get round-trip.
	if err := repo.Set(ctx, persistence.A2APushConfig{TaskID: "task_x", URL: "https://h/x", Token: "tok"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := repo.Get(ctx, "task_x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.URL != "https://h/x" || got.Token != "tok" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Upsert (last-write-wins on task_id).
	if err := repo.Set(ctx, persistence.A2APushConfig{TaskID: "task_x", URL: "https://h/y"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got2, _ := repo.Get(ctx, "task_x")
	if got2.URL != "https://h/y" || got2.Token != "" {
		t.Errorf("upsert mismatch: %+v", got2)
	}

	// Empty url rejected.
	if err := repo.Set(ctx, persistence.A2APushConfig{TaskID: "task_y"}); err == nil {
		t.Errorf("empty url: want error")
	}
}
