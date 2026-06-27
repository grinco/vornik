package workspacelock

import (
	"sync"
	"testing"
	"time"
)

// lockTimeout bounds "did not acquire" assertions. Long enough to be
// reliable on a loaded CI box, short enough to keep the suite snappy.
const lockTimeout = 100 * time.Millisecond

// tryAcquireLock attempts l.Lock(key) in a goroutine and reports whether it
// was acquired within the timeout. On timeout the goroutine may STILL
// eventually acquire the lock; to avoid leaking that acquisition and
// poisoning a later assertion in the same test, the goroutine immediately
// releases when it finds the caller has already given up.
func tryAcquireLock(l *Locker, key string) (func(), bool) {
	ch := make(chan func(), 1)
	go func() { ch <- l.Lock(key) }()
	select {
	case unlock := <-ch:
		return unlock, true
	case <-time.After(lockTimeout):
		go func() { (<-ch)() }()
		return nil, false
	}
}

func tryAcquireRLock(l *Locker, key string) (func(), bool) {
	ch := make(chan func(), 1)
	go func() { ch <- l.RLock(key) }()
	select {
	case unlock := <-ch:
		return unlock, true
	case <-time.After(lockTimeout):
		go func() { (<-ch)() }()
		return nil, false
	}
}

// TestLocker_ExclusiveExcludesSecondLock asserts a second exclusive Lock
// blocks until the first releases.
func TestLocker_ExclusiveExcludesSecondLock(t *testing.T) {
	l := New()
	unlock := l.Lock("p")

	if _, ok := tryAcquireLock(l, "p"); ok {
		t.Fatal("second Lock acquired while first is held — not mutually exclusive")
	}

	unlock()

	unlock2, ok := tryAcquireLock(l, "p")
	if !ok {
		t.Fatal("second Lock did not acquire after first released")
	}
	unlock2()
}

// TestLocker_DifferentKeysIndependent asserts locks on different keys don't
// serialize each other.
func TestLocker_DifferentKeysIndependent(t *testing.T) {
	l := New()
	unlockA := l.Lock("a")
	defer unlockA()

	unlockB, ok := tryAcquireLock(l, "b")
	if !ok {
		t.Fatal("Lock on a different key blocked — locks are not per-key independent")
	}
	unlockB()
}

// TestLocker_RLockAllowsConcurrentReaders asserts two RLock holders proceed
// concurrently.
func TestLocker_RLockAllowsConcurrentReaders(t *testing.T) {
	l := New()
	unlock1 := l.RLock("p")
	defer unlock1()

	unlock2, ok := tryAcquireRLock(l, "p")
	if !ok {
		t.Fatal("second RLock blocked while first held — readers must be concurrent")
	}
	unlock2()
}

// TestLocker_ExclusiveExcludesRLock asserts an exclusive Lock blocks a
// reader, and the reader proceeds only after release.
func TestLocker_ExclusiveExcludesRLock(t *testing.T) {
	l := New()
	unlock := l.Lock("p")

	if _, ok := tryAcquireRLock(l, "p"); ok {
		t.Fatal("RLock acquired while exclusive Lock held — must be excluded")
	}

	unlock()

	runlock, ok := tryAcquireRLock(l, "p")
	if !ok {
		t.Fatal("RLock did not acquire after exclusive Lock released")
	}
	runlock()
}

// TestLocker_RLockExcludesExclusive asserts a held RLock blocks a writer.
func TestLocker_RLockExcludesExclusive(t *testing.T) {
	l := New()
	runlock := l.RLock("p")

	if _, ok := tryAcquireLock(l, "p"); ok {
		t.Fatal("exclusive Lock acquired while RLock held — writer must be excluded by reader")
	}

	runlock()

	unlock, ok := tryAcquireLock(l, "p")
	if !ok {
		t.Fatal("exclusive Lock did not acquire after RLock released")
	}
	unlock()
}

// TestLocker_TryLock asserts TryLock returns false while an exclusive lock is
// held and true after release, and false while we hold it from a prior
// TryLock.
func TestLocker_TryLock(t *testing.T) {
	l := New()
	unlock := l.Lock("p")

	if _, ok := l.TryLock("p"); ok {
		t.Fatal("TryLock succeeded while exclusive Lock held")
	}

	unlock()

	rel, ok := l.TryLock("p")
	if !ok {
		t.Fatal("TryLock failed after release")
	}
	if _, ok2 := l.TryLock("p"); ok2 {
		t.Fatal("TryLock succeeded while we hold the lock from a prior TryLock")
	}
	rel()
}

// TestLocker_TryLockFalseWhileReaderHolds asserts TryLock returns false while
// a reader holds the lock (exclusive must be excluded by any reader).
func TestLocker_TryLockFalseWhileReaderHolds(t *testing.T) {
	l := New()
	runlock := l.RLock("p")
	defer runlock()

	if _, ok := l.TryLock("p"); ok {
		t.Fatal("TryLock succeeded while a reader holds the lock")
	}
}

// TestLocker_EmptyKeyNoOps asserts empty-key calls return no-op unlocks and
// never block.
func TestLocker_EmptyKeyNoOps(t *testing.T) {
	l := New()

	// Two exclusive empty-key locks must both "acquire" (no-ops).
	u1 := l.Lock("")
	u2 := l.Lock("")
	u1()
	u2()

	r1 := l.RLock("")
	r1()

	rel, ok := l.TryLock("")
	if !ok {
		t.Fatal("TryLock(\"\") should be a no-op that returns ok=true")
	}
	rel()
	// All returned closures must be safe to call (no panic).
}

// TestLocker_ConcurrentStress runs many goroutines contending the same
// exclusive lock and asserts a counter protected only by the lock never sees
// a torn increment (race-detector + invariant check).
func TestLocker_ConcurrentStress(t *testing.T) {
	l := New()
	var counter int
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			unlock := l.Lock("shared")
			counter++
			unlock()
		}()
	}
	wg.Wait()
	if counter != goroutines {
		t.Fatalf("counter = %d, want %d — exclusive lock failed to serialize", counter, goroutines)
	}
}

// TestLocker_SameKeySameMutex asserts repeated calls for the same key reuse
// the same underlying mutex (so an exclusive holder blocks a second
// acquisition obtained via a fresh call).
func TestLocker_SameKeySameMutex(t *testing.T) {
	l := New()
	unlock := l.Lock("p")
	defer unlock()
	if _, ok := l.TryLock("p"); ok {
		t.Fatal("TryLock on same key succeeded while Lock held — keys did not share a mutex")
	}
}
