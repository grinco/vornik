package projectwizard

import (
	"strings"
	"testing"
)

func validBundleMaps() (project, swarm map[string]any, workflows []map[string]any) {
	project = map[string]any{"projectId": "ai-news-digest", "swarmId": "ai-news-digest-swarm", "defaultWorkflowId": "research-digest"}
	swarm = map[string]any{"swarmId": "ai-news-digest-swarm", "leadRole": "researcher"}
	workflows = []map[string]any{{"workflowId": "research-digest"}}
	return
}

func TestShapeCheckBundle_Valid(t *testing.T) {
	project, swarm, workflows := validBundleMaps()
	bundle := &ComposedBundle{Project: project, Swarm: swarm, Workflows: workflows}
	ids, errs := shapeCheckBundle(bundle)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
	if ids.ProjectID != "ai-news-digest" || ids.SwarmID != "ai-news-digest-swarm" {
		t.Errorf("ids not extracted correctly: %+v", ids)
	}
	if len(ids.WorkflowIDs) != 1 || ids.WorkflowIDs[0] != "research-digest" {
		t.Errorf("workflow ids not extracted correctly: %+v", ids)
	}
}

func TestShapeCheckBundle_NilBundle(t *testing.T) {
	_, errs := shapeCheckBundle(nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error for nil bundle, got %v", errs)
	}
}

func TestShapeCheckBundle_ZeroWorkflows(t *testing.T) {
	project, swarm, _ := validBundleMaps()
	bundle := &ComposedBundle{Project: project, Swarm: swarm, Workflows: nil}
	_, errs := shapeCheckBundle(bundle)
	if !anyContains(errs, "at least one workflow") {
		t.Errorf("expected zero-workflow error, got %v", errs)
	}
}

func TestShapeCheckBundle_TooManyWorkflows(t *testing.T) {
	project, swarm, workflows := validBundleMaps()
	workflows = append(workflows,
		map[string]any{"workflowId": "wf-2"},
		map[string]any{"workflowId": "wf-3"},
	)
	bundle := &ComposedBundle{Project: project, Swarm: swarm, Workflows: workflows}
	_, errs := shapeCheckBundle(bundle)
	if !anyContains(errs, "at most 2 workflows") {
		t.Errorf("expected >2 workflow error, got %v", errs)
	}
}

func TestShapeCheckBundle_MissingIDs(t *testing.T) {
	bundle := &ComposedBundle{
		Project:   map[string]any{},
		Swarm:     map[string]any{},
		Workflows: []map[string]any{{}},
	}
	_, errs := shapeCheckBundle(bundle)
	if !anyContains(errs, "project.projectId") {
		t.Errorf("expected missing projectId error, got %v", errs)
	}
	if !anyContains(errs, "swarm.swarmId") {
		t.Errorf("expected missing swarmId error, got %v", errs)
	}
	if !anyContains(errs, "workflowId is required") {
		t.Errorf("expected missing workflowId error, got %v", errs)
	}
}

func TestShapeCheckBundle_InvalidSlug(t *testing.T) {
	bundle := &ComposedBundle{
		Project:   map[string]any{"projectId": "not a slug!"},
		Swarm:     map[string]any{"swarmId": "ok-swarm"},
		Workflows: []map[string]any{{"workflowId": "ok-wf"}},
	}
	_, errs := shapeCheckBundle(bundle)
	if !anyContains(errs, "not a valid slug") {
		t.Errorf("expected slug validation error, got %v", errs)
	}
}

func TestShapeCheckBundle_DuplicateWorkflowID(t *testing.T) {
	project, swarm, _ := validBundleMaps()
	bundle := &ComposedBundle{Project: project, Swarm: swarm, Workflows: []map[string]any{
		{"workflowId": "dup"}, {"workflowId": "dup"},
	}}
	_, errs := shapeCheckBundle(bundle)
	if !anyContains(errs, "duplicate workflowId") {
		t.Errorf("expected duplicate workflowId error, got %v", errs)
	}
}

func TestCollisionCheckBundle(t *testing.T) {
	ids := bundleIDs{ProjectID: "p1", SwarmID: "s1", WorkflowIDs: []string{"w1", "w2"}}
	live := liveEntityIDs{
		Projects:  map[string]bool{"p1": true},
		Swarms:    map[string]bool{},
		Workflows: map[string]bool{"w2": true},
	}
	errs := collisionCheckBundle(ids, live)
	if !anyContains(errs, `projectId "p1"`) {
		t.Errorf("expected project collision, got %v", errs)
	}
	if !anyContains(errs, `workflowId "w2"`) {
		t.Errorf("expected workflow collision, got %v", errs)
	}
	if anyContains(errs, `swarmId`) {
		t.Errorf("no swarm collision expected, got %v", errs)
	}
}

func TestCollisionCheckBundle_NoCollision(t *testing.T) {
	ids := bundleIDs{ProjectID: "fresh", SwarmID: "fresh-swarm", WorkflowIDs: []string{"fresh-wf"}}
	live := liveEntityIDs{Projects: map[string]bool{"other": true}}
	if errs := collisionCheckBundle(ids, live); len(errs) != 0 {
		t.Errorf("expected no collisions, got %v", errs)
	}
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
