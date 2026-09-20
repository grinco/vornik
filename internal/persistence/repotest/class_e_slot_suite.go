package repotest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// RunClassESlotSuite pins the class-E admission cap on BOTH backends.
//
// The cap is a PRIMARY KEY rather than a counted-then-inserted row precisely so
// that the two drivers enforce it identically, with no isolation-level argument
// to get wrong twice (config-assistant design §13.9a). A shared suite is how
// that claim stops being an assertion: duplicated code with a shared contract is
// a deliberate duplication, duplicated code without one is two things that used
// to agree.
func RunClassESlotSuite(t *testing.T, repo persistence.ClassESlotRepository) {
	ctx := context.Background()
	day := "2026-09-19"

	t.Run("a free slot is granted", func(t *testing.T) {
		cred := uniqueID("cred")
		wantOK(t, "Reserve(slot 1)", repo.Reserve(ctx, cred, day, 1))
	})

	t.Run("a taken slot is ErrDuplicateKey, not a silent no-op", func(t *testing.T) {
		cred := uniqueID("cred")
		wantOK(t, "Reserve(slot 1)", repo.Reserve(ctx, cred, day, 1))

		err := repo.Reserve(ctx, cred, day, 1)
		if !errors.Is(err, persistence.ErrDuplicateKey) {
			// An ON CONFLICT DO NOTHING would return nil here, and the caller
			// could not tell admission from refusal without a second query —
			// which is the race the whole design exists to remove.
			t.Fatalf("want ErrDuplicateKey for a taken slot, got %v", err)
		}
	})

	t.Run("slots are independent within a day", func(t *testing.T) {
		cred := uniqueID("cred")
		wantOK(t, "Reserve(slot 1)", repo.Reserve(ctx, cred, day, 1))
		wantOK(t, "Reserve(slot 2)", repo.Reserve(ctx, cred, day, 2))
		wantOK(t, "Reserve(slot 3)", repo.Reserve(ctx, cred, day, 3))
	})

	t.Run("the bucket is per credential", func(t *testing.T) {
		a, b := uniqueID("cred"), uniqueID("cred")
		wantOK(t, "Reserve(a, slot 1)", repo.Reserve(ctx, a, day, 1))
		wantOK(t, "Reserve(b, slot 1)", repo.Reserve(ctx, b, day, 1))
	})

	t.Run("the bucket is per day", func(t *testing.T) {
		cred := uniqueID("cred")
		wantOK(t, "Reserve(day 1)", repo.Reserve(ctx, cred, "2026-09-19", 1))
		wantOK(t, "Reserve(day 2)", repo.Reserve(ctx, cred, "2026-09-20", 1))
	})

	// The property the cap actually rests on: concurrent reservers of ONE slot
	// produce exactly one winner. On Postgres this is the unique index; on
	// SQLite it is the same constraint under a different lock regime, which is
	// why it is asserted here rather than reasoned about once.
	t.Run("concurrent reservers of one slot produce exactly one winner", func(t *testing.T) {
		cred := uniqueID("cred")
		const racers = 8

		var wg sync.WaitGroup
		results := make(chan error, racers)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- repo.Reserve(ctx, cred, day, 1)
			}()
		}
		wg.Wait()
		close(results)

		granted, duplicates, other := 0, 0, 0
		for err := range results {
			switch {
			case err == nil:
				granted++
			case errors.Is(err, persistence.ErrDuplicateKey):
				duplicates++
			default:
				other++
				t.Errorf("unexpected error from a racing reserver: %v", err)
			}
		}
		if granted != 1 {
			t.Fatalf("want exactly 1 winner, got %d (duplicates=%d, other=%d)", granted, duplicates, other)
		}
	})

	t.Run("a malformed reservation is refused before it reaches the table", func(t *testing.T) {
		if err := repo.Reserve(ctx, "", day, 1); err == nil {
			t.Fatal("an empty credential was accepted; an unattributable slot charges nobody")
		}
		if err := repo.Reserve(ctx, uniqueID("cred"), day, 0); err == nil {
			t.Fatal("slot 0 was accepted; slots are 1-based and the cap counts them")
		}
	})
}
