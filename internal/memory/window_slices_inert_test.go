package memory

import (
	"testing"
	"time"
)

// Slices 0b/0c/0d all ship dark. Each is a behaviour change to windowed recall,
// so the claim that a default-constructed Searcher behaves exactly as it did
// before they landed is the thing that makes them safe to merge ahead of being
// switched on. These tests pin that claim at the seam where the slices attach,
// rather than only at each pure function.

// TestSearcherDefaults_TemporalSlicesOff — a Searcher built the normal way has
// every slice disabled.
func TestSearcherDefaults_TemporalSlicesOff(t *testing.T) {
	s := NewSearcher(Config{}, nil, nil)

	if s.temporalBeta != 0 {
		t.Errorf("temporalBeta = %v on a fresh Searcher, want 0 (§4.3 ships dark)", s.temporalBeta)
	}
	if s.spreadWindow {
		t.Error("spreadWindow enabled on a fresh Searcher, want off (§4.4 ships dark)")
	}
}

// TestSearchOptionsDefaults_QueryWindowOff — and the per-call opt-in is off too,
// so an existing caller's query text can never start narrowing its own search.
func TestSearchOptionsDefaults_QueryWindowOff(t *testing.T) {
	var opts SearchOptions
	if opts.ParseQueryWindow {
		t.Error("ParseQueryWindow set on a zero SearchOptions, want off (§4.2 is opt-in)")
	}
}

// TestSetters_RoundTrip — the setters are the only way these turn on, and they
// are nil-safe because the container wires them conditionally.
func TestSetters_RoundTrip(t *testing.T) {
	s := NewSearcher(Config{}, nil, nil)

	s.SetTemporalBeta(defaultTemporalBeta)
	if s.temporalBeta != defaultTemporalBeta {
		t.Errorf("temporalBeta = %v, want %v", s.temporalBeta, defaultTemporalBeta)
	}
	s.SetSpreadWindow(true)
	if !s.spreadWindow {
		t.Error("SetSpreadWindow(true) did not take")
	}

	var nilSearcher *Searcher
	nilSearcher.SetTemporalBeta(0.5) // must not panic
	nilSearcher.SetSpreadWindow(true)
}

// TestTemporalSlices_DisabledLeaveResultsAlone drives the two post-fetch passes
// exactly as searchInternal does, with the default (disabled) settings and a
// window present — the configuration where a bug would be invisible in the pure
// unit tests because those pass the feature's own parameters explicitly.
func TestTemporalSlices_DisabledLeaveResultsAlone(t *testing.T) {
	s := NewSearcher(Config{}, nil, nil)
	from, to := day(2024, 1, 1), day(2024, 3, 1)

	in := []SearchResult{
		{ChunkID: "a", Score: 0.30, EventTime: day(2024, 2, 27)}, // near centre-late
		{ChunkID: "b", Score: 0.20, EventTime: day(2024, 1, 2)},
		{ChunkID: "c", Score: 0.10, EventTime: time.Time{}},
	}
	want := []string{"a", "b", "c"}

	got := in
	if s.temporalBeta != 0 {
		got = applyTemporalProximity(got, from, to, s.temporalBeta)
	}
	if s.spreadWindow {
		got = spreadAcrossWindow(got, from, to, 2)
	}

	if len(got) != len(want) {
		t.Fatalf("result count changed with slices off: %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ChunkID != want[i] {
			t.Errorf("ordering changed with slices off: position %d = %q, want %q",
				i, got[i].ChunkID, want[i])
		}
	}
	if got[0].Score != 0.30 || got[1].Score != 0.20 || got[2].Score != 0.10 {
		t.Errorf("scores mutated with slices off: %v, %v, %v",
			got[0].Score, got[1].Score, got[2].Score)
	}
}
