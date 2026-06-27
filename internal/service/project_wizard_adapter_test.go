package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
