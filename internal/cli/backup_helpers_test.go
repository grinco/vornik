package cli

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
)

func TestCopyFileAndDirPreserveContent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file := filepath.Join(src, "nested", "data.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "nested", "data.txt"))
	if err != nil {
		t.Fatalf("ReadFile copied: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("copied content = %q", got)
	}
	info, err := os.Stat(filepath.Join(dst, "nested", "data.txt"))
	if err != nil {
		t.Fatalf("Stat copied: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("copied mode = %o, want 0640", info.Mode().Perm())
	}
	if fileSize(filepath.Join(dst, "nested", "data.txt")) != 5 {
		t.Fatalf("fileSize copied file mismatch")
	}
	if fileSize(filepath.Join(dst, "missing")) != 0 {
		t.Fatalf("fileSize missing should be zero")
	}
}

func TestCopyDirSkipsSymlinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("MkdirAll src: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "regular.txt"), []byte("regular"), 0o644); err != nil {
		t.Fatalf("WriteFile regular: %v", err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "linked-secret.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "regular.txt")); err != nil {
		t.Fatalf("regular file was not copied: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "linked-secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was copied, lstat err = %v", err)
	}
}

func TestTarGzDirAndUntarGzRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "data.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "backup.tgz")
	if err := tarGzDir(src, archive); err != nil {
		t.Fatalf("tarGzDir() error = %v", err)
	}

	dst := filepath.Join(t.TempDir(), "restore")
	if err := untarGz(archive, dst); err != nil {
		t.Fatalf("untarGz() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "nested", "data.txt"))
	if err != nil {
		t.Fatalf("ReadFile restored: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("restored content = %q", got)
	}
}

func TestUntarGzRejectsUnsafePath(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bad.tgz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: int64(len("bad"))}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte("bad")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}

	err = untarGz(archive, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("untarGz unsafe error = %v", err)
	}
}

func TestWritePGPassFileEscapesAndCleansUp(t *testing.T) {
	path, cleanup, err := writePGPassFile(&config.DatabaseConfig{
		Host:     `db\host`,
		Port:     5432,
		Name:     "swarm:prod",
		User:     "user:name",
		Password: `p\ass:word`,
	})
	if err != nil {
		t.Fatalf("writePGPassFile() error = %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat pgpass: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pgpass mode = %o, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile pgpass: %v", err)
	}
	want := `db\\host:5432:swarm\:prod:user\:name:p\\ass\:word` + "\n"
	if string(raw) != want {
		t.Fatalf("pgpass content = %q, want %q", raw, want)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove pgpass, stat err = %v", err)
	}
}

func TestFallbackSSLMode(t *testing.T) {
	if got := fallbackSSLMode(""); got != "disable" {
		t.Errorf("empty → %q, want disable", got)
	}
	if got := fallbackSSLMode("  "); got != "disable" {
		t.Errorf("whitespace → %q, want disable", got)
	}
	if got := fallbackSSLMode("verify-full"); got != "verify-full" {
		t.Errorf("explicit value clobbered: %q", got)
	}
}

func TestRestoreCommandHasAllowNonEmptyFlag(t *testing.T) {
	// Smoke: the slice-3 backup polish added --allow-non-empty. If
	// the flag goes missing the operator escape hatch disappears,
	// so guard the wiring at the cobra layer.
	if f := restoreCmd.Flags().Lookup("allow-non-empty"); f == nil {
		t.Fatal("restoreCmd is missing the --allow-non-empty flag")
	}
}

func TestRestoreCommandUsesSingleTransactionFlag(t *testing.T) {
	// The Long help text mentions single-transaction in nowhere
	// human-visible, but the cobra command's Use field stays
	// "restore". The behavioural assertion lives in the exec
	// surface (--single-transaction passed to psql). Smoke-test
	// that runRestore is still wired in.
	if restoreCmd.RunE == nil {
		t.Fatal("restoreCmd.RunE is nil")
	}
}

func TestTrimTS(t *testing.T) {
	if trimTS("") != "" || trimTS("open") != "open" {
		t.Fatalf("trimTS should preserve empty/open")
	}
	if got := trimTS("2026-05-14T12:34:56Z"); got != "2026-05-14 12:34" {
		t.Fatalf("trimTS RFC3339 = %q", got)
	}
	if got := trimTS("not-time"); got != "not-time" {
		t.Fatalf("trimTS invalid = %q", got)
	}
}
