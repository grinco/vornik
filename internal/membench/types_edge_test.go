package membench

import (
	"path/filepath"
	"testing"
)

// TestRecalled_SourceIDs_PreservesRankOrder — the tier-2 metrics take rank order
// as input (MRR depends on it entirely), so this must not reorder or dedupe.
func TestRecalled_SourceIDs_PreservesRankOrder(t *testing.T) {
	r := Recalled{Hits: []Hit{
		{SourceID: "d3", Score: 0.9},
		{SourceID: "d1", Score: 0.5},
		{SourceID: "d3", Score: 0.4}, // a second chunk from the same document
	}}
	got := r.SourceIDs()
	want := []string{"d3", "d1", "d3"}
	if len(got) != len(want) {
		t.Fatalf("SourceIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SourceIDs() = %v, want %v (rank order and duplicates must "+
				"survive — MRR and precision both depend on them)", got, want)
		}
	}
}

// TestRecalled_SourceIDs_Empty — no hits is an empty slice, not a nil-deref.
func TestRecalled_SourceIDs_Empty(t *testing.T) {
	if got := (Recalled{}).SourceIDs(); len(got) != 0 {
		t.Errorf("SourceIDs() on an empty recall = %v, want empty", got)
	}
}

func TestIngestStats_HaystackLoss(t *testing.T) {
	cases := []struct {
		name  string
		stats IngestStats
		want  float64
	}{
		{"nothing rejected", IngestStats{Bytes: 1000}, 0},
		{"half rejected", IngestStats{Bytes: 1000, RejectedBytes: 500}, 0.5},
		{"all rejected", IngestStats{Bytes: 1000, RejectedBytes: 1000}, 1.0},
		// Zero submitted bytes must not divide by zero — and an empty haystack
		// has lost nothing, so 0 is right rather than 1.
		{"nothing submitted", IngestStats{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stats.HaystackLoss(); got != tc.want {
				t.Errorf("HaystackLoss() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOutcomeCounts_AddIgnoresUnknown — an unrecognised outcome must not be
// silently filed as one of the four. Miscounting it as incorrect would blame
// retrieval for a harness bug.
func TestOutcomeCounts_AddIgnoresUnknown(t *testing.T) {
	var c OutcomeCounts
	c.Add(Outcome("something-new"))
	if c.Total() != 0 {
		t.Errorf("an unknown outcome was tallied: %+v", c)
	}
}

// TestOutcomeCounts_AddAllFour — every branch of the tally.
func TestOutcomeCounts_AddAllFour(t *testing.T) {
	var c OutcomeCounts
	for _, o := range []Outcome{OutcomeCorrect, OutcomeIncorrect, OutcomeInvalid, OutcomeError} {
		c.Add(o)
	}
	if c.Correct != 1 || c.Incorrect != 1 || c.Invalid != 1 || c.Error != 1 {
		t.Errorf("counts = %+v, want one of each", c)
	}
}

// TestJournal_NilReceiverIsSafe — the runner may hold a nil journal when
// journalling is off, and must not have to nil-check at every call site.
func TestJournal_NilReceiverIsSafe(t *testing.T) {
	var j *Journal
	if err := j.Record(JournalEntry{ItemID: "q1"}); err != nil {
		t.Errorf("Record on a nil journal errored: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Errorf("Close on a nil journal errored: %v", err)
	}
}

// TestOpenJournal_UnwritablePathErrors — a bad path must fail at open, before a
// run starts, rather than at the first Record when work is already in flight.
func TestOpenJournal_UnwritablePathErrors(t *testing.T) {
	// A path whose parent is a file, not a directory.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := writeFile(blocker, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(filepath.Join(blocker, "journal.jsonl")); err == nil {
		t.Error("OpenJournal accepted a path under a regular file")
	}
}

// TestReplay_ZeroValueIsUsable — a zero Replay (never loaded) must answer
// queries rather than panic, so a caller that skipped LoadJournal still works.
func TestReplay_ZeroValueIsUsable(t *testing.T) {
	var r Replay
	if r.Completed("q1") {
		t.Error("zero Replay reported an item complete")
	}
	if got := r.CountsByCategory(); got == nil {
		t.Error("zero Replay returned a nil map; callers would panic on write")
	}
}

// TestLoadJournal_CorruptMiddleLineIsAnError — only the TRAILING line is
// forgiven. Corruption in the middle means real damage, and silently skipping it
// would under-report the population a resumed run scores over.
func TestLoadJournal_CorruptMiddleLineIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	content := `{"item_id":"q1","phase":"judged","outcome":"correct"}` + "\n" +
		`{ this is not json` + "\n" +
		`{"item_id":"q3","phase":"judged","outcome":"correct"}` + "\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJournal(path); err == nil {
		t.Error("a corrupt middle line loaded without error; a resumed run would " +
			"silently score over a smaller population than it reports")
	}
}

// TestJournal_RecordAfterCloseErrors — writing to a closed journal must fail
// loudly. Silently dropping the entry is the worst outcome available: resume
// would then re-run work it believes unfinished, or worse, skip work it believes
// done because an earlier entry landed and a later one did not.
func TestJournal_RecordAfterCloseErrors(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "j.jsonl"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := j.Record(JournalEntry{ItemID: "q1", Phase: PhaseJudged}); err == nil {
		t.Error("Record on a closed journal reported success; a lost entry " +
			"corrupts resume silently")
	}
}

// TestJournal_DoubleCloseErrors — surfaced rather than swallowed, for the same
// reason: a Close that quietly fails may have left the tail unflushed.
func TestJournal_DoubleCloseErrors(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "j.jsonl"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := j.Close(); err == nil {
		t.Error("second Close reported success on an already-closed file")
	}
}
