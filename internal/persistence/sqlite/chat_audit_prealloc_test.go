package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestChatAuditRepository_List_PreallocClamped is a regression test for the
// CodeQL go/uncontrolled-allocation-size finding at List's result-slice
// pre-allocation. filter.PageSize reaches the repository straight from an
// HTTP query parameter; before the fix `make([]*Entry, 0, filter.PageSize)`
// pre-allocated an attacker-controlled capacity (a huge PageSize → huge
// allocation / DoS). The fix clamps the capacity hint with
// min(PageSize, maxChatAuditPrealloc).
//
// We prove the clamp by observing the returned slice's capacity: with a
// pathological PageSize but only a handful of rows, append never has to grow
// the slice, so cap(out) equals the (clamped) initial capacity. Pre-fix this
// would be ~1<<30; post-fix it is bounded.
func TestChatAuditRepository_List_PreallocClamped(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewChatAuditRepository(db.DB)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := repo.Insert(ctx, &persistence.ChatAuditEntry{
			ChatID: "c1", UserID: "u1", ProjectID: "p1", RoleUsed: "r", Model: "m",
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	const huge = 1 << 30
	got, err := repo.List(ctx, persistence.ChatAuditFilter{ProjectID: "p1", PageSize: huge})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Behaviour preserved: all matching rows still returned (LIMIT uses the
	// true PageSize, and append grows past the capacity hint).
	if len(got) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(got))
	}
	// Security property: the pre-allocation was bounded, not driven by the
	// attacker-supplied PageSize.
	if cap(got) > 4096 {
		t.Errorf("List pre-allocated cap=%d for PageSize=%d; expected the capacity hint to be clamped (uncontrolled-allocation-size regression)", cap(got), huge)
	}
}
