package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// §5.4 follow-up (CA-09). The bound lives in the one function every claim door
// calls, so no door can be added past it — but the BUCKET was process memory,
// so N replicas allowed N times the attempts and a restart cleared the count.

type fakeClaimAttempts struct {
	counts map[string]int
	err    error
	calls  int
}

func (f *fakeClaimAttempts) RecordClaimAttempt(_ context.Context, keyID string, _ time.Time, _ time.Duration) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	f.counts[keyID]++
	return f.counts[keyID], nil
}

func (f *fakeClaimAttempts) PruneClaimAttempts(context.Context, time.Time) (int64, error) {
	return 0, nil
}

var _ persistence.KeyClaimAttemptRepository = (*fakeClaimAttempts)(nil)

func TestAllowClaimAttempt_UsesTheDurableStoreWhenWired(t *testing.T) {
	store := &fakeClaimAttempts{counts: map[string]int{}}
	a := &Accounts{claims: newClaimLimiter(), claimAttempts: store}

	for i := 1; i <= ClaimAttemptLimit; i++ {
		if !a.allowClaimAttempt(context.Background(), "key-1") {
			t.Fatalf("attempt %d was refused inside the bound", i)
		}
	}
	if a.allowClaimAttempt(context.Background(), "key-1") {
		t.Fatalf("attempt %d was allowed past the bound of %d", ClaimAttemptLimit+1, ClaimAttemptLimit)
	}
	if store.calls != ClaimAttemptLimit+1 {
		t.Fatalf("the durable store was not consulted on every attempt: %d calls", store.calls)
	}
}

// A second process shares the bound, which is the whole point: the fake stands
// in for the shared table, and two Accounts values stand in for two replicas.
func TestAllowClaimAttempt_BoundIsSharedAcrossProcesses(t *testing.T) {
	store := &fakeClaimAttempts{counts: map[string]int{}}
	replicaA := &Accounts{claims: newClaimLimiter(), claimAttempts: store}
	replicaB := &Accounts{claims: newClaimLimiter(), claimAttempts: store}

	for i := 0; i < ClaimAttemptLimit; i++ {
		target := replicaA
		if i%2 == 1 {
			target = replicaB
		}
		if !target.allowClaimAttempt(context.Background(), "key-1") {
			t.Fatalf("attempt %d refused inside the bound", i+1)
		}
	}
	if replicaB.allowClaimAttempt(context.Background(), "key-1") {
		t.Fatal("a second replica had its own bucket; the bound is still per process")
	}
}

// An unreadable store refuses. The bound exists to bound a guesser, so "cannot
// tell" is not permission — and falling back to the in-memory bucket would mean
// a deployment with a flapping database quietly loses the property it believes
// it has.
func TestAllowClaimAttempt_StoreErrorRefuses(t *testing.T) {
	a := &Accounts{claims: newClaimLimiter(), claimAttempts: &fakeClaimAttempts{err: errors.New("db down")}}
	if a.allowClaimAttempt(context.Background(), "key-1") {
		t.Fatal("an unreadable attempt store admitted a claim")
	}
}

// No store wired: the process-local bucket still bounds, which is correct for a
// single-process deployment. The bound must never disappear because a store is
// missing.
func TestAllowClaimAttempt_FallsBackToTheInMemoryBucket(t *testing.T) {
	a := &Accounts{claims: newClaimLimiter()}
	for i := 1; i <= ClaimAttemptLimit; i++ {
		if !a.allowClaimAttempt(context.Background(), "key-1") {
			t.Fatalf("attempt %d refused inside the bound", i)
		}
	}
	if a.allowClaimAttempt(context.Background(), "key-1") {
		t.Fatal("the in-memory fallback stopped bounding")
	}
}
