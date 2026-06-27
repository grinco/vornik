package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/templates"
)

// TestFSProjectWriterWriteFiles covers the template-anchored commit
// path: a full rendered file set (project.yaml + swarm.md) lands
// below the configs root and the synchronous reload fires once so
// the new project is registered before the redirect.
func TestFSProjectWriterWriteFiles(t *testing.T) {
	dir := t.TempDir()
	reloads := 0
	w := newFSProjectWriter(dir, func() error { reloads++; return nil })
	mw, ok := w.(projectwizard.MultiFileProjectWriter)
	if !ok {
		t.Fatal("fsProjectWriter must implement MultiFileProjectWriter")
	}

	files := map[string]string{
		"projects/acme.yaml":   "projectId: acme\nswarmId: acme-swarm\ndefaultWorkflowId: adaptive\n",
		"swarms/acme-swarm.md": "---\nswarmId: acme-swarm\n---\n# swarm\n",
	}
	url, err := mw.WriteFiles(context.Background(), "acme", files)
	if err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if url != "/ui/projects/acme" {
		t.Errorf("url = %q, want /ui/projects/acme", url)
	}
	if reloads != 1 {
		t.Errorf("reload called %d times, want 1", reloads)
	}
	for target := range files {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(target))); err != nil {
			t.Errorf("file %s not written: %v", target, err)
		}
	}
}

func TestFSProjectWriterWriteFiles_RefusesCollision(t *testing.T) {
	dir := t.TempDir()
	w := newFSProjectWriter(dir, nil).(projectwizard.MultiFileProjectWriter)
	files := map[string]string{"projects/acme.yaml": "projectId: acme\n"}
	if _, err := w.WriteFiles(context.Background(), "acme", files); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Second write of the same target must refuse rather than overwrite.
	if _, err := w.WriteFiles(context.Background(), "acme", files); err == nil {
		t.Error("expected collision refusal on existing target")
	}
}

func TestFSProjectWriterWriteFiles_EmptyAndBadID(t *testing.T) {
	dir := t.TempDir()
	w := newFSProjectWriter(dir, nil).(projectwizard.MultiFileProjectWriter)
	if _, err := w.WriteFiles(context.Background(), "acme", nil); err == nil {
		t.Error("expected error on empty file set")
	}
	if _, err := w.WriteFiles(context.Background(), "../escape", map[string]string{"projects/x.yaml": "x"}); err == nil {
		t.Error("expected error on unsafe project id")
	}
}

// writeTemplate builds a minimal but real template dir so the
// catalogTemplateSource can be exercised against templates.Load /
// MaterialiseFiles end-to-end (no mocking of the templates package).
func writeTemplate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "news-feed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `displayName: "News Feed"
description: "test"
parameters:
  - name: projectId
    type: string
    required: true
  - name: interval
    type: enum
    options: ["1h", "4h"]
    default: "4h"
    required: true
files:
  - source: project.yaml.tmpl
    target: "projects/{{.projectId}}.yaml"
  - source: swarm.md.tmpl
    target: "swarms/{{.projectId}}-swarm.md"
`
	project := "projectId: \"{{.projectId}}\"\nswarmId: \"{{.projectId}}-swarm\"\ndefaultWorkflowId: \"adaptive\"\nautonomy:\n  pollInterval: \"{{.interval}}\"\n"
	swarm := "---\nswarmId: \"{{.projectId}}-swarm\"\n---\n# swarm\n"
	mustWrite(t, filepath.Join(dir, "template.yaml"), manifest)
	mustWrite(t, filepath.Join(dir, "project.yaml.tmpl"), project)
	mustWrite(t, filepath.Join(dir, "swarm.md.tmpl"), swarm)
	return root
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogTemplateSource_LookupAndMaterialise(t *testing.T) {
	cat, err := templates.Load(writeTemplate(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	src := catalogTemplateSource{cat: cat}

	spec, ok := src.Lookup("news-feed")
	if !ok {
		t.Fatal("Lookup(news-feed) = false, want true")
	}
	if spec.Slug != "news-feed" || len(spec.Params) != 2 {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	// The enum param's options must survive the adapter so the wizard
	// can screen out-of-range LLM values.
	var foundEnum bool
	for _, p := range spec.Params {
		if p.Name == "interval" {
			foundEnum = true
			if p.Type != "enum" || len(p.Options) != 2 {
				t.Errorf("enum param not mapped: %#v", p)
			}
		}
	}
	if !foundEnum {
		t.Error("interval param missing from spec")
	}

	// Materialise renders both files; omitted interval falls to default.
	files, err := src.Materialise("news-feed", map[string]string{"projectId": "acme"})
	if err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %#v", len(files), files)
	}
	body, ok := files["projects/acme.yaml"]
	if !ok {
		t.Fatal("project file not rendered")
	}
	if !strings.Contains(body, "pollInterval: \"4h\"") {
		t.Errorf("default interval not applied: %q", body)
	}
	if !strings.Contains(body, "swarmId: \"acme-swarm\"") {
		t.Errorf("swarmId not rendered: %q", body)
	}

	if _, ok := src.Lookup("nope"); ok {
		t.Error("Lookup of unknown slug should be false")
	}
	if _, err := src.Materialise("nope", nil); err == nil {
		t.Error("Materialise of unknown slug should error")
	}
}
