package configassist

import (
	"strings"
	"testing"
	"time"
)

// Regression: audit 2026-09-15 CA-02 — "Cyclic YAML Can Cause a Fatal Stack
// Overflow". flattenNode followed yaml.AliasNode.Alias with no cycle check
// and no expansion bound, so a self-referential alias recursed until the Go
// runtime killed the process. A fatal stack overflow is NOT a recoverable
// panic: the chat door's recover() cannot contain it, and the class ceiling
// runs AFTER flattening, so class E does not contain it either.
//
// The test asserts termination. It cannot assert "does not stack overflow"
// from inside the same process — an overflow takes the test binary with it —
// so the guard is that this test returns at all, plus the explicit bound
// assertions below.
func TestFlatten_CyclicAliasTerminates(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"self-referential sequence", "description: &loop [*loop]\n"},
		{"self-referential mapping", "a: &loop\n  b: *loop\n"},
		{"mutual recursion", "a: &x\n  p: *y\nb: &y\n  q: *x\n"},
		{"alias at the document root", "&root\na: *root\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan map[string]string, 1)
			go func() {
				defer func() {
					// A recoverable panic must not escape either.
					if r := recover(); r != nil {
						t.Errorf("Flatten panicked on %s: %v", tc.name, r)
						done <- nil
					}
				}()
				done <- Flatten([]byte(tc.doc))
			}()
			select {
			case out := <-done:
				// The document is hostile, not empty: it must be VISIBLE to
				// the classifier, never silently flattened to nothing (an
				// empty map would read as "no changes" and classify as A).
				if len(out) == 0 {
					t.Fatalf("%s: a cyclic document must yield a marker key, got an empty map", tc.name)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: Flatten did not terminate", tc.name)
			}
		})
	}
}

// The bound must be enforced on NON-cyclic but explosively nested aliases
// too — the YAML "billion laughs" shape, where each level is finite but the
// expansion is exponential.
func TestFlatten_AliasExpansionIsBounded(t *testing.T) {
	doc := `
a: &a ["x","x","x","x","x","x","x","x","x"]
b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]
e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]
f: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e]
g: [*f,*f,*f,*f,*f,*f,*f,*f,*f]
`
	done := make(chan map[string]string, 1)
	go func() { done <- Flatten([]byte(doc)) }()
	select {
	case out := <-done:
		if len(out) > maxFlattenKeys+8 {
			t.Fatalf("expansion is unbounded: produced %d keys", len(out))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Flatten did not terminate on an exponentially expanding document")
	}
}

// A cyclic document must reach the classifier as UNCLASSIFIABLE, so the
// deny-by-default rule (design §6.1) makes it class E rather than a
// no-op bundle.
func TestClassify_CyclicDocumentIsE(t *testing.T) {
	out := Flatten([]byte("description: &loop [*loop]\n"))
	found := ""
	for k := range out {
		if strings.HasPrefix(k, "<") {
			found = k
		}
	}
	if found == "" {
		t.Fatalf("a cyclic document needs a marker key the classifier can refuse, got %v", out)
	}
	bc := ClassifyChanges([]Change{{File: "projects/p.yaml", Key: found, Before: "", After: out[found]}})
	if bc.Class != ClassE {
		t.Fatalf("a document the flattener could not expand must be E, got %s: %v", bc.Class, bc.Reasons())
	}
}
