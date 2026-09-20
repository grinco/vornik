package registry

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/projectdeps"
)

func TestProjectWithoutDependenciesStaysValid(t *testing.T) {
	// The common case: a project declaring no manifest must keep loading
	// exactly as it did, and must carry no entries to mount.
	p := validProject()
	if err := p.Validate("p.yaml"); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if len(p.Dependencies) != 0 {
		t.Fatalf("Dependencies = %v, want empty", p.Dependencies)
	}
}

func TestProjectDependenciesParseFromYAML(t *testing.T) {
	const src = `
projectId: headmatch
swarmId: dev
defaultWorkflowId: dev-pipeline
dependencies:
  - ecosystem: pip
    lockfile: requirements.lock
`
	var p Project
	if err := yaml.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if err := p.Validate("headmatch.yaml"); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if len(p.Dependencies) != 1 {
		t.Fatalf("Dependencies = %v, want one entry", p.Dependencies)
	}
	if got := p.Dependencies[0]; got.Ecosystem != projectdeps.EcosystemPip || got.Lockfile != "requirements.lock" {
		t.Fatalf("Dependencies[0] = %+v", got)
	}
}

func TestProjectDependencyDefectsSurfaceAtRegistryLoad(t *testing.T) {
	// A bad manifest must fail where a project file fails — at load —
	// rather than at the first task that needed the mount.
	tests := []struct {
		name    string
		entries []projectdeps.Entry
		wantErr string
	}{
		{
			name:    "an install command is refused",
			entries: []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "r.lock", Install: "pip install -r r.txt"}},
			wantErr: "not a supported field",
		},
		{
			name:    "a lockfile escaping the project tree is refused",
			entries: []projectdeps.Entry{{Ecosystem: projectdeps.EcosystemPip, Lockfile: "../other/r.lock"}},
			wantErr: "must stay within the project tree",
		},
		{
			name:    "an unknown ecosystem is refused",
			entries: []projectdeps.Entry{{Ecosystem: "cargo", Lockfile: "Cargo.lock"}},
			wantErr: "unknown ecosystem",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProject()
			p.Dependencies = tt.entries
			err := p.Validate("p.yaml")
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
			var ve ProjectValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("got %T, want ProjectValidationError naming the file", err)
			}
			if ve.Field != "dependencies" {
				t.Fatalf("Field = %q, want %q", ve.Field, "dependencies")
			}
		})
	}
}
