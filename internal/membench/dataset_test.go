package membench

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Dataset loaders (design §5.4). Fixtures are trimmed real-shape files in
// testdata; the full datasets are fetched and hash-pinned, never committed.

func fixture(name string) string { return filepath.Join("testdata", name) }

// TestLongMemEval_LoadsItemsAndCategories — the basic shape.
func TestLongMemEval_LoadsItemsAndCategories(t *testing.T) {
	items, err := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("loaded %d items, want 2", len(items))
	}
	if items[0].ID != "q0001" || items[0].Category != "multi-session" {
		t.Errorf("item 0 = %s/%s, want q0001/multi-session", items[0].ID, items[0].Category)
	}
	if items[1].Category != "temporal-reasoning" {
		t.Errorf("item 1 category = %s, want temporal-reasoning", items[1].Category)
	}
}

// TestLongMemEval_GoldDocumentsFromHasAnswer is the load-bearing derivation.
// LongMemEval marks has_answer per TURN; the gold DOCUMENT is the session that
// turn belongs to. Getting this wrong makes every tier-2 metric score zero for a
// reason unrelated to retrieval — the failure mode that would make a green
// harness meaningless.
func TestLongMemEval_GoldDocumentsFromHasAnswer(t *testing.T) {
	items, err := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	qa := items[0].QAs[0]

	// Sessions s1 and s3 carry an answer turn; s2 is a pure distractor.
	want := map[string]bool{"q0001_s1": true, "q0001_s3": true}
	if len(qa.GoldDocumentIDs) != len(want) {
		t.Fatalf("gold documents = %v, want %v", qa.GoldDocumentIDs, want)
	}
	for _, id := range qa.GoldDocumentIDs {
		if !want[id] {
			t.Errorf("unexpected gold document %q — s2 has no has_answer turn and "+
				"must remain a distractor", id)
		}
	}
}

// TestLongMemEval_DocumentIDsAreQuestionScoped — every item has its own haystack
// and its own scope, but session ids repeat across items. An unscoped document id
// would collide and cross-attribute gold documents between items.
func TestLongMemEval_DocumentIDsAreQuestionScoped(t *testing.T) {
	items, _ := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{})
	for _, it := range items {
		for _, h := range it.Haystack {
			if !strings.HasPrefix(h.DocumentID, it.ID+"_") {
				t.Errorf("document id %q is not scoped to item %q; ids would collide "+
					"across items", h.DocumentID, it.ID)
			}
		}
	}
}

// TestLongMemEval_HaystackCarriesEventTime — the reason Phase 0 shipped first.
// Without parsed dates the temporal-reasoning and knowledge-update categories are
// unanswerable for a reason that is not retrieval quality.
func TestLongMemEval_HaystackCarriesEventTime(t *testing.T) {
	items, _ := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{})

	h := items[0].Haystack[0]
	if h.EventTime.IsZero() {
		t.Fatal("haystack session has no event time; the dated categories cannot be scored")
	}
	want := time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)
	if !h.EventTime.Equal(want) {
		t.Errorf("event time = %v, want %v (parsed from the 2023/05/14 (Sun) 09:30 form)",
			h.EventTime, want)
	}
}

// TestLongMemEval_HasAnswerStrippedFromContent — has_answer is the LABEL. Leaving
// it in the ingested text tells the system under test which session holds the
// answer, which is straightforwardly cheating.
func TestLongMemEval_HasAnswerStrippedFromContent(t *testing.T) {
	items, _ := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{})
	for _, it := range items {
		for _, h := range it.Haystack {
			if strings.Contains(h.Content, "has_answer") {
				t.Errorf("item %s session %s leaks has_answer into the ingested "+
					"content — the system under test would be told where the answer is",
					it.ID, h.DocumentID)
			}
		}
	}
}

// TestLongMemEval_ContextCarriesSessionDate — both systems must receive the same
// provenance framing, and the date has to be visible in-band for a system with no
// event-time concept of its own.
func TestLongMemEval_ContextCarriesSessionDate(t *testing.T) {
	items, _ := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{})
	ctx := items[0].Haystack[0].Context
	if !strings.Contains(ctx, "2023-05-14") {
		t.Errorf("context %q omits the session date; a system without an event-time "+
			"column could not answer a dated question at all", ctx)
	}
}

