package persistence

import (
	"encoding/json"
	"testing"
)

func TestCheckpointOptionLabel(t *testing.T) {
	meta := json.RawMessage(`{"kind":"decision","question":"how?","options":[
		{"id":"retry_research_pathfix","label":"Retry from researcher with an explicit corrective hint."},
		{"id":"abort","label":"Abort with explanation."}]}`)

	cases := []struct {
		name   string
		meta   json.RawMessage
		choice string
		want   string
	}{
		{"match", meta, "retry_research_pathfix", "Retry from researcher with an explicit corrective hint."},
		{"match case-insensitive", meta, "ABORT", "Abort with explanation."},
		{"unknown id", meta, "nope", ""},
		{"empty choice", meta, "", ""},
		{"nil meta", nil, "abort", ""},
		{"malformed meta", json.RawMessage(`{not json`), "abort", ""},
		{"no options key", json.RawMessage(`{"question":"x"}`), "abort", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckpointOptionLabel(tc.meta, tc.choice); got != tc.want {
				t.Errorf("CheckpointOptionLabel(%q) = %q, want %q", tc.choice, got, tc.want)
			}
		})
	}
}
