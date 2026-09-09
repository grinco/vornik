package registry

import (
	"sort"
	"testing"
)

// SystemHandlers lists the handler names of a workflow's system steps, sorted,
// so a caller can ask what a workflow DOES without knowing forge (2026-09-09:
// forgeci uses it to learn whether a success-triggered workflow can run on a
// build that has no pull request).
func TestWorkflow_SystemHandlers(t *testing.T) {
	w := &Workflow{Steps: map[string]WorkflowStep{
		"fetch_diff": {Type: "system", Handler: "forge.fetch_diff"},
		"fetch_ci":   {Type: "system", Handler: "forge.fetch_ci"},
		"review":     {Type: "agent", Role: "reviewer"},
		"post":       {Type: "system", Handler: "forge.post_review"},
		"gate":       {Type: "gate"},
	}}
	got := w.SystemHandlers()
	want := []string{"forge.fetch_ci", "forge.fetch_diff", "forge.post_review"}
	if !sort.StringsAreSorted(got) || len(got) != len(want) {
		t.Fatalf("SystemHandlers = %v, want sorted %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SystemHandlers = %v, want %v", got, want)
		}
	}
	if (&Workflow{}).SystemHandlers() != nil {
		t.Error("a workflow with no steps has no system handlers")
	}
}
