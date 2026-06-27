package registry

import (
	"reflect"
	"testing"
)

// TestGateConditionPaths covers the gate-condition LHS extractor used
// by the workflow-gate schema compat check (item 11 of
// https://docs.vornik.io).
func TestGateConditionPaths(t *testing.T) {
	cases := []struct {
		name      string
		condition string
		want      []string
	}{
		{
			name:      "empty condition returns nil",
			condition: "",
			want:      nil,
		},
		{
			name:      "single equality",
			condition: "review.approved == true",
			want:      []string{"review.approved"},
		},
		{
			name:      "compound condition extracts both LHS paths",
			condition: "review.approved == true && review.all_done == false",
			want:      []string{"review.approved", "review.all_done"},
		},
		{
			name:      "string equality preserves dotted path",
			condition: `analysis.feature == "login"`,
			want:      []string{"analysis.feature"},
		},
		{
			name:      "trailing && (typo) is skipped, not flagged here",
			condition: "review.approved == true &&",
			want:      []string{"review.approved"},
		},
		{
			name:      "non-comparison terms are skipped, not flagged here",
			condition: "review.approved && review.all_done == true",
			want:      []string{"review.all_done"},
		},
		{
			name:      "deeply nested paths preserved",
			condition: "result.outer.inner.flag == true",
			want:      []string{"result.outer.inner.flag"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gateConditionPaths(tc.condition)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("gateConditionPaths(%q) = %v, want %v",
					tc.condition, got, tc.want)
			}
		})
	}
}

// TestDeclaresPath covers the schema-side half of the compat check.
// Producer/consumer asymmetry is the key invariant: a path is
// "declared" if the schema lists it under properties (anywhere in the
// nesting tree), regardless of whether it's in the required list.
func TestDeclaresPath(t *testing.T) {
	schema := &OutputSchema{
		Type:     "object",
		Required: []string{"review"},
		Properties: map[string]*OutputSchema{
			"review": {
				Type:     "object",
				Required: []string{"approved"},
				Properties: map[string]*OutputSchema{
					"approved": {Type: "bool"},
					// not required, but declared — gate can reference it.
					"all_done": {Type: "bool"},
				},
			},
		},
	}
	cases := []struct {
		path string
		want bool
	}{
		{"review", true},
		{"review.approved", true},
		{"review.all_done", true},         // declared, not required
		{"review.feedback", false},        // never declared
		{"review.approved.deeper", false}, // bool has no properties
		{"unrelated", false},
		{"", true}, // self
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := schema.DeclaresPath(tc.path)
			if got != tc.want {
				t.Errorf("DeclaresPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
	t.Run("nil schema rejects all paths", func(t *testing.T) {
		var s *OutputSchema
		if s.DeclaresPath("anything") {
			t.Error("nil schema declared a path")
		}
	})
}
