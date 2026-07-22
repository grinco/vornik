package dispatcher

import (
	"sync"
	"testing"
)

// TestWebWriteTokenStore_PutTakeSingleUse — a Put'd token is returned exactly
// once by Take; the second Take reports absent (single-use redemption).
func TestWebWriteTokenStore_PutTakeSingleUse(t *testing.T) {
	s := NewWebWriteTokenStore()
	s.Put("sub-1", "tok-1")

	got, ok := s.Take("sub-1")
	if !ok || got != "tok-1" {
		t.Fatalf("first Take = (%q, %v); want (tok-1, true)", got, ok)
	}
	if got2, ok2 := s.Take("sub-1"); ok2 || got2 != "" {
		t.Fatalf("second Take = (%q, %v); want (\"\", false) — token must be single-use", got2, ok2)
	}
}

// TestWebWriteTokenStore_TakeMissing — Take for an unknown submission id reports
// absent without panicking.
func TestWebWriteTokenStore_TakeMissing(t *testing.T) {
	s := NewWebWriteTokenStore()
	if got, ok := s.Take("nope"); ok || got != "" {
		t.Fatalf("Take(missing) = (%q, %v); want (\"\", false)", got, ok)
	}
}

// TestWebWriteTokenStore_PutOverwrites — a re-approval overwrites the prior
// token; Take yields the latest.
func TestWebWriteTokenStore_PutOverwrites(t *testing.T) {
	s := NewWebWriteTokenStore()
	s.Put("sub-1", "old")
	s.Put("sub-1", "new")
	if got, ok := s.Take("sub-1"); !ok || got != "new" {
		t.Fatalf("Take after overwrite = (%q, %v); want (new, true)", got, ok)
	}
}

// TestWebWriteTokenStore_Delete — Delete evicts a token so a later Take misses.
func TestWebWriteTokenStore_Delete(t *testing.T) {
	s := NewWebWriteTokenStore()
	s.Put("sub-1", "tok")
	s.Delete("sub-1")
	if got, ok := s.Take("sub-1"); ok || got != "" {
		t.Fatalf("Take after Delete = (%q, %v); want (\"\", false)", got, ok)
	}
}

// TestWebWriteTokenStore_NilAndEmptySafe — nil receiver and empty ids are no-ops
// rather than panics, so callers need not guard.
func TestWebWriteTokenStore_NilAndEmptySafe(t *testing.T) {
	var s *WebWriteTokenStore
	s.Put("x", "y") // must not panic
	if got, ok := s.Take("x"); ok || got != "" {
		t.Fatalf("nil Take = (%q, %v); want (\"\", false)", got, ok)
	}
	s.Delete("x") // must not panic

	s2 := NewWebWriteTokenStore()
	s2.Put("", "tok") // empty id — no-op
	if got, ok := s2.Take(""); ok || got != "" {
		t.Fatalf("empty-id Take = (%q, %v); want (\"\", false)", got, ok)
	}
}

// TestWebWriteTokenStore_ConcurrentPutTake — concurrent Puts/Takes are race-free
// and each token is redeemed at most once (run with -race).
// runs under -race; there is nothing to assert beyond "no data race / no panic",
// so the *testing.T is intentionally unused.
func TestWebWriteTokenStore_ConcurrentPutTake(_ *testing.T) {
	s := NewWebWriteTokenStore()
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := string(rune('a'+(i%26))) + string(rune('0'+(i%10)))
		wg.Add(2)
		go func() { defer wg.Done(); s.Put(id, "tok") }()
		go func() { defer wg.Done(); s.Take(id) }()
	}
	wg.Wait()
}
