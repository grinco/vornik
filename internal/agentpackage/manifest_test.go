package agentpackage

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleManifest = `
package: acme-incident-response
version: 1.2.0
contributes:
  workflows: [incident-triage.md]
  roles: [incident-lead.md]
`

func TestManifestParsesAndValidates(t *testing.T) {
	var m Manifest
	if err := yaml.Unmarshal([]byte(sampleManifest), &m); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if m.Package != "acme-incident-response" || m.Version != "1.2.0" {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestManifestValidationRefusals(t *testing.T) {
	base := func() Manifest {
		return Manifest{Package: "acme", Version: "1.0.0", Contributes: Contributions{Workflows: []string{"w.md"}}}
	}
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{"no package name", func(m *Manifest) { m.Package = "" }, "package: is required"},
		{"package name is not kebab-case", func(m *Manifest) { m.Package = "Acme Package" }, "kebab-case"},
		{"no version", func(m *Manifest) { m.Version = "" }, "version: is required"},
		{"version is not semver", func(m *Manifest) { m.Version = "v1" }, "MAJOR.MINOR.PATCH"},
		{"contributes nothing", func(m *Manifest) { m.Contributes = Contributions{} }, "declares nothing"},
		{"absolute payload path", func(m *Manifest) { m.Contributes.Workflows = []string{"/etc/passwd"} }, "must be relative"},
		{"escaping payload path", func(m *Manifest) { m.Contributes.Workflows = []string{"../../../etc/passwd"} }, "escapes the package tree"},
		{"empty payload path", func(m *Manifest) { m.Contributes.Workflows = []string{"  "} }, "empty path"},
		{
			name:    "one file contributed twice",
			mutate:  func(m *Manifest) { m.Contributes.Roles = []string{"w.md"} },
			wantErr: "already contributed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			tt.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeferredContributionKindsRefuseWithTheirReason(t *testing.T) {
	// "not yet" and "never" are different boundaries, and an operator
	// planning a package needs to know which one they met. Each deferred
	// kind must say WHY, not just that it is unsupported.
	tests := []struct {
		name     string
		mutate   func(*Manifest)
		wantWord string
	}{
		{"guidance is prompt text", func(m *Manifest) { m.Contributes.Guidance = []string{"priors.md"} }, "print-and-confirm"},
		{"mcp servers are an endpoint", func(m *Manifest) { m.Contributes.MCPServers = []string{"pd.yaml"} }, "SECURITY reason"},
		{"skills have their own ledger", func(m *Manifest) { m.Contributes.Skills = []string{"runbook.md"} }, "two stores writing one deployed file"},
		{"swarm fragments", func(m *Manifest) { m.Contributes.SwarmFragments = []string{"s.yaml"} }, "slice 1"},
		{"schedules", func(m *Manifest) { m.Contributes.Schedules = []string{"nightly.yaml"} }, "slice 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Manifest{Package: "acme", Version: "1.0.0", Contributes: Contributions{Workflows: []string{"w.md"}}}
			tt.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Fatalf("Validate() = %q, want the reason to mention %q", err, tt.wantWord)
			}
		})
	}
}

func TestContentHashDistinguishesAnEdit(t *testing.T) {
	// The whole uninstall guarantee rests on this: a row id alone says
	// "this package put something here"; it cannot say whether what is
	// there now is still what the package put.
	a := ContentHash([]byte("workflow: v1\n"))
	if a != ContentHash([]byte("workflow: v1\n")) {
		t.Fatal("ContentHash is not deterministic")
	}
	if a == ContentHash([]byte("workflow: v1 \n")) {
		t.Fatal("ContentHash missed a one-byte edit")
	}
}
