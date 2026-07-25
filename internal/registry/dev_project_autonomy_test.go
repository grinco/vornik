package registry

// The shipped dev example must NOT auto-run: a fresh install left autonomy on
// (5m tick, 6 tasks/hr) burned tokens unattended. See onboarding-hardening-design F4.

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// LoadProjectFile doesn't exist in this package; a single project YAML is
// parsed with a direct yaml.Unmarshal into Project + Project.Validate — the
// exact idiom the package already uses to load one project file for a test
// (see TestExampleConfigs_ParseAndValidate in example_configs_test.go).
// LoadProjects, by contrast, walks an entire projects/ directory and is the
// wrong shape for asserting on a single named file.
func TestDevProjectShipsAutonomyOff(t *testing.T) {
	path := "../../configs/projects/dev-project.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var p Project
	if err := yaml.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	if err := p.Validate(path); err != nil {
		t.Fatalf("validate %s: %v", path, err)
	}
	if p.Autonomy.Enabled {
		t.Fatalf("dev-project must ship autonomy.enabled=false")
	}
}
