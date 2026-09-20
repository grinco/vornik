package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunKeyClaimAttemptSuite pins the cluster-wide claim bound on BOTH backends.
//
// §5.4's bound is 10 attempts per key id per hour, shared across claimants and
// addresses because the attack is guessing ONE key from many. It lived in
// process memory, so N replicas allowed N times the attempts and a restart
// cleared the count.
func RunKeyClaimAttemptSuite(t *testing.T, repo persistence.KeyClaimAttemptRepository) {
	ctx := context.Background()
	const window = time.Hour

	t.Run("the count includes the attempt just recorded", func(t *testing.T) {
		key := uniqueID("key")
		now := time.Now().UTC()

		n, err := repo.RecordClaimAttempt(ctx, key, now, window)
		wantOK(t, "RecordClaimAttempt", err)
		if n != 1 {
			t.Fatalf("want 1 after the first attempt, got %d", n)
		}
		n, err = repo.RecordClaimAttempt(ctx, key, now.Add(time.Second), window)
		wantOK(t, "RecordClaimAttempt", err)
		if n != 2 {
			t.Fatalf("want 2 after the second attempt, got %d", n)
		}
	})

	t.Run("attempts outside the window do not count", func(t *testing.T) {
		key := uniqueID("key")
		now := time.Now().UTC()

		// Two attempts, both older than the window.
		for _, age := range []time.Duration{3 * time.Hour, 2 * time.Hour} {
			_, err := repo.RecordClaimAttempt(ctx, key, now.Add(-age), window)
			wantOK(t, "RecordClaimAttempt(old)", err)
		}
		n, err := repo.RecordClaimAttempt(ctx, key, now, window)
		wantOK(t, "RecordClaimAttempt(now)", err)
		if n != 1 {
			t.Fatalf("stale attempts leaked into the window: want 1, got %d", n)
		}
	})

	t.Run("the bucket is per key id", func(t *testing.T) {
		a, b := uniqueID("key"), uniqueID("key")
		now := time.Now().UTC()

		for i := 0; i < 3; i++ {
			_, err := repo.RecordClaimAttempt(ctx, a, now, window)
			wantOK(t, "RecordClaimAttempt(a)", err)
		}
		n, err := repo.RecordClaimAttempt(ctx, b, now, window)
		wantOK(t, "RecordClaimAttempt(b)", err)
		if n != 1 {
			t.Fatalf("key b inherited key a's attempts: got %d", n)
		}
	})

	// Two attempts in the same instant are TWO attempts. Collapsing them —
	// through a unique constraint or an upsert — would under-count exactly the
	// burst the bound exists to catch.
	t.Run("simultaneous attempts are not collapsed", func(t *testing.T) {
		key := uniqueID("key")
		now := time.Now().UTC()

		first, err := repo.RecordClaimAttempt(ctx, key, now, window)
		wantOK(t, "RecordClaimAttempt(1)", err)
		second, err := repo.RecordClaimAttempt(ctx, key, now, window)
		wantOK(t, "RecordClaimAttempt(2)", err)

		if first != 1 || second != 2 {
			t.Fatalf("same-instant attempts were collapsed: %d then %d", first, second)
		}
	})

	runKeyClaimAttemptHousekeeping(t, repo, window)
}

// runKeyClaimAttemptHousekeeping covers pruning and the argument guard. Split
// out because the suite is one function per contract and this half is
// housekeeping rather than admission — and because the linter's length bound is
// a reasonable prompt to say which is which.
func runKeyClaimAttemptHousekeeping(t *testing.T, repo persistence.KeyClaimAttemptRepository, window time.Duration) {
	ctx := context.Background()

	t.Run("pruning removes only what is outside the window", func(t *testing.T) {
		key := uniqueID("key")
		now := time.Now().UTC()

		_, err := repo.RecordClaimAttempt(ctx, key, now.Add(-4*time.Hour), window)
		wantOK(t, "RecordClaimAttempt(old)", err)
		_, err = repo.RecordClaimAttempt(ctx, key, now, window)
		wantOK(t, "RecordClaimAttempt(new)", err)

		if _, err := repo.PruneClaimAttempts(ctx, now.Add(-window)); err != nil {
			t.Fatalf("PruneClaimAttempts: %v", err)
		}
		n, err := repo.RecordClaimAttempt(ctx, key, now, window)
		wantOK(t, "RecordClaimAttempt(after prune)", err)
		if n != 2 {
			t.Fatalf("pruning removed an in-window attempt: want 2, got %d", n)
		}
	})

	t.Run("a missing key id is refused", func(t *testing.T) {
		if _, err := repo.RecordClaimAttempt(ctx, "", time.Now().UTC(), window); err == nil {
			t.Fatal("an empty key id was accepted; the bucket would be shared by every key")
		}
	})
}
