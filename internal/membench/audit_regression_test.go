package membench

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestComparabilityKeyIncludesSingleSystem(t *testing.T) {
	a := ComparabilityFields{ObservedEmbedder: "m", ObservedRecallMethod: "vector", SingleSystem: true}
	b := a
	b.SingleSystem = false
	if a.Key() == b.Key() {
		t.Fatal("single-system and comparison runs produced the same key")
	}
}

func TestComparabilityPartialRequiresObservedRecallMethod(t *testing.T) {
	f := ComparabilityFields{ObservedEmbedder: "m", SingleSystem: true}
	if !f.Partial() {
		t.Fatal("a run with an unknown retrieval path was marked fully comparable")
	}
}

func TestCorpusDigestCoversContextAndEventTime(t *testing.T) {
	base := []Item{{DocumentID: "d", Content: "body", Context: "source A",
		EventTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	contextChanged := append([]Item(nil), base...)
	contextChanged[0].Context = "source B"
	timeChanged := append([]Item(nil), base...)
	timeChanged[0].EventTime = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if CorpusDigest(base) == CorpusDigest(contextChanged) {
		t.Fatal("context that changes ingested bytes did not change the corpus digest")
	}
	if CorpusDigest(base) == CorpusDigest(timeChanged) {
		t.Fatal("event time that changes temporal recall did not change the corpus digest")
	}
}

func TestDiffComparabilityPairsDetectsNamesAndLength(t *testing.T) {
	a := [][2]string{{"a", "same"}}
	b := [][2]string{{"b", "same"}, {"extra", "x"}}
	diffs := DiffComparabilityPairs(a, b)
	if len(diffs) != 2 {
		t.Fatalf("diffs = %v, want field-name and extra-field differences", diffs)
	}
}

func TestExternalDoJSONCapsResponseBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxExternalResponseBytes+1))
	}))
	defer srv.Close()
	e := NewExternalSystem(ExternalConfig{BaseURL: srv.URL, Client: srv.Client()})
	var out map[string]any
	err := e.doJSON(context.Background(), http.MethodGet, "/", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want bounded-response refusal", err)
	}
}
