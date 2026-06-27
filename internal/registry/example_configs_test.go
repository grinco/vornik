package registry

// Lightweight smoke check that the operator-facing example YAMLs
// under configs/examples/ parse cleanly against the current
// registry.Project shape and pass Validate. Catches drift between
// the docs and the code when fields are renamed.

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExampleConfigs_ParseAndValidate(t *testing.T) {
	// Walk up to repo root: this test file lives at
	// internal/registry/, so two levels up gets us there.
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	// Validate every example YAML that is present — globbed rather than
	// hard-listed so the smoke check stays correct as examples are added or
	// (in edition builds) excluded.
	matches, err := filepath.Glob(filepath.Join(repoRoot, "configs", "examples", "*.yaml"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no example configs found under configs/examples/")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
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
			if p.ID == "" {
				t.Errorf("%s: parsed but projectId is empty", path)
			}
		})
	}
}
