package memory

import (
	"strings"
	"testing"
)

func TestTokenSet(t *testing.T) {
	got := tokenSet("Go is great, Go is fast! go-runs-fine")
	for _, want := range []string{"great", "fast", "runs", "fine"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	// "go" is length 2 → excluded by the floor.
	if _, ok := got["go"]; ok {
		t.Errorf("'go' should be filtered (len<3)")
	}
	if _, ok := got["is"]; ok {
		t.Errorf("'is' should be filtered (len<3)")
	}
	// Empty input.
	if got := tokenSet(""); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	// Punctuation-only input.
	if got := tokenSet("!!! ... ???"); len(got) != 0 {
		t.Fatalf("punct: %v", got)
	}
}

func TestJaccard(t *testing.T) {
	a := tokenSet("alpha beta gamma")
	b := tokenSet("alpha beta gamma")
	if got := jaccard(a, b); got != 1.0 {
		t.Fatalf("identical: %v", got)
	}
	if got := jaccard(a, tokenSet("delta epsilon")); got != 0.0 {
		t.Fatalf("disjoint: %v", got)
	}
	// Empty maps.
	if got := jaccard(nil, a); got != 0 {
		t.Fatal("empty a")
	}
	if got := jaccard(a, nil); got != 0 {
		t.Fatal("empty b")
	}
	// Partial overlap: {a,b,c} vs {b,c,d} → 2/4 = 0.5.
	got := jaccard(tokenSet("alpha beta gamma"), tokenSet("beta gamma delta"))
	if got < 0.49 || got > 0.51 {
		t.Fatalf("partial: %v", got)
	}
	// Swap so the smaller-map branch is taken from the other side.
	got = jaccard(tokenSet("beta gamma delta epsilon zeta"), tokenSet("alpha beta gamma"))
	if got < 0.1 || got > 0.4 {
		t.Fatalf("unbalanced: %v", got)
	}
}

func TestApplyMMR_SmallInputsPassThrough(t *testing.T) {
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	out := applyMMR(in, 0.7)
	if len(out) != 2 || out[0].ChunkID != "a" {
		t.Fatalf("size-2 must be unchanged: %+v", out)
	}
}

func TestApplyMMR_DiversifiesNearDuplicates(t *testing.T) {
	// Three near-duplicates + one diverse result, in relevance order.
	in := []SearchResult{
		{ChunkID: "a", Content: "deploy script for production cluster"},
		{ChunkID: "b", Content: "deploy script for production cluster, copy"},
		{ChunkID: "c", Content: "deploy script for production cluster, another"},
		{ChunkID: "d", Content: "incident response runbook for paging on-call"},
	}
	out := applyMMR(in, 0.5) // moderate diversity weight
	// Top result stays "a" (always picked first).
	if out[0].ChunkID != "a" {
		t.Fatalf("first must be a: %+v", out)
	}
	// "d" should be lifted ahead of at least one of b/c.
	posD, posB, posC := indexOf(out, "d"), indexOf(out, "b"), indexOf(out, "c")
	if posD >= posB && posD >= posC {
		t.Fatalf("diverse result should be lifted: %+v", out)
	}
}

func TestApplyMMR_LambdaOneIsRelevanceOnly(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Content: "alpha"},
		{ChunkID: "b", Content: "alpha"},
		{ChunkID: "c", Content: "alpha"},
	}
	out := applyMMR(in, 1.0)
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if out[i].ChunkID != id {
			t.Fatalf("lambda=1 must preserve order: %+v", out)
		}
	}
}

func TestApplyMMR_LambdaClampedAndDefault(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Content: "alpha beta"},
		{ChunkID: "b", Content: "alpha gamma"},
		{ChunkID: "c", Content: "alpha delta"},
	}
	// lambda <= 0 → defaults to 0.7 internally.
	out := applyMMR(in, 0)
	if len(out) != 3 || out[0].ChunkID != "a" {
		t.Fatalf("default lambda: %+v", out)
	}
	// lambda > 1 → clamped to 1.
	out = applyMMR(in, 2.0)
	if out[0].ChunkID != "a" {
		t.Fatalf("clamp: %+v", out)
	}
}

func TestApplyMMR_PreservesAllResults(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Content: strings.Repeat("a ", 20)},
		{ChunkID: "b", Content: strings.Repeat("b ", 20)},
		{ChunkID: "c", Content: strings.Repeat("c ", 20)},
		{ChunkID: "d", Content: strings.Repeat("d ", 20)},
		{ChunkID: "e", Content: strings.Repeat("e ", 20)},
	}
	out := applyMMR(in, 0.7)
	if len(out) != 5 {
		t.Fatalf("lost results: %+v", out)
	}
	seen := make(map[string]bool)
	for _, r := range out {
		seen[r.ChunkID] = true
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !seen[id] {
			t.Errorf("missing %s", id)
		}
	}
}

func indexOf(in []SearchResult, id string) int {
	for i, r := range in {
		if r.ChunkID == id {
			return i
		}
	}
	return -1
}
