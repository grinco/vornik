package ui

// Regression tests for CodeQL go/path-injection remediation (2026-07-18).
//
// Each test asserts that a request-derived identifier which is NOT a single
// safe path component (traversal / separator forms) is rejected AS INVALID
// by the handler or data-builder that joins it into the config / workspace
// tree — before any filesystem sink is reached.
//
// These fail pre-fix: the old hand-rolled `strings.Contains(id, "/")` /
// os.PathSeparator guards missed the backslash form (os.PathSeparator == "/"
// on POSIX) and the bare ".." form, so a `back\slash` or ".." id slipped the
// guard and fell through to a "not found" / read-error path instead of an
// "invalid id" refusal. safepath.CleanPathComponent + JoinUnder is the
// recognised barrier that refuses every one up front.
//
// Sites covered (CE alert lines in parentheses):
//   - listArtifactFiles           (artifacts.go:403)
//   - applyProjectPatches         (project_archive.go:271)
//   - removeEmptyLifecycleMap     (project_archive.go:296)
//   - projectBriefData            (project_brief.go:172)
//   - projectConfigFormData       (project_config_form.go:332,439)
//   - projectSchemaConfigData     (project_schema_config.go:112,203,217)
//   - swarmEditData               (swarm_edit.go:159,292; swarm_delete.go:51)
//   - workflowEditData            (workflow_edit.go:201,326; swarm_delete.go:84)
//   - swarmSchemaConfigData       (swarm_schema_config.go:235; schema_config_save.go:78)
//   - workflowSchemaConfigData    (workflow_schema_config.go:172; schema_config_save.go:78)
//   - WizardGenerate              (wizard.go:204; 153/160 clear via the same barrier)

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pathInjectionTraversalIDs is the shared hostile-id corpus every barrier
// below must refuse as invalid: parent-dir traversal, a relative escape, a
// forward-slash separator, and a backslash separator (the case the old
// strings.Contains(id, "/") guard missed on POSIX).
var pathInjectionTraversalIDs = []string{"..", "../escape", "with/slash", `back\slash`}

func TestListArtifactFiles_RejectsTraversalID(t *testing.T) {
	// listArtifactFiles was already validated pre-fix (via
	// validateProjectIDComponent) but discarded the cleaned value, so CodeQL
	// did not recognise the barrier; the fix threads the cleaned id through
	// JoinUnder. This pins the invariant: every hostile id is refused before
	// the os.Stat sink.
	root := t.TempDir()
	for _, id := range pathInjectionTraversalIDs {
		if _, err := listArtifactFiles(root, id); err == nil {
			t.Errorf("listArtifactFiles(root, %q) = nil error, want rejection", id)
		}
	}
}

func TestApplyProjectPatches_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	patches := []yamlPatch{{Path: []string{"lifecycle", "status"}, Value: "archived"}}
	for _, id := range pathInjectionTraversalIDs {
		err := s.applyLifecyclePatches(id, patches)
		if err == nil || !strings.Contains(err.Error(), "invalid project id") {
			t.Errorf("applyLifecyclePatches(%q) err = %v, want invalid-project-id refusal", id, err)
		}
		err = s.removeEmptyLifecycleMap(id)
		if err == nil || !strings.Contains(err.Error(), "invalid project id") {
			t.Errorf("removeEmptyLifecycleMap(%q) err = %v, want invalid-project-id refusal", id, err)
		}
	}
}

func TestProjectBriefData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.projectBriefData(id); !strings.Contains(data.Error, "Invalid project id") {
			t.Errorf("projectBriefData(%q).Error = %q, want %q", id, data.Error, "Invalid project id")
		}
	}
}

func TestProjectConfigFormData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.projectConfigFormData(id); !strings.Contains(data.Error, "Invalid project id") {
			t.Errorf("projectConfigFormData(%q).Error = %q, want %q", id, data.Error, "Invalid project id")
		}
	}
}

func TestProjectSchemaConfigData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.projectSchemaConfigData(id); !strings.Contains(data.Error, "Invalid project id") {
			t.Errorf("projectSchemaConfigData(%q).Error = %q, want %q", id, data.Error, "Invalid project id")
		}
	}
}

func TestSwarmEditData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.swarmEditData(id); !strings.Contains(data.Error, "Invalid swarm id") {
			t.Errorf("swarmEditData(%q).Error = %q, want %q", id, data.Error, "Invalid swarm id")
		}
	}
}

func TestWorkflowEditData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.workflowEditData(id); !strings.Contains(data.Error, "Invalid workflow id") {
			t.Errorf("workflowEditData(%q).Error = %q, want %q", id, data.Error, "Invalid workflow id")
		}
	}
}

func TestSwarmSchemaConfigData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.swarmSchemaConfigData(id); !strings.Contains(data.Error, "Invalid swarm id") {
			t.Errorf("swarmSchemaConfigData(%q).Error = %q, want %q", id, data.Error, "Invalid swarm id")
		}
	}
}

func TestWorkflowSchemaConfigData_RejectsTraversalID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		if data := s.workflowSchemaConfigData(id); !strings.Contains(data.Error, "Invalid workflow id") {
			t.Errorf("workflowSchemaConfigData(%q).Error = %q, want %q", id, data.Error, "Invalid workflow id")
		}
	}
}

func TestWizardGenerate_RejectsTraversalProjectID(t *testing.T) {
	s := NewServer(WithProjectRegistry(buildPopulatedUIRegistry(t)))
	for _, id := range pathInjectionTraversalIDs {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/projects/x/wizard", strings.NewReader(""))
		s.WizardGenerate(rec, req, id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("WizardGenerate(%q) status = %d, want 404", id, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "invalid project id") {
			t.Errorf("WizardGenerate(%q) body = %q, want invalid-project-id refusal", id, rec.Body.String())
		}
	}
}
