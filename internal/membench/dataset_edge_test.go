package membench

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Parser failure branches. These matter more than they look: a date parser that
// silently substitutes "now" for an unreadable value would make every haystack
// session look contemporaneous and quietly destroy the temporal categories, while
// still passing every happy-path test.

func TestParseLMEDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"full form", "2023/05/14 (Sun) 09:30", time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)},
		{"no weekday", "2023/05/14 09:30", time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)},
		{"date only", "2023/05/14", time.Date(2023, 5, 14, 0, 0, 0, 0, time.UTC)},
		{"wrong weekday tolerated", "2023/05/14 (Mon) 09:30", time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLMEDate(tc.in); !got.Equal(tc.want) {
				t.Errorf("parseLMEDate(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseLMEDate_UnparseableIsZeroNotNow is the invariant that protects the
// temporal categories. Zero persists as SQL NULL — "unknown" — whereas
// substituting the load time would make every session appear to have happened
// today.
func TestParseLMEDate_UnparseableIsZeroNotNow(t *testing.T) {
	for _, in := range []string{"", "not a date", "yesterday", "14 May 2023", "0000/00/00"} {
		got := parseLMEDate(in)
		if !got.IsZero() {
			t.Errorf("parseLMEDate(%q) = %v, want the zero time (unknown). A "+
				"substituted timestamp would make the session look contemporaneous.",
				in, got)
		}
	}
}

func TestParseLoCoMoDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"afternoon", "1:56 pm on 8 May, 2023", time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)},
		{"morning", "10:10 am on 20 May, 2023", time.Date(2023, 5, 20, 10, 10, 0, 0, time.UTC)},
		{"no comma", "9:05 am on 3 June 2024", time.Date(2024, 6, 3, 9, 5, 0, 0, time.UTC)},
		// Midnight and noon are where 12-hour clocks break implementations.
		{"noon", "12:00 pm on 1 July, 2024", time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)},
		{"midnight", "12:30 am on 1 July, 2024", time.Date(2024, 7, 1, 0, 30, 0, 0, time.UTC)},
		{"uppercase meridiem", "3:15 PM on 2 August, 2024", time.Date(2024, 8, 2, 15, 15, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLoCoMoDate(tc.in); !got.Equal(tc.want) {
				t.Errorf("parseLoCoMoDate(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseLoCoMoDate_UnparseableIsZero(t *testing.T) {
	for _, in := range []string{"", "sometime", "1:56 on 8 May, 2023", "1:56 pm on 8 Smarch, 2023"} {
		if got := parseLoCoMoDate(in); !got.IsZero() {
			t.Errorf("parseLoCoMoDate(%q) = %v, want zero", in, got)
		}
	}
}

func TestAtoiSafe(t *testing.T) {
	// A slice rather than a map because several cases deliberately carry
	// whitespace, which is exactly what atoiSafe must reject.
	cases := []struct {
		in   string
		want int
	}{
		{"12", 12},
		{"0", 0},
		{"007", 7},
		{"", 0},
		{"1a", 0},
		{"-1", 0},
		{"3 ", 0},
		{" 3", 0},
	}
	for _, tc := range cases {
		if got := atoiSafe(tc.in); got != tc.want {
			t.Errorf("atoiSafe(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMonthByName(t *testing.T) {
	if m, ok := monthByName("September"); !ok || m != time.September {
		t.Errorf("monthByName(September) = %v/%v", m, ok)
	}
	if m, ok := monthByName("sep"); !ok || m != time.September {
		t.Errorf("monthByName(sep) = %v/%v", m, ok)
	}
	for _, bad := range []string{"", "Sm", "Smarch", "xx"} {
		if _, ok := monthByName(bad); ok {
			t.Errorf("monthByName(%q) reported success", bad)
		}
	}
}

func TestSessionNumber(t *testing.T) {
	if got := sessionNumber("session_12"); got != 12 {
		t.Errorf("sessionNumber(session_12) = %d, want 12", got)
	}
	// A non-matching key sorts as 0 rather than panicking; sortedSessionKeys only
	// ever passes it keys the regex already matched, so this is the defensive path.
	if got := sessionNumber("not_a_session"); got != 0 {
		t.Errorf("sessionNumber(not_a_session) = %d, want 0", got)
	}
}

// TestLongMemEval_MalformedJSONErrors — a corrupt dataset must not read as empty.
func TestLongMemEval_MalformedJSONErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (LongMemEval{}).Load(path, Limits{}); err == nil {
		t.Error("malformed dataset JSON loaded without error")
	}
}

// TestLongMemEval_MismatchedArrayLengthsClip — the three parallel arrays are the
// dataset's shape. A short one is a corrupt file, and clipping is preferable to
// indexing out of range mid-run.
func TestLongMemEval_MismatchedArrayLengthsClip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.json")
	content := `[{"question_id":"q1","question_type":"multi-session","question":"?","answer":"a",
	  "haystack_session_ids":["s1","s2"],
	  "haystack_dates":["2023/01/01 00:00"],
	  "haystack_sessions":[[{"role":"user","content":"one"}],[{"role":"user","content":"two"}]]}]`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := (LongMemEval{}).Load(path, Limits{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(items[0].Haystack) != 1 {
		t.Errorf("loaded %d sessions, want 1 (clipped to the shortest array)",
			len(items[0].Haystack))
	}
}

// TestLongMemEval_ItemWithNoAnswerTurnHasNoGold — an item whose sessions carry no
// has_answer label yields empty gold, which makes its tier-2 metrics NaN rather
// than a spurious zero.
func TestLongMemEval_ItemWithNoAnswerTurnHasNoGold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nogold.json")
	content := `[{"question_id":"q1","question_type":"multi-session","question":"?","answer":"a",
	  "haystack_session_ids":["s1"],
	  "haystack_dates":["2023/01/01 00:00"],
	  "haystack_sessions":[[{"role":"user","content":"nothing relevant"}]]}]`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	items, _ := (LongMemEval{}).Load(path, Limits{})
	if len(items[0].QAs[0].GoldDocumentIDs) != 0 {
		t.Errorf("gold = %v, want empty so the metrics report NaN rather than 0",
			items[0].QAs[0].GoldDocumentIDs)
	}
}

// TestLoCoMo_MalformedJSONErrors — same contract as LongMemEval.
func TestLoCoMo_MalformedJSONErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (LoCoMo{}).Load(path, Limits{}); err == nil {
		t.Error("malformed LoCoMo JSON loaded without error")
	}
}

// TestNative_RequiresCorpusDir — a native dataset with no corpus has no haystack,
// which would score as total retrieval failure rather than a misconfiguration.
func TestNative_RequiresCorpusDir(t *testing.T) {
	if _, err := (Native{}).Load(fixture("native_goldset.json"), Limits{}); err == nil {
		t.Error("Native loaded with no CorpusDir; an empty haystack would look like " +
			"total retrieval failure instead of a config error")
	}
}

// TestNative_UnreadableCorpusDirErrors — likewise for a bad path.
func TestNative_UnreadableCorpusDirErrors(t *testing.T) {
	ds := Native{CorpusDir: filepath.Join(t.TempDir(), "does-not-exist")}
	if _, err := ds.Load(fixture("native_goldset.json"), Limits{}); err == nil {
		t.Error("Native loaded against a missing corpus directory")
	}
}

// TestNative_CorpusIsDeterministicallyOrdered — os.ReadDir order is not
// guaranteed across platforms, and an unstable haystack order would make ingest
// non-reproducible between runs on the same data.
func TestNative_CorpusIsDeterministicallyOrdered(t *testing.T) {
	ds := Native{CorpusDir: "testdata"}
	first, err := ds.Load(fixture("native_goldset.json"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := ds.Load(fixture("native_goldset.json"), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		for j := range first[0].Haystack {
			if first[0].Haystack[j].DocumentID != again[0].Haystack[j].DocumentID {
				t.Fatalf("corpus order is unstable: %q vs %q at %d",
					first[0].Haystack[j].DocumentID, again[0].Haystack[j].DocumentID, j)
			}
		}
	}
}

// TestNative_CorpusExcludesNonMarkdown — the gold set names markdown documents;
// pulling the JSON fixtures into the haystack would add noise the gold labels
// cannot refer to.
func TestNative_CorpusExcludesNonMarkdown(t *testing.T) {
	ds := Native{CorpusDir: "testdata"}
	items, err := ds.Load(fixture("native_goldset.json"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range items[0].Haystack {
		if filepath.Ext(h.DocumentID) != ".md" {
			t.Errorf("haystack contains a non-markdown document %q", h.DocumentID)
		}
	}
}
