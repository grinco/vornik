package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCRLFTestTree lays down a small config tree under a temp dir with one
// CRLF-tainted file and one clean file. Returns the dir and the path of the
// tainted file so callers can assert on it after a --fix.
func writeCRLFTestTree(t *testing.T) (dir, taintedPath, cleanPath string) {
	t.Helper()
	dir = t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	taintedPath = filepath.Join(swarmsDir, "tainted.md")
	cleanPath = filepath.Join(swarmsDir, "clean.md")
	if err := os.WriteFile(taintedPath, []byte("---\r\nswarmId: \"x\"\r\n---\r\n"), 0o644); err != nil {
		t.Fatalf("write tainted: %v", err)
	}
	if err := os.WriteFile(cleanPath, []byte("---\nswarmId: \"y\"\n---\n"), 0o644); err != nil {
		t.Fatalf("write clean: %v", err)
	}
	return dir, taintedPath, cleanPath
}

// TestCheckConfigCRLF_NoConfigDirOK: with nothing wired, the check skips
// cleanly rather than erroring.
func TestCheckConfigCRLF_NoConfigDirOK(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkConfigCRLF(false)
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK", got.Status)
	}
}

// TestCheckConfigCRLF_FlagsTaintedFile: a CRLF file is flagged WARNING and
// named in Items; the clean file is not.
func TestCheckConfigCRLF_FlagsTaintedFile(t *testing.T) {
	dir, tainted, clean := writeCRLFTestTree(t)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkConfigCRLF(false)
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING; msg=%q", got.Status, got.Message)
	}
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, filepath.Base(tainted)) {
		t.Errorf("tainted file should be listed; items=%v", got.Items)
	}
	if strings.Contains(joined, filepath.Base(clean)) {
		t.Errorf("clean file should NOT be listed; items=%v", got.Items)
	}
	if got.Fixed != 0 {
		t.Errorf("Fixed should be 0 without --fix; got %d", got.Fixed)
	}
}

// TestCheckConfigCRLF_FixStripsCR: --fix removes the CR bytes in place and
// reports the count; a re-scan then comes back clean.
func TestCheckConfigCRLF_FixStripsCR(t *testing.T) {
	dir, tainted, _ := writeCRLFTestTree(t)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkConfigCRLF(true)
	if got.Fixed != 1 {
		t.Fatalf("Fixed = %d, want 1; msg=%q items=%v", got.Fixed, got.Message, got.Items)
	}
	if got.Status != "OK" {
		t.Errorf("status after full fix = %q, want OK", got.Status)
	}
	data, err := os.ReadFile(tainted)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if strings.Contains(string(data), "\r") {
		t.Errorf("CR bytes should be stripped; file still contains \\r")
	}
	if string(data) != "---\nswarmId: \"x\"\n---\n" {
		t.Errorf("LF content not preserved; got %q", string(data))
	}
	// Idempotent: a second scan finds nothing.
	again := h.checkConfigCRLF(false)
	if again.Status != "OK" {
		t.Errorf("rescan status = %q, want OK; items=%v", again.Status, again.Items)
	}
}

// TestCheckConfigCRLF_ScansConfigYAML: the daemon's config.yaml (configPath)
// is scanned even when it lives outside configDir.
func TestCheckConfigCRLF_ScansConfigYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\r\n  address: x\r\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	h := &DoctorHandlers{configPath: cfgPath}
	got := h.checkConfigCRLF(false)
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING", got.Status)
	}
	if !strings.Contains(strings.Join(got.Items, "\n"), "config.yaml") {
		t.Errorf("config.yaml should be flagged; items=%v", got.Items)
	}
}
