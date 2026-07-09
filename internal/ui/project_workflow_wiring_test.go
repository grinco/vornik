package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func wfResolver(ids ...string) func(string) *registry.Workflow {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func(id string) *registry.Workflow {
		if set[id] {
			return &registry.Workflow{ID: id}
		}
		return nil
	}
}

// TestBuildProjectWorkflowWiring_HeadmatchShape covers the real headmatch wiring:
// a webhook source routing labeled issues → issue-fix and opened PRs →
// github-review, plus a default workflow. All three resolve; each is a distinct
// panel, all open (≤3), with the right trigger labels.
func TestBuildProjectWorkflowWiring_HeadmatchShape(t *testing.T) {
	p := &registry.Project{
		ID:                "headmatch",
		DefaultWorkflowID: "dev-pipeline",
		Webhooks: registry.ProjectWebhooks{Sources: []registry.ProjectWebhookSource{{
			Name:                    "github",
			WorkflowID:              "issue-fix",
			ChangeRequestWorkflowID: "github-review",
		}}},
	}
	w := buildProjectWorkflowWiring(p, wfResolver("dev-pipeline", "issue-fix", "github-review"))

	wantTriggers := map[string]string{
		"issue labeled":              "issue-fix",
		"PR / change-request opened": "github-review",
		"default / manual submit":    "dev-pipeline",
	}
	if len(w.Triggers) != len(wantTriggers) {
		t.Fatalf("got %d triggers, want %d: %+v", len(w.Triggers), len(wantTriggers), w.Triggers)
	}
	for _, tr := range w.Triggers {
		if wantTriggers[tr.Label] != tr.WorkflowID {
			t.Errorf("trigger %q → %q, want %q", tr.Label, tr.WorkflowID, wantTriggers[tr.Label])
		}
		if tr.Unresolved {
			t.Errorf("trigger %q flagged unresolved but workflow exists", tr.Label)
		}
	}
	if len(w.Workflows) != 3 {
		t.Fatalf("got %d distinct workflows, want 3", len(w.Workflows))
	}
	for _, ww := range w.Workflows {
		if ww.Unresolved || ww.Workflow == nil {
			t.Errorf("workflow %q unexpectedly unresolved", ww.WorkflowID)
		}
		if !ww.DefaultOpen {
			t.Errorf("workflow %q should be DefaultOpen (<=3 distinct)", ww.WorkflowID)
		}
		if ww.EditURL != "/ui/workflows/"+ww.WorkflowID+"/edit?projectId=headmatch" {
			t.Errorf("workflow %q EditURL=%q", ww.WorkflowID, ww.EditURL)
		}
	}
}

// TestBuildProjectWorkflowWiring_DedupAndUnresolved: default and autonomy point
// at the same id (one deduped panel, two trigger labels); an adaptive candidate
// id that doesn't resolve is flagged Unresolved.
func TestBuildProjectWorkflowWiring_DedupAndUnresolved(t *testing.T) {
	p := &registry.Project{
		ID:                         "janka",
		DefaultWorkflowID:          "adaptive",
		AdaptiveCandidateWorkflows: []string{"ghost-wf"},
		Autonomy:                   registry.ProjectAutonomy{WorkflowID: "adaptive", Mode: "backlog"},
	}
	w := buildProjectWorkflowWiring(p, wfResolver("adaptive"))

	var adaptive *WiredWorkflow
	var ghost *WiredWorkflow
	for i := range w.Workflows {
		switch w.Workflows[i].WorkflowID {
		case "adaptive":
			adaptive = &w.Workflows[i]
		case "ghost-wf":
			ghost = &w.Workflows[i]
		}
	}
	if adaptive == nil {
		t.Fatal("expected a deduped 'adaptive' workflow panel")
	}
	if len(adaptive.TriggerLabels) < 2 {
		t.Errorf("adaptive should carry >=2 trigger labels (default + autonomy), got %v", adaptive.TriggerLabels)
	}
	if ghost == nil || !ghost.Unresolved {
		t.Errorf("ghost-wf must be present and flagged Unresolved, got %+v", ghost)
	}
}

// TestBuildProjectWorkflowWiring_DefaultOnly: a project with just a default
// yields one trigger + one panel.
func TestBuildProjectWorkflowWiring_DefaultOnly(t *testing.T) {
	p := &registry.Project{ID: "snake", DefaultWorkflowID: "dev-pipeline"}
	w := buildProjectWorkflowWiring(p, wfResolver("dev-pipeline"))
	if len(w.Triggers) != 1 || len(w.Workflows) != 1 {
		t.Fatalf("default-only: got %d triggers / %d workflows, want 1/1", len(w.Triggers), len(w.Workflows))
	}
}

// TestProjectDetailWiringRenders asserts the project-detail page renders the
// routing legend (every trigger→workflow), a panel per distinct workflow with
// an editor link, and an unresolved reference flagged rather than hidden.
func TestProjectDetailWiringRenders(t *testing.T) {
	proj := &registry.Project{
		ID:                         "headmatch",
		DefaultWorkflowID:          "dev-pipeline",
		AdaptiveCandidateWorkflows: []string{"ghost-wf"},
		Webhooks: registry.ProjectWebhooks{Sources: []registry.ProjectWebhookSource{{
			Name:                    "github",
			WorkflowID:              "issue-fix",
			ChangeRequestWorkflowID: "github-review",
		}}},
	}
	resolve := wfResolver("dev-pipeline", "issue-fix", "github-review") // ghost-wf unresolved
	data := ProjectDetailData{
		Title:          "Project: headmatch",
		CurrentPage:    "projects",
		Project:        proj,
		WorkflowWiring: buildProjectWorkflowWiring(proj, resolve),
	}
	body := renderProjectDetailBody(t, data)

	for _, want := range []string{
		"Workflow wiring", // legend header
		"issue labeled",   // trigger labels
		"PR / change-request opened",
		"default / manual submit",
		"adaptive pick",
		"/ui/workflows/issue-fix/edit", // per-workflow editor links
		"/ui/workflows/github-review/edit",
		"triggered by:", // per-workflow panel marker
		"ghost-wf",      // unresolved id surfaced
		"unresolved",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered project detail missing %q", want)
		}
	}
}
