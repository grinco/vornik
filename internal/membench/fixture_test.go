package membench

import (
	"os"
	"path/filepath"
	"testing"
)

// The committed retrieval-gate fixture is DATA, and data can be wrong in ways
// that look exactly like a working run.
//
// On 2026-08-12 the first version of this fixture named its labels
// gold_document_ids and stripped the .md extension. The loader reads
// gold_documents and DocumentID is the corpus filename WITH .md, so every item
// loaded with no labels. The run completed, retrieved the right documents in the
// right order, and reported `scored: 0` with every metric at 0 — which reads like
// broken retrieval and was actually missing labels.
//
// The harness behaved correctly: ContextRecall returns NaN for an unlabelled item
// precisely so "no labels" cannot be confused with "retrieved nothing". What was
// missing was any check on the fixture before a run consumed it. This is it.

const fixtureDir = "../../bench/fixtures/retrieval-gate"

func TestRetrievalGateFixture_LabelsLoad(t *testing.T) {
	items, err := Native{CorpusDir: filepath.Join(fixtureDir, "corpus")}.
		Load(filepath.Join(fixtureDir, "goldset.json"), Limits{})
	if err != nil {
		t.Fatalf("the committed fixture does not load: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("fixture loaded zero items")
	}

	for _, it := range items {
		for _, qa := range it.QAs {
			if len(qa.GoldDocumentIDs) == 0 {
				t.Errorf("item %s has NO gold documents. The field is `gold_documents` — "+
					"`gold_document_ids` parses silently to nothing, and the run then reports "+
					"scored:0 with all metrics at 0, which reads like broken retrieval",
					it.ID)
			}
		}
	}
}

// TestRetrievalGateFixture_GoldNamesExistInTheCorpus catches the other half: a
// label that loads but names a document the corpus does not contain scores zero
// recall forever, and zero recall on a fixture is indistinguishable from a
// retrieval regression — which is the exact signal the gate is built to trust.
func TestRetrievalGateFixture_GoldNamesExistInTheCorpus(t *testing.T) {
	corpus := filepath.Join(fixtureDir, "corpus")
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatalf("read fixture corpus: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			present[e.Name()] = true
		}
	}
	if len(present) == 0 {
		t.Fatal("fixture corpus is empty")
	}

	items, err := Native{CorpusDir: corpus}.Load(filepath.Join(fixtureDir, "goldset.json"), Limits{})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	for _, it := range items {
		for _, qa := range it.QAs {
			for _, gold := range qa.GoldDocumentIDs {
				if !present[gold] {
					t.Errorf("item %s names gold document %q, which is not in the fixture "+
						"corpus. DocumentID is the FILENAME including .md (Native.loadCorpus "+
						"uses e.Name()), so a stripped extension never matches",
						it.ID, gold)
				}
			}
		}
	}
}

// TestRetrievalGateFixture_ScoresPerfectlyOnAnOracle proves the labels and the
// scorer agree: a system that returns exactly the gold documents must score 1.0.
//
// Without this, a fixture could load, name real files, and still be mis-shaped in
// some way that makes a perfect retrieval score less than perfect — and a gate
// whose ceiling is not 1.0 cannot tell "retrieval regressed" from "the fixture was
// always like that".
func TestRetrievalGateFixture_ScoresPerfectlyOnAnOracle(t *testing.T) {
	items, err := Native{CorpusDir: filepath.Join(fixtureDir, "corpus")}.
		Load(filepath.Join(fixtureDir, "goldset.json"), Limits{})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	for _, it := range items {
		for _, qa := range it.QAs {
			// The oracle: retrieve precisely the gold documents, in order.
			if got := ContextRecall(qa.GoldDocumentIDs, qa.GoldDocumentIDs); got != 1.0 {
				t.Errorf("item %s: an oracle scored recall %v, want 1.0", it.ID, got)
			}
			if got := MRR(qa.GoldDocumentIDs, qa.GoldDocumentIDs); got != 1.0 {
				t.Errorf("item %s: an oracle scored MRR %v, want 1.0", it.ID, got)
			}
		}
	}
}
