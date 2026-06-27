package featuredoctor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBackupRestoreRoundTrip verifies that backupConfig writes a timestamped
// copy of the target file and that restoreConfig reproduces byte-identical
// content at the original path.
func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "config.yaml")
	content := []byte("instinct:\n  enabled: true\n")
	if err := os.WriteFile(original, content, 0o600); err != nil {
		t.Fatal(err)
	}

	backupPath, err := backupConfig(original)
	if err != nil {
		t.Fatalf("backupConfig: %v", err)
	}

	// Backup must exist.
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup file not found at %s: %v", backupPath, err)
	}

	// Backup name must differ from original.
	if filepath.Base(backupPath) == filepath.Base(original) {
		t.Fatal("backup filename must differ from original")
	}

	// Overwrite the original to simulate a botched write.
	if err := os.WriteFile(original, []byte("bad content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Restore from backup.
	if err := restoreConfig(backupPath, original); err != nil {
		t.Fatalf("restoreConfig: %v", err)
	}

	// Verify byte-identical restoration.
	got, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("restored content mismatch:\n  want: %q\n  got:  %q", content, got)
	}
}

// TestBackupConfig_MissingSource ensures backupConfig returns an error when
// the source file does not exist.
func TestBackupConfig_MissingSource(t *testing.T) {
	_, err := backupConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing source file")
	}
}

// TestRestoreConfig_MissingBackup ensures restoreConfig returns an error when
// the backup file does not exist.
func TestRestoreConfig_MissingBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	err := restoreConfig("/nonexistent/backup.yaml", target)
	if err == nil {
		t.Fatal("expected error for missing backup file")
	}
}

// TestRestoreConfig_BadTargetDir ensures restoreConfig returns an error when
// the target directory does not exist (CreateTemp will fail).
func TestRestoreConfig_BadTargetDir(t *testing.T) {
	dir := t.TempDir()
	// Write a valid backup file.
	backupPath := filepath.Join(dir, "config.yaml.bak")
	if err := os.WriteFile(backupPath, []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Target in a non-existent directory.
	target := filepath.Join(dir, "noexist", "config.yaml")
	err := restoreConfig(backupPath, target)
	if err == nil {
		t.Fatal("expected error when target directory does not exist")
	}
}

// TestBackupConfig_UnwritableDir ensures backupConfig returns an error when
// the directory is not writable.
func TestBackupConfig_UnwritableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(src, []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Remove write permission from the directory.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck
	_, err := backupConfig(src)
	if err == nil {
		t.Fatal("expected error writing backup in non-writable dir")
	}
}
