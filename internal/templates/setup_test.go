package templates

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemplate(t *testing.T, root, slug, manifest string) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.tmpl"), []byte("id: {{.projectId}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "template.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

const setupManifest = `displayName: "With Setup"
hidden: false
setup:
  secrets:
    - name: GITHUB_TOKEN
      label: "GitHub token (repo read)"
      required: true
  mcpServers:
    - name: slack
      hint: "used to deliver reports"
  model: required
  smokeTask:
    goal: "Fetch one item and summarise it in one line."
  checks: [secrets, mcp_reachable, model_ping]
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`

func TestSetupSection_Parses(t *testing.T) {
	root := t.TempDir()
	writeTemplate(t, root, "with-setup", setupManifest)
	cat, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m, ok := cat.Get("with-setup")
	if !ok || m.Setup == nil {
		t.Fatal("setup section not parsed")
	}
	if len(m.Setup.Secrets) != 1 || m.Setup.Secrets[0].Name != "GITHUB_TOKEN" || !m.Setup.Secrets[0].Required {
		t.Fatalf("secrets: %#v", m.Setup.Secrets)
	}
	if m.Setup.Model != "required" {
		t.Fatalf("model: %q", m.Setup.Model)
	}
	if m.Setup.SmokeTask == nil || m.Setup.SmokeTask.Goal == "" {
		t.Fatal("smokeTask.goal not parsed")
	}
	if len(m.Setup.Checks) != 3 {
		t.Fatalf("checks: %#v", m.Setup.Checks)
	}
}

func TestSetupSection_RefusesUnknownCheckAndModel(t *testing.T) {
	root := t.TempDir()
	writeTemplate(t, root, "bad-check", `displayName: "Bad"
setup:
  checks: [secrets, astrology]
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`)
	if _, err := Load(root); err == nil {
		t.Fatal("unknown check name must fail Load")
	}
	root2 := t.TempDir()
	writeTemplate(t, root2, "bad-model", `displayName: "Bad"
setup:
  model: optional-ish
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`)
	if _, err := Load(root2); err == nil {
		t.Fatal("setup.model outside {\"\", \"required\"} must fail Load")
	}
}

func TestHidden_ExcludedFromListButGettable(t *testing.T) {
	root := t.TempDir()
	writeTemplate(t, root, "visible", `displayName: "Visible"
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`)
	writeTemplate(t, root, "sneaky", `displayName: "Sneaky"
hidden: true
domain: "internal"
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`)
	cat, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cat.List() {
		if m.Slug == "sneaky" {
			t.Fatal("hidden template leaked into List()")
		}
	}
	for _, d := range cat.Domains() {
		if d == "internal" {
			t.Fatal("hidden template's domain leaked into Domains()")
		}
	}
	if _, ok := cat.Get("sneaky"); !ok {
		t.Fatal("hidden template must stay resolvable via Get")
	}
}

func TestSetupSummary(t *testing.T) {
	s := &SetupSpec{
		Secrets:    []SetupSecret{{Name: "GITHUB_TOKEN", Label: "GitHub token", Required: true}},
		MCPServers: []SetupMCPServer{{Name: "slack"}},
		Model:      "required",
	}
	sum := s.Summary()
	if len(sum) != 3 {
		t.Fatalf("summary: %#v", sum)
	}
}
