package registry

import (
	"strings"
	"testing"
)

// forge.ci.trigger_workflow_paths validation (design
// 2026-09-08-forge-ci-outcomes-design.md §14.2).

func ciProject(watched, triggers []string) *Project {
	return &Project{
		ID: "p", DisplayName: "p", SwarmID: "s", DefaultWorkflowID: "wf",
		Forge: ProjectForge{
			Provider: "github",
			CI:       ProjectForgeCI{Enabled: true, WorkflowPaths: watched, TriggerWorkflowPaths: triggers},
		},
	}
}

// A trigger path outside a non-empty workflow_paths could never fire, because
// the run is never ingested. Rejected at LOAD: a control that silently never
// fires is the defect, not the config that produced it.
func TestValidate_TriggerPathOutsideWorkflowPathsIsRejected(t *testing.T) {
	err := ciProject(
		[]string{".github/workflows/pytest.yml"},
		[]string{".github/workflows/typecheck.yml"},
	).Validate("p.yaml")
	if err == nil {
		t.Fatal("a trigger path that is never ingested must fail the load")
	}
	for _, want := range []string{"typecheck.yml", "workflow_paths"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q so the fix is obvious: %v", want, err)
		}
	}
}

func TestValidate_TriggerPathInsideWorkflowPathsIsAccepted(t *testing.T) {
	p := ciProject(
		[]string{".github/workflows/pytest.yml", ".github/workflows/typecheck.yml"},
		[]string{".github/workflows/typecheck.yml"},
	)
	if err := p.Validate("p.yaml"); err != nil {
		t.Fatalf("a trigger path that IS ingested must load: %v", err)
	}
}

// An EMPTY workflow_paths records everything, so any trigger path is reachable.
// This is the arrangement §14 exists to enable — record all, trigger one — and
// rejecting it would defeat the whole amendment.
func TestValidate_EmptyWorkflowPathsAcceptsAnyTrigger(t *testing.T) {
	p := ciProject(nil, []string{".github/workflows/typecheck.yml"})
	if err := p.Validate("p.yaml"); err != nil {
		t.Fatalf("record-everything + trigger-on-one must load: %v", err)
	}
}
