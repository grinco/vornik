package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// TestWarnOnAnonymousAttribution_ConcurrentInit_NoRace is the
// regression for the 2026-06-04 bug sweep: the rate-limit warners
// (anonAttrWarner / legacyHeaderShadowedWarner) were lazily created
// with an unsynchronised check-then-set on *Server fields. net/http
// dispatches handlers concurrently, so two anonymous proxy requests
// racing the first-time init both wrote the pointer (lost update) and
// raced a concurrent read — flagged by -race, and on weakly-ordered
// arches a torn pointer could nil-deref. The sync.Once guard fixes it.
//
// Run under `go test -race` (the default for make test-unit).
func TestWarnOnAnonymousAttribution_ConcurrentInit_NoRace(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))

	const goroutines = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/api/chat", nil)
			req.Header.Set("User-Agent", fmt.Sprintf("client-%d", i))
			s.warnOnAnonymousAttribution(req, AttributionAnonymous)
		}(i)
	}
	close(start)
	wg.Wait()

	if s.anonAttrWarner == nil {
		t.Fatal("anonAttrWarner should be initialised after the calls")
	}
}

// TestMaybeWarnLegacyHeaderShadowed_ConcurrentInit_NoRace mirrors the
// above for the sibling warner.
func TestMaybeWarnLegacyHeaderShadowed_ConcurrentInit_NoRace(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))

	const goroutines = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/api/chat", nil)
			req.Header.Set("User-Agent", fmt.Sprintf("client-%d", i))
			// Both the DB-key context and the legacy header must be
			// present for the shadow warn (and its lazy init) to fire.
			req = req.WithContext(context.WithValue(req.Context(), projectIDFromKeyKey, "db-proj"))
			req.Header.Set("X-Vornik-Project-ID", "hdr-proj")
			s.maybeWarnLegacyHeaderShadowed(nil, req)
		}(i)
	}
	close(start)
	wg.Wait()

	if s.legacyHeaderShadowedWarner == nil {
		t.Fatal("legacyHeaderShadowedWarner should be initialised after the calls")
	}
}
