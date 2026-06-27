package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// resumeGuardRepo is a TaskRepository whose GetChildren is fully scriptable,
// so resolveResumeGuard's no-children / error / failed-child branches are all
// reachable. It embeds MockTaskRepo for the rest of the interface.
type resumeGuardRepo struct {
	*MockTaskRepo
	children []*persistence.Task
	err      error
}

func (r *resumeGuardRepo) GetChildren(context.Context, string) ([]*persistence.Task, error) {
	return r.children, r.err
}

func failedChild() *persistence.Task {
	return &persistence.Task{ID: "c-failed", Status: persistence.TaskStatusFailed}
}

func okChild() *persistence.Task {
	return &persistence.Task{ID: "c-ok", Status: persistence.TaskStatusCompleted}
}

// TestResolveResumeGuard pins the resume-guard detection extracted from the
// agent case (Track-B Phase 2): handled=true only on a strict-route step with
// a wired repo, a non-nil project, and at least one existing child; childFailed
// reflects whether any detected child is FAILED.
func TestResolveResumeGuard(t *testing.T) {
	adaptive := &registry.Workflow{ID: "adaptive", Entrypoint: "route"}
	project := &registry.Project{}
	task := &persistence.Task{ID: "parent-1"}

	tests := []struct {
		name        string
		plan        *executionPlan
		repo        TaskRepository
		stepID      string
		wantHandled bool
		wantFailed  bool
		wantCount   int
	}{
		{
			name:        "non-strict workflow is never handled",
			plan:        &executionPlan{workflow: &registry.Workflow{ID: "dev-pipeline"}, project: project},
			repo:        &resumeGuardRepo{children: []*persistence.Task{okChild()}},
			stepID:      "route",
			wantHandled: false,
		},
		{
			name:        "nil project short-circuits before the repo query",
			plan:        &executionPlan{workflow: adaptive, project: nil},
			repo:        &resumeGuardRepo{children: []*persistence.Task{okChild()}},
			stepID:      "route",
			wantHandled: false,
		},
		{
			name:        "GetChildren error means not handled",
			plan:        &executionPlan{workflow: adaptive, project: project},
			repo:        &resumeGuardRepo{err: errors.New("db down")},
			stepID:      "route",
			wantHandled: false,
		},
		{
			name:        "no children means a fresh run, not a resume",
			plan:        &executionPlan{workflow: adaptive, project: project},
			repo:        &resumeGuardRepo{children: nil},
			stepID:      "route",
			wantHandled: false,
		},
		{
			name:        "children present, all healthy → handled, not failed",
			plan:        &executionPlan{workflow: adaptive, project: project},
			repo:        &resumeGuardRepo{children: []*persistence.Task{okChild(), okChild()}},
			stepID:      "route",
			wantHandled: true,
			wantFailed:  false,
			wantCount:   2,
		},
		{
			name:        "any failed child → handled and childFailed",
			plan:        &executionPlan{workflow: adaptive, project: project},
			repo:        &resumeGuardRepo{children: []*persistence.Task{okChild(), failedChild()}},
			stepID:      "route",
			wantHandled: true,
			wantFailed:  true,
			wantCount:   2,
		},
		{
			name: "resume_after_children: handled at entrypoint",
			plan: &executionPlan{
				workflow: &registry.Workflow{ID: "github-router", ResumeAfterChildren: true, Entrypoint: "decompose"},
				project:  project,
			},
			repo:        &resumeGuardRepo{children: []*persistence.Task{okChild()}},
			stepID:      "decompose",
			wantHandled: true,
			wantCount:   1,
		},
		{
			name: "resume_after_children: NOT handled at a non-entrypoint step",
			plan: &executionPlan{
				workflow: &registry.Workflow{ID: "github-router", ResumeAfterChildren: true, Entrypoint: "decompose"},
				project:  project,
			},
			repo:        &resumeGuardRepo{children: []*persistence.Task{okChild()}},
			stepID:      "review",
			wantHandled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &Executor{taskRepo: tc.repo}
			handled, failed, children := e.resolveResumeGuard(context.Background(), task, tc.plan, tc.stepID)
			if handled != tc.wantHandled {
				t.Fatalf("handled: want %v, got %v", tc.wantHandled, handled)
			}
			if failed != tc.wantFailed {
				t.Fatalf("childFailed: want %v, got %v", tc.wantFailed, failed)
			}
			if len(children) != tc.wantCount {
				t.Fatalf("children count: want %d, got %d", tc.wantCount, len(children))
			}
		})
	}
}

// TestResolveResumeGuard_NilRepo covers the e.taskRepo == nil guard — an
// executor wired without a task repo must never attempt the children query.
func TestResolveResumeGuard_NilRepo(t *testing.T) {
	e := &Executor{taskRepo: nil}
	plan := &executionPlan{workflow: &registry.Workflow{ID: "adaptive", Entrypoint: "route"}, project: &registry.Project{}}
	handled, failed, children := e.resolveResumeGuard(context.Background(), &persistence.Task{ID: "p"}, plan, "route")
	if handled || failed || children != nil {
		t.Fatalf("nil repo must yield (false,false,nil), got (%v,%v,%v)", handled, failed, children)
	}
}