func TestLongMemEval_Limits(t *testing.T) {
	t.Run("max items", func(t *testing.T) {
		items, err := LongMemEval{}.Load(fixture("longmemeval_2items.json"), Limits{MaxItems: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Errorf("loaded %d items, want 1", len(items))
		}
	})
	t.Run("category filter", func(t *testing.T) {
		items, err := LongMemEval{}.Load(fixture("longmemeval_2items.json"),
			Limits{Category: "temporal-reasoning"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].ID != "q0002" {
			t.Errorf("category filter returned %d items (%v), want just q0002",
				len(items), itemIDs(items))
		}
	})
	t.Run("per category cap", func(t *testing.T) {
		items, err := LongMemEval{}.Load(fixture("longmemeval_2items.json"),
			Limits{MaxItemsPerCategory: 1})
		if err != nil {
			t.Fatal(err)
		}
		// Two distinct categories, one each.
		if len(items) != 2 {
			t.Errorf("loaded %d items, want 2 (one per category)", len(items))
		}
	})
	t.Run("unknown category yields nothing", func(t *testing.T) {
		items, err := LongMemEval{}.Load(fixture("longmemeval_2items.json"),
			Limits{Category: "no-such-category"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Errorf("unknown category returned %d items, want 0", len(items))
		}
	})
}

// TestLongMemEval_MissingFileErrors — a clear failure beats an empty run that
// looks like a dataset with no items.
func TestLongMemEval_MissingFileErrors(t *testing.T) {
	if _, err := (LongMemEval{}).Load(fixture("nope.json"), Limits{}); err == nil {
		t.Error("loading a missing dataset file reported success")
	}
}

// TestLoCoMo_LoadsSessionsAndEvidence — evidence ids are the gold labels here,
// mapped to the session that contains them.
func TestLoCoMo_LoadsSessionsAndEvidence(t *testing.T) {
	items, err := LoCoMo{}.Load(fixture("locomo_1item.json"), Limits{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("loaded %d items, want 1", len(items))
	}
	it := items[0]
	if len(it.Haystack) != 2 {
		t.Fatalf("loaded %d sessions, want 2", len(it.Haystack))
	}
	if it.Haystack[0].EventTime.IsZero() {
		t.Error("session has no event time; the LoCoMo date form was not parsed")
	}
	qa := it.QAs[0]
	if len(qa.GoldDocumentIDs) != 1 || !strings.Contains(qa.GoldDocumentIDs[0], "session_2") {
		t.Errorf("gold documents = %v, want the session containing evidence D2:1",
			qa.GoldDocumentIDs)
	}
}

// TestNative_LoadsGoldsetAgainstCorpus — the regression gate's dataset. Haystack
// comes from a real directory; the questions come from a version-controlled gold
// file.
func TestNative_LoadsGoldsetAgainstCorpus(t *testing.T) {
	ds := Native{CorpusDir: "testdata"}
	items, err := ds.Load(fixture("native_goldset.json"), Limits{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("loaded %d items, want 2", len(items))
	}
	if items[0].Category != "architecture" {
		t.Errorf("category = %q, want architecture", items[0].Category)
	}
	qa := items[0].QAs[0]
	if qa.GoldAnswer == "" {
		t.Error("gold answer is empty")
	}
	if len(qa.GoldDocumentIDs) != 1 {
		t.Errorf("gold documents = %v, want one", qa.GoldDocumentIDs)
	}
	// Every native item shares the same corpus haystack — the discrimination task
	// is over the whole doc set, not a per-question needle pile.
	if len(items[0].Haystack) == 0 {
		t.Error("native item has an empty haystack; the corpus directory was not read")
	}
}

// TestNative_MissingGoldsetErrors — the gold file is the gate's definition of
// correct; a missing one must not silently produce an empty benchmark.
func TestNative_MissingGoldsetErrors(t *testing.T) {
	ds := Native{CorpusDir: "testdata"}
	if _, err := ds.Load(fixture("no-goldset.json"), Limits{}); err == nil {
		t.Error("a missing gold set loaded without error")
	}
}

// TestDatasetNames — recorded in the manifest and in the comparability key, so
// they must be stable strings rather than incidental.
func TestDatasetNames(t *testing.T) {
	cases := map[string]Dataset{
		"longmemeval": LongMemEval{},
		"locomo":      LoCoMo{},
		"native":      Native{},
	}
	for want, ds := range cases {
		if got := ds.Name(); got != want {
			t.Errorf("Name() = %q, want %q", got, want)
		}
	}
}

func itemIDs(items []BenchItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}
