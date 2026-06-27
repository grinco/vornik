package cli

import (
	"reflect"
	"testing"
)

// TestSplitMemoryEvictChunks pins the CSV-parsing helper that takes
// --chunks "id1, id2 , id3" and yields a clean []string. Adjacent
// commas and pure-whitespace fragments drop; surrounding whitespace
// trims. Operators paste IDs from clipboards / spreadsheets / chat
// transcripts and the parser has to absorb the variation without
// silently corrupting an ID.
func TestSplitMemoryEvictChunks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{}},
		{"single", "chunk_1", []string{"chunk_1"}},
		{"two clean", "a,b", []string{"a", "b"}},
		{"whitespace around", " a , b , c ", []string{"a", "b", "c"}},
		{"trailing comma", "a,b,", []string{"a", "b"}},
		{"empty fragment", "a,,b", []string{"a", "b"}},
		{"all whitespace", " , , ", []string{}},
		{"newline mixed", "a,\nb,\tc", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitMemoryEvictChunks(tc.in)
			// Normalise nil-vs-empty since the helper returns
			// []string{} on the empty path and the test cases
			// declare {}.
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("split(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCurrentOperatorIdentity_NonEmpty — `evicted_by` must land
// something rather than empty so the audit row is meaningful.
// We don't pin a specific username (test environments vary) —
// just that the function returns a non-empty string.
func TestCurrentOperatorIdentity_NonEmpty(t *testing.T) {
	got := currentOperatorIdentity()
	if got == "" {
		t.Error("currentOperatorIdentity returned empty string")
	}
}
