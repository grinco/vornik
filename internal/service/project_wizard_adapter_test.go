package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/templates"
)

func TestFSProjectWriterRejectsUnsafeProjectID(t *testing.T) {
	root := t.TempDir()
	w := &fsProjectWriter{configsDir: root}
	_, err := w.Write(context.Background(), "../escape", []byte("projectId: escape\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid project id") {
		t.Fatalf("expected invalid project id error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "escape.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe write escaped project dir: %v", statErr)
	}
}

func TestFSProjectWriterRefusesExistingFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "projects", "demo.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("existing"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := &fsProjectWriter{configsDir: root}
	_, err := w.Write(context.Background(), "demo", []byte("projectId: demo\n"))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected existing-file error, got %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "existing" {
		t.Fatalf("existing project was overwritten: %q", got)
	}
}

func TestCatalogTemplateSource_MaterialiseMulti(t *testing.T) {
	// Build a temp template directory with a list param fixture.
	dir := t.TempDir()
	slugDir := filepath.Join(dir, "multi")
	require.NoError(t, os.MkdirAll(slugDir, 0o755))

	// Template file that uses {{range .feeds}} to render multiple values.
	templateBody := "id: {{.projectId}}\nfeeds:\n{{range .feeds}}  - {{.}}\n{{end}}"
	require.NoError(t, os.WriteFile(filepath.Join(slugDir, "p.tmpl"), []byte(templateBody), 0o600))

	// Manifest for the multi template.
	manifestYAML := `displayName: "Multi"
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
  - name: feeds
    type: list
    label: Feeds
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`
	require.NoError(t, os.WriteFile(filepath.Join(slugDir, "template.yaml"), []byte(manifestYAML), 0o600))

	// Load the catalog.
	cat, err := templates.Load(dir)
	require.NoError(t, err)

	// Verify catalogTemplateSource satisfies projectwizard.MultiMaterialiser.
	var src projectwizard.MultiMaterialiser = catalogTemplateSource{cat: cat}
	require.NotNil(t, src)

	// Call MaterialiseMulti with multiple feed values.
	files, err := src.MaterialiseMulti("multi", map[string][]string{
		"projectId": {"p1"},
		"feeds":     {"https://a.example", "https://b.example"},
	}, nil)
	require.NoError(t, err)

	// Verify both feeds are present in the rendered output.
	renderedFile, ok := files["projects/p1.yaml"]
	require.True(t, ok, "expected rendered file at projects/p1.yaml")
	require.Contains(t, renderedFile, "- https://a.example", "feed a should be rendered")
	require.Contains(t, renderedFile, "- https://b.example", "feed b should be rendered")
	require.Contains(t, renderedFile, "id: p1", "projectId should be rendered")
}
