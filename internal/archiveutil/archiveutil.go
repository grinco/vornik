// Package archiveutil holds the tar.gz packaging + safe-extraction
// helpers shared by vornikctl backup/restore (internal/cli/backup.go)
// and the support-report bundle builder (internal/api +
// internal/cli/support_report.go).
//
// Why a shared package: both surfaces need identical tar.gz semantics
// and, critically, the SAME path-traversal / symlink guards on
// extraction. The support-report design (https://docs.vornik.io
// support-report-design.md §4.1 / §7) calls for reusing backup.go's
// tarGzDir + safe-path patterns rather than duplicating them; this
// package is that shared spot. Duplicating the guards risks one copy
// drifting from the other and re-opening a Zip-Slip hole.
package archiveutil

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CopyFile copies src to dst preserving mode bits. Parent
// directories of dst are created as needed.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err == nil {
		_ = os.Chmod(dst, info.Mode())
	}
	return nil
}

// CopyDir recursively copies src to dst. Symlinks are skipped (we
// never follow or materialize them — the same posture the safe
// extractor takes on link entries).
func CopyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return CopyFile(path, target)
	})
}

// TarGzDir packages dir's contents (not dir itself) into the gzip
// tarball at out. Only regular files and directories are emitted;
// symlinks and other special entries are skipped so the archive can't
// carry a link that a later extraction would follow.
func TarGzDir(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		// Skip symlinks / sockets / devices: only ship regular
		// files + dirs. tar.FileInfoHeader on a symlink would emit a
		// TypeSymlink the extractor drops anyway; skipping here keeps
		// the archive contents honest.
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = in.Close() }()
			if _, err := io.Copy(tw, in); err != nil {
				return err
			}
		}
		return nil
	})
}

// UntarGz extracts a tar.gz archive into dir. It rejects absolute
// paths and any entry whose joined target escapes the extraction root
// (Zip-Slip), and silently drops symlink / hardlink entries so a
// crafted archive can't redirect a later file write outside dir.
func UntarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("refusing entry with unsafe path: %s", hdr.Name)
		}
		target := filepath.Join(dir, hdr.Name)
		rel, relErr := filepath.Rel(dir, target)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing entry with unsafe path: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // size-bounded by caller's --max-size cap
				_ = out.Close()
				return err
			}
			_ = out.Close()
		}
	}
}

// FileSize returns the size of p in bytes, or 0 if it can't be
// stat'd.
func FileSize(p string) int64 {
	if info, err := os.Stat(p); err == nil {
		return info.Size()
	}
	return 0
}
