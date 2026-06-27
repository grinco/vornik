package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestTarGzDirRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "out.tgz")
	if err := TarGzDir(src, archive); err != nil {
		t.Fatalf("TarGzDir: %v", err)
	}
	if FileSize(archive) == 0 {
		t.Fatal("archive is empty")
	}

	dst := t.TempDir()
	if err := UntarGz(archive, dst); err != nil {
		t.Fatalf("UntarGz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "top.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("top.txt = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dst, "sub", "nested.txt"))
	if err != nil || string(got) != "world" {
		t.Fatalf("nested.txt = %q, %v", got, err)
	}
}

func TestUntarGzRejectsTraversal(t *testing.T) {
	// Hand-build a malicious archive with a ../ escape entry.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "../escape.txt",
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	archive := filepath.Join(t.TempDir(), "evil.tgz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := UntarGz(archive, dst); err == nil {
		t.Fatal("UntarGz accepted a traversal entry; want refusal")
	}
}

func TestUntarGzDropsSymlinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "link",
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	archive := filepath.Join(t.TempDir(), "link.tgz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := UntarGz(archive, dst); err != nil {
		t.Fatalf("UntarGz: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Fatal("symlink entry should not have been materialized")
	}
}

func TestCopyDirSkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	dst := t.TempDir()
	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link.txt")); !os.IsNotExist(err) {
		t.Fatal("symlink should have been skipped")
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "real.txt")); string(got) != "x" {
		t.Fatalf("real.txt not copied: %q", got)
	}
}

func TestCopyFileMissingSrc(t *testing.T) {
	if err := CopyFile(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "dst")); err == nil {
		t.Fatal("CopyFile of missing src should error")
	}
}

func TestTarGzDirMissing(t *testing.T) {
	if err := TarGzDir(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "out.tgz")); err == nil {
		t.Fatal("TarGzDir of missing dir should error")
	}
}

func TestFileSizeMissing(t *testing.T) {
	if got := FileSize(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Fatalf("FileSize of missing = %d, want 0", got)
	}
}

func TestUntarGzBadArchive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.tgz")
	if err := os.WriteFile(p, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UntarGz(p, t.TempDir()); err == nil {
		t.Fatal("UntarGz of non-gzip should error")
	}
}
