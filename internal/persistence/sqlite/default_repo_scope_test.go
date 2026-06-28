package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestAPIKeyRepo_DefaultRepoScope is the TDD anchor for migration 110:
// the default_repo_scope column round-trips through Create + the SELECT
// scan paths, and an unset value reads back as the empty string (NULL).
func TestAPIKeyRepo_DefaultRepoScope(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewAPIKeyRepository(db.DB)

	scoped := &persistence.APIKey{
		ID:               "akey-scoped",
		ProjectID:        "companion-example",
		Name:             "codex-session",
		KeyHash:          "hash-scoped",
		KeyPrefix:        "sk-scoped",
		ClientKind:       "codex",
		DefaultRepoScope: "github.com/grinco/vornik",
		CreatedAt:        time.Now().UTC(),
	}
	if err := repo.Create(ctx, scoped); err != nil {
		t.Fatalf("Create scoped: %v", err)
	}

	got, err := repo.LookupActiveByHash(ctx, "hash-scoped")
	if err != nil {
		t.Fatalf("LookupActiveByHash: %v", err)
	}
	if got.DefaultRepoScope != "github.com/grinco/vornik" {
		t.Errorf("DefaultRepoScope round-trip = %q, want github.com/grinco/vornik", got.DefaultRepoScope)
	}

	// A key minted without a default scope must read back as empty
	// (the NULL column), not error.
	plain := &persistence.APIKey{
		ID:        "akey-plain",
		ProjectID: "companion-example",
		Name:      "plain-session",
		KeyHash:   "hash-plain",
		KeyPrefix: "sk-plain",
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(ctx, plain); err != nil {
		t.Fatalf("Create plain: %v", err)
	}
	gotPlain, err := repo.LookupActiveByHash(ctx, "hash-plain")
	if err != nil {
		t.Fatalf("LookupActiveByHash plain: %v", err)
	}
	if gotPlain.DefaultRepoScope != "" {
		t.Errorf("unset DefaultRepoScope = %q, want empty string", gotPlain.DefaultRepoScope)
	}

	// The scoped value must also survive the list-path scan.
	rows, err := repo.ListCompanionByProject(ctx, "companion-example")
	if err != nil {
		t.Fatalf("ListCompanionByProject: %v", err)
	}
	var foundScoped bool
	for _, r := range rows {
		if r.ID == "akey-scoped" {
			foundScoped = true
			if r.DefaultRepoScope != "github.com/grinco/vornik" {
				t.Errorf("list-path DefaultRepoScope = %q, want github.com/grinco/vornik", r.DefaultRepoScope)
			}
		}
	}
	if !foundScoped {
		t.Fatal("scoped companion key missing from ListCompanionByProject")
	}
}
