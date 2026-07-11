package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
)

// TestProjectConfigWriter_ImplementsConfigWriter is a compile-time-ish
// check kept as a runtime assertion too: ProjectConfigWriter must satisfy
// featuredoctor.ConfigWriter so it's a drop-in for the Save write path.
func TestProjectConfigWriter_ImplementsConfigWriter(_ *testing.T) {
	var _ featuredoctor.ConfigWriter = (*ProjectConfigWriter)(nil)
}

func TestProjectConfigWriter_ValidateAcceptsValidProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj-a.yaml")
	content := "projectId: proj-a\nswarmId: s\ndefaultWorkflowId: w\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &ProjectConfigWriter{FileConfigWriter: featuredoctor.FileConfigWriter{Path: path}}
	if err := w.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a valid project file", err)
	}
}

func TestProjectConfigWriter_ValidateRejectsMalformedProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj-a.yaml")
	// swarmId missing — Project.Validate requires it.
	content := "projectId: proj-a\ndefaultWorkflowId: w\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &ProjectConfigWriter{FileConfigWriter: featuredoctor.FileConfigWriter{Path: path}}
	err := w.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error (missing swarmId)")
	}
	if !strings.Contains(err.Error(), "swarmId") {
		t.Errorf("error = %v, want it to name swarmId", err)
	}
}

// TestProjectConfigWriter_DoesNotValidateAsDaemonSchema proves the actual
// bug fix: a project file that would fail as a daemon config.Config (no
// server/database block) must still VALIDATE CLEANLY here, because it's
// being checked against the project schema, not the daemon schema.
func TestProjectConfigWriter_DoesNotValidateAsDaemonSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj-a.yaml")
	content := "projectId: proj-a\nswarmId: s\ndefaultWorkflowId: w\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &ProjectConfigWriter{FileConfigWriter: featuredoctor.FileConfigWriter{Path: path}}
	if err := w.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil — this file has no server/database block and would fail the DAEMON schema, but must pass the project schema", err)
	}

	// Sanity check the premise: the plain FileConfigWriter (daemon schema)
	// really would reject this same file, proving the two Validate()
	// implementations diverge as documented.
	daemon := &featuredoctor.FileConfigWriter{Path: path}
	if err := daemon.Validate(); err == nil {
		t.Fatal("sanity check failed: expected the daemon-schema FileConfigWriter.Validate() to reject a bare project file")
	}
}

func TestProjectConfigWriter_ReadWriteBackupRestoreDelegate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj-a.yaml")
	original := []byte("projectId: proj-a\nswarmId: s\ndefaultWorkflowId: w\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	w := &ProjectConfigWriter{FileConfigWriter: featuredoctor.FileConfigWriter{Path: path}}

	got, err := w.Read()
	if err != nil || string(got) != string(original) {
		t.Fatalf("Read() = (%q, %v), want (%q, nil)", got, err, original)
	}

	backup, err := w.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	updated := []byte("projectId: proj-a\nswarmId: s2\ndefaultWorkflowId: w\n")
	if err := w.Write(updated); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err = w.Read()
	if err != nil || string(got) != string(updated) {
		t.Fatalf("Read() after Write = (%q, %v), want (%q, nil)", got, err, updated)
	}

	if err := w.Restore(backup); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err = w.Read()
	if err != nil || string(got) != string(original) {
		t.Fatalf("Read() after Restore = (%q, %v), want the original content restored", got, err)
	}
}
