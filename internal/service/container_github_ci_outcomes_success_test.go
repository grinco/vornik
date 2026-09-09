package service

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// The success trigger learns from the workflow's OWN steps whether it can run
// on a build with no pull request (headmatch task_20260909150605_13cc14a3d079dc09,
// 2026-09-09: success_workflow_id named the review workflow, a green push to
// main had no PR, forge.fetch_diff refused it three times).
func TestSuccessWorkflowNeedsChangeRequest(t *testing.T) {
	wfs := map[string]*registry.Workflow{
		"github-review": {Steps: map[string]registry.WorkflowStep{
			"fetch_diff": {Type: "system", Handler: "forge.fetch_diff"},
			"fetch_ci":   {Type: "system", Handler: "forge.fetch_ci"},
			"review":     {Type: "agent", Role: "reviewer"},
			"post":       {Type: "system", Handler: "forge.post_review"},
		}},
		"rag-deposit": {Steps: map[string]registry.WorkflowStep{
			"fetch_ci": {Type: "system", Handler: "forge.fetch_ci"},
			"ingest":   {Type: "system", Handler: "rag.ingest"},
		}},
	}
	lookup := func(id string) *registry.Workflow { return wfs[id] }

	if !successWorkflowNeedsChangeRequest(lookup, "github-review") {
		t.Error("the review workflow needs a pull request")
	}
	if successWorkflowNeedsChangeRequest(lookup, "rag-deposit") {
		t.Error("a deposit that only reads CI must keep firing on merged main")
	}
	if successWorkflowNeedsChangeRequest(lookup, "no-such-workflow") {
		t.Error("an unknown workflow is not this check's finding; the executor reports it")
	}
	if successWorkflowNeedsChangeRequest(lookup, "") || successWorkflowNeedsChangeRequest(nil, "github-review") {
		t.Error("no workflow id, or no lookup, needs nothing")
	}
	if (&Container{}).workflowLookup()("github-review") != nil {
		t.Error("a container without a registry must answer nil, not panic")
	}
}
