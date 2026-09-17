package service

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
)

// Regression: 2026-09-16 stale-news incident.
//
// memory.SearchResult has carried EventTime since migration 157, and every
// wire shape above it dropped the field — so a caller using `recall` received
// content with no age whatsoever. Combined with a ranking that has no recency
// term (score is RRF × utility; the designed freshness_weight is unbuilt), an
// 11-day-old news digest could outrank today's AND arrive looking exactly
// like it. A model cannot caveat an age it was never told.
//
// THE SEAM: the repository was right, the memory layer was right, and the
// DTO conversion silently narrowed the record. A repository test passes
// either way — the assertion has to be on what the CALLER receives.
func TestMemorySearchResultToAPI_CarriesEventTimeOnBothPaths(t *testing.T) {
	when := time.Date(2026, 9, 5, 6, 1, 0, 0, time.UTC)
	in := []memory.SearchResult{{
		ChunkID: "c1", ProjectID: "assistant", SourceName: "czech-news.md",
		Content: "old headlines", Score: 0.9, EventTime: when,
		CreatedAt: when.Add(2 * time.Hour),
	}}

	// Routing OFF — the default path, and the one that dropped it.
	plain := memorySearchResultToAPI(in[0])
	if plain.EventTime != when.Format(time.RFC3339) {
		t.Fatalf("non-routing recall hit lost the event time: %+v", plain)
	}
	if plain.CreatedAt == "" {
		t.Fatalf("non-routing recall hit lost the ingest time: %+v", plain)
	}

	// Routing ON.
	routed := memorySearchResultToAPIRouting(in[0])
	if routed.EventTime != when.Format(time.RFC3339) {
		t.Fatalf("routing recall hit lost the event time: %+v", routed)
	}
}

// A chunk with no recorded event time must report NOTHING rather than the
// zero time (which renders as year 1 and reads as a bug) or now (which would
// be a lie about the content's age).
func TestMemorySearchResultToAPI_UnknownEventTimeStaysEmpty(t *testing.T) {
	// No event time, but a real ingest time: the caller must still get an
	// age, because event_time is nullable with no backfill.
	in := []memory.SearchResult{{ChunkID: "c1", Content: "x", CreatedAt: time.Date(2026, 9, 5, 6, 1, 0, 0, time.UTC)}}
	plain := memorySearchResultToAPI(in[0])
	if plain.EventTime != "" {
		t.Fatalf("an unknown event time must stay empty, got %q", plain.EventTime)
	}
	if plain.CreatedAt == "" {
		t.Fatal("a chunk with no event time must still report when it was ingested, or it has no age at all")
	}
	if routed := memorySearchResultToAPIRouting(in[0]); routed.EventTime != "" {
		t.Fatalf("an unknown event time must stay empty, got %q", routed.EventTime)
	}
}
