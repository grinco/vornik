package membench

import (
	"os"
	"path/filepath"
	"testing"
)

// LongMemEval's `answer` is not always a string.
//
// In longmemeval-cleaned (v1-cleaned, Sept 2025) 32 of 500 items answer with a
// bare JSON number — counting questions, all in the multi-session ability:
// `"answer": 3`. Typed as `string`, encoding/json refuses the whole file, so the
// loader could not read the real dataset at all: "cannot unmarshal number into Go
// struct field lmeItem.answer of type string".
//
// This is the sharp edge of a strict decoder on someone else's schema. The failure
// is at least loud — the alternative, `any`, would have produced `float64(3)`
// rendering as "3" via %v today and could silently become "3e+00" under a
// different formatter, with a judge then marking every counting question wrong for
// a formatting reason invisible in the results.

func TestLongMemEval_AcceptsNumericAnswers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lme.json")
	// Two items: one string answer, one bare number. Both must load.
	if err := os.WriteFile(path, []byte(`[
	  {"question_id":"q1","question_type":"multi-session","question":"how many?",
	   "answer": 3,
	   "haystack_session_ids":["s1"],"haystack_dates":["2026/05/01 (Fri) 10:00"],
	   "haystack_sessions":[[{"role":"user","content":"three of them","has_answer":true}]]},
	  {"question_id":"q2","question_type":"single-session-user","question":"which city?",
	   "answer": "Brno",
	   "haystack_session_ids":["s1"],"haystack_dates":["2026/05/02 (Sat) 10:00"],
	   "haystack_sessions":[[{"role":"user","content":"I live in Brno","has_answer":true}]]}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}

	items, err := LongMemEval{}.Load(path, Limits{})
	if err != nil {
		t.Fatalf("a numeric answer broke the whole file: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("loaded %d items, want 2", len(items))
	}

	byID := map[string]BenchItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	// The number must arrive as the digits a judge can compare, not as a float
	// rendering. "3" is gradeable; "3e+00" would be marked wrong for a reason
	// that never appears in the results.
	if got := byID["q1"].QAs[0].GoldAnswer; got != "3" {
		t.Errorf("numeric gold answer = %q, want %q", got, "3")
	}
	if got := byID["q2"].QAs[0].GoldAnswer; got != "Brno" {
		t.Errorf("string gold answer = %q, want %q", got, "Brno")
	}
}

// TestLongMemEval_LoadsTheRealCleanedDataset is the assertion the unit test above
// cannot make: that our reading of the schema matches the actual published file.
// Skipped when the haystack is absent, since it is 15 MB and gitignored.
func TestLongMemEval_LoadsTheRealCleanedDataset(t *testing.T) {
	const path = "../../bench/longmemeval/longmemeval_oracle.json"
	if _, err := os.Stat(path); err != nil {
		t.Skip("longmemeval_oracle.json not present (gitignored; fetch from " +
			"huggingface.co/datasets/xiaowu0162/longmemeval-cleaned)")
	}

	items, err := LongMemEval{}.Load(path, Limits{})
	if err != nil {
		t.Fatalf("the real cleaned dataset does not load: %v", err)
	}
	if len(items) != 500 {
		t.Errorf("loaded %d items, want 500", len(items))
	}

	// 479 of 500 carry a has_answer turn. The 21 that do not are ALL abstention
	// items (`_abs`), where the answer is deliberately absent from the haystack —
	// so an empty gold set is correct there, not a labelling bug. Tier-2 cannot
	// measure abstention at all: there is nothing to retrieve, ContextRecall
	// returns NaN, and the item is excluded from the mean rather than scored zero.
	labelled, unlabelled := 0, 0
	for _, it := range items {
		if len(it.QAs) == 0 {
			t.Fatalf("item %s has no QA", it.ID)
		}
		if len(it.QAs[0].GoldDocumentIDs) > 0 {
			labelled++
		} else {
			unlabelled++
		}
	}
	if labelled != 479 || unlabelled != 21 {
		t.Errorf("labelled/unlabelled = %d/%d, want 479/21 — a change here means the "+
			"published revision moved, and results are not comparable across revisions",
			labelled, unlabelled)
	}

	// Gold documents must be question-scoped: session ids repeat across items, so
	// a bare id would cross-attribute one question's gold to another's haystack.
	for _, it := range items {
		for _, gold := range it.QAs[0].GoldDocumentIDs {
			if len(gold) <= len(it.ID) || gold[:len(it.ID)] != it.ID {
				t.Fatalf("item %s has gold document %q not scoped to its question id",
					it.ID, gold)
			}
		}
	}
}
