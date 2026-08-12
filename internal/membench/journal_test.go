package membench

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Journal and resume (design §5.8). Append-only JSONL, one line per item per
// phase, so a killed run costs only the in-flight item. This is also what makes
// ErrQuotaExhausted survivable: abort cleanly, resume when capacity returns.

func tempJournal(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "journal.jsonl")
}

// TestJournal_RecordsAndReplays — the basic contract.
func TestJournal_RecordsAndReplays(t *testing.T) {
	path := tempJournal(t)

	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	for _, e := range []JournalEntry{
		{ItemID: "q1", Phase: PhaseIngested},
		{ItemID: "q1", Phase: PhaseJudged, Outcome: OutcomeCorrect},
		{ItemID: "q2", Phase: PhaseIngested},
	} {
		if err := j.Record(e); err != nil {
			t.Fatalf("Record(%v): %v", e, err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if !done.Completed("q1") {
		t.Error("q1 reached PhaseJudged but is not reported complete")
	}
	if done.Completed("q2") {
		t.Error("q2 only reached PhaseIngested but is reported complete")
	}
	if done.Completed("q3") {
		t.Error("q3 was never recorded but is reported complete")
	}
}

// TestJournal_ResumeSkipsExactlyCompletedItems is the resume guarantee: a
// partial journal must skip the finished work and no more. Skipping too much
// silently shrinks the dataset and inflates nothing visibly.
func TestJournal_ResumeSkipsExactlyCompletedItems(t *testing.T) {
	path := tempJournal(t)
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	// q1 and q3 finished; q2 was interrupted mid-flight.
	entries := []JournalEntry{
		{ItemID: "q1", Phase: PhaseJudged, Outcome: OutcomeCorrect},
		{ItemID: "q2", Phase: PhaseRecalled},
		{ItemID: "q3", Phase: PhaseJudged, Outcome: OutcomeIncorrect},
	}
	for _, e := range entries {
		if err := j.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = j.Close()

	done, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}

	all := []string{"q1", "q2", "q3", "q4"}
	var todo []string
	for _, id := range all {
		if !done.Completed(id) {
			todo = append(todo, id)
		}
	}
	want := []string{"q2", "q4"}
	if len(todo) != len(want) {
		t.Fatalf("resume todo = %v, want %v", todo, want)
	}
	for i := range want {
		if todo[i] != want[i] {
			t.Errorf("resume todo = %v, want %v", todo, want)
			break
		}
	}
}

// TestJournal_OutcomesRecoverable — resuming must not lose the verdicts already
// earned, or a resumed run reports a smaller population than it actually judged.
func TestJournal_OutcomesRecoverable(t *testing.T) {
	path := tempJournal(t)
	j, _ := OpenJournal(path)
	_ = j.Record(JournalEntry{ItemID: "q1", Phase: PhaseJudged, Outcome: OutcomeCorrect, Category: "multi-session"})
	_ = j.Record(JournalEntry{ItemID: "q2", Phase: PhaseJudged, Outcome: OutcomeInvalid, Category: "multi-session"})
	_ = j.Close()

	done, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	counts := done.CountsByCategory()
	c := counts["multi-session"]
	if c.Correct != 1 || c.Invalid != 1 {
		t.Errorf("recovered counts = %+v, want 1 correct + 1 invalid", c)
	}
}

// TestJournal_AppendsAcrossReopen — resume appends rather than truncating.
// Truncating would silently discard the very work resume exists to preserve.
func TestJournal_AppendsAcrossReopen(t *testing.T) {
	path := tempJournal(t)

	j1, _ := OpenJournal(path)
	_ = j1.Record(JournalEntry{ItemID: "q1", Phase: PhaseJudged, Outcome: OutcomeCorrect})
	_ = j1.Close()

	j2, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = j2.Record(JournalEntry{ItemID: "q2", Phase: PhaseJudged, Outcome: OutcomeCorrect})
	_ = j2.Close()

	done, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if !done.Completed("q1") {
		t.Error("reopening the journal discarded the earlier run's work")
	}
	if !done.Completed("q2") {
		t.Error("the second run's work was not recorded")
	}
}

// TestLoadJournal_MissingFileIsEmptyNotError — a first run has no journal. That
// must not be an error, or --resume can never be the default-safe flag.
func TestLoadJournal_MissingFileIsEmptyNotError(t *testing.T) {
	done, err := LoadJournal(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("LoadJournal on a missing file errored: %v", err)
	}
	if done.Completed("anything") {
		t.Error("empty journal reported an item complete")
	}
}

// TestLoadJournal_TruncatedTrailingLineTolerated — a killed process can leave a
// half-written final line. Refusing to load would make a crash unrecoverable,
// which defeats the journal's whole purpose.
func TestLoadJournal_TruncatedTrailingLineTolerated(t *testing.T) {
	path := tempJournal(t)
	content := `{"item_id":"q1","phase":"judged","outcome":"correct"}` + "\n" +
		`{"item_id":"q2","phase":"jud`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	done, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("a truncated trailing line made the journal unloadable: %v", err)
	}
	if !done.Completed("q1") {
		t.Error("the intact line before the truncation was lost")
	}
	if done.Completed("q2") {
		t.Error("the truncated line was counted as complete")
	}
}

// TestErrQuotaExhausted_IsSentinel — the runner must be able to distinguish a
// quota stop from any other failure with errors.Is, because the handling is
// opposite: never retry, abort the run, and record it.
func TestErrQuotaExhausted_IsSentinel(t *testing.T) {
	wrapped := errors.Join(errors.New("http 429"), ErrQuotaExhausted)
	if !errors.Is(wrapped, ErrQuotaExhausted) {
		t.Error("ErrQuotaExhausted does not survive wrapping; the runner cannot " +
			"tell a quota stop from a transient fault")
	}
	if errors.Is(errors.New("some other failure"), ErrQuotaExhausted) {
		t.Error("an unrelated error matched ErrQuotaExhausted")
	}
}

// writeFile is a test helper kept here so types_edge_test.go and journal_test.go
// share one implementation.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
