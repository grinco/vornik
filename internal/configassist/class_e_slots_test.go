package configassist

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Config-assistant design §13.9a. The cap counted exhaustively and re-counted
// after the insert, withdrawing a row that landed over it (CA-17). That bounds
// the overshoot and does not prevent one — concurrent requests across projects
// or replicas each pass the pre-flight before any of them files — and the
// repair has a hole of its own: SetStatus can fail AFTER the insert, leaving an
// actionable DRAFT while the response says it was withdrawn.
//
// The UNIQUE constraint is now the cap. These tests pin the admission rule, not
// the storage: the fake below behaves exactly as the PRIMARY KEY does.

// fakeSlots is an in-memory stand-in for the reservation table, with the same
// contract: a taken slot returns ErrDuplicateKey and nothing else.
type fakeSlots struct {
	mu    sync.Mutex
	taken map[string]bool
	fail  error
}

func newFakeSlots() *fakeSlots { return &fakeSlots{taken: map[string]bool{}} }

func (f *fakeSlots) Reserve(_ context.Context, credentialID, day string, slot int) error {
	if f.fail != nil {
		return f.fail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := credentialID + "|" + day + "|" + string(rune('0'+slot))
	if f.taken[key] {
		return persistence.ErrDuplicateKey
	}
	f.taken[key] = true
	return nil
}

func (f *fakeSlots) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.taken)
}

func TestReserveClassESlot_AdmitsUpToTheCapAndThenRefuses(t *testing.T) {
	slots := newFakeSlots()
	e := &Engine{Slots: slots}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	for i := 1; i <= 3; i++ {
		if r := e.reserveClassESlot(context.Background(), "cred-1", 3, 0, now); r != nil {
			t.Fatalf("admission %d refused: %s", i, r.Message)
		}
	}
	r := e.reserveClassESlot(context.Background(), "cred-1", 3, 0, now)
	if r == nil {
		t.Fatal("the fourth admission was allowed past a cap of 3")
	}
	if r.Code != RefuseClassECap {
		t.Fatalf("want RefuseClassECap, got %s", r.Code)
	}
	if slots.count() != 3 {
		t.Fatalf("want exactly 3 slots consumed, got %d", slots.count())
	}
}

// The hint is a HINT. A stale one must cost a probe, not an admission: the
// whole point of moving the decision into the constraint is that a wrong count
// can no longer admit or refuse on its own.
func TestReserveClassESlot_StaleHintStillAdmitsWhileSlotsRemain(t *testing.T) {
	slots := newFakeSlots()
	e := &Engine{Slots: slots}
	now := time.Now().UTC()

	// Slot 1 is already taken by someone else; the hint says "nothing filed".
	if err := slots.Reserve(context.Background(), "cred-1", now.Format("2006-01-02"), 1); err != nil {
		t.Fatal(err)
	}
	if r := e.reserveClassESlot(context.Background(), "cred-1", 3, 0, now); r != nil {
		t.Fatalf("a stale hint refused an admission that had slots left: %s", r.Message)
	}
}

// Different credentials share no slots — the cap is per credential per day.
func TestReserveClassESlot_IsPerCredential(t *testing.T) {
	slots := newFakeSlots()
	e := &Engine{Slots: slots}
	now := time.Now().UTC()

	for i := 0; i < 2; i++ {
		if r := e.reserveClassESlot(context.Background(), "cred-1", 2, 0, now); r != nil {
			t.Fatalf("cred-1 admission %d refused: %s", i, r.Message)
		}
	}
	if r := e.reserveClassESlot(context.Background(), "cred-2", 2, 0, now); r != nil {
		t.Fatalf("cred-2 was refused by cred-1's usage: %s", r.Message)
	}
}

// A different UTC day is a different bucket, and the day boundary is the only
// thing that frees a slot.
func TestReserveClassESlot_IsPerUTCDay(t *testing.T) {
	slots := newFakeSlots()
	e := &Engine{Slots: slots}
	day1 := time.Date(2026, 9, 19, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)

	if r := e.reserveClassESlot(context.Background(), "cred-1", 1, 0, day1); r != nil {
		t.Fatalf("day 1 refused: %s", r.Message)
	}
	if r := e.reserveClassESlot(context.Background(), "cred-1", 1, 0, day1); r == nil {
		t.Fatal("day 1 admitted twice under a cap of 1")
	}
	if r := e.reserveClassESlot(context.Background(), "cred-1", 1, 0, day2); r != nil {
		t.Fatalf("the day roll-over did not free the slot: %s", r.Message)
	}
}

// Concurrency is the whole reason this exists: N racers, cap C, exactly C
// admissions. Run under -race.
func TestReserveClassESlot_ConcurrentRacersNeverExceedTheCap(t *testing.T) {
	slots := newFakeSlots()
	e := &Engine{Slots: slots}
	now := time.Now().UTC()
	const dailyCap, racers = 4, 24

	var wg sync.WaitGroup
	admitted := make(chan struct{}, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.reserveClassESlot(context.Background(), "cred-1", dailyCap, 0, now); r == nil {
				admitted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(admitted)

	if got := len(admitted); got != dailyCap {
		t.Fatalf("want exactly %d admissions under a cap of %d, got %d", dailyCap, dailyCap, got)
	}
}

// An unreadable store must refuse. The cap bounds a leaked credential, so
// "cannot tell" is not permission.
func TestReserveClassESlot_StoreFailureRefuses(t *testing.T) {
	slots := newFakeSlots()
	slots.fail = errors.New("connection refused")
	e := &Engine{Slots: slots}

	r := e.reserveClassESlot(context.Background(), "cred-1", 3, 0, time.Now().UTC())
	if r == nil {
		t.Fatal("an unreadable reservation store admitted the request")
	}
	if r.Code != RefuseClassECap {
		t.Fatalf("want RefuseClassECap, got %s", r.Code)
	}
}

// An unattributed caller cannot be counted, so it must not be admitted under a
// per-credential quota either.
func TestReserveClassESlot_NoCredentialRefuses(t *testing.T) {
	e := &Engine{Slots: newFakeSlots()}
	if r := e.reserveClassESlot(context.Background(), "", 3, 0, time.Now().UTC()); r == nil {
		t.Fatal("a request with no credential was admitted under a per-credential cap")
	}
}

// No store wired: the cap cannot be enforced, and the deployment predates the
// table. Admit, exactly as a zero cap does — this is the upgrade path, not a
// bypass, and the pre-existing exhaustive count still runs beside it.
func TestReserveClassESlot_NoStoreIsNotAGate(t *testing.T) {
	e := &Engine{}
	if r := e.reserveClassESlot(context.Background(), "cred-1", 3, 0, time.Now().UTC()); r != nil {
		t.Fatalf("an unwired store became a refusal: %s", r.Message)
	}
}
