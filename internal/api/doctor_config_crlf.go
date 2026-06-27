package api

// Doctor check: config_crlf.
//
// The web UI's YAML writer historically emitted CRLF (\r\n) line endings
// when an operator edited a swarm/project/workflow through the dashboard.
// The daemon parses CRLF fine, but `git`/diff tooling treats the deployed
// copy as changed against the LF source tree, producing phantom source↔
// deployed drift (the 72f6e1aa reconcile commit cleaned a batch of these).
// This check scans the deployed config tree for files carrying CR bytes and,
// with --fix, strips them in place via an atomic temp+rename so a half-written
// file can never be left behind.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// crlfScanMaxBytes bounds how much of a single file we read when sniffing for
// CR bytes. Config YAML/MD files are small; anything past this is almost
// certainly not a hand-edited config and we skip it to keep the check cheap.
const crlfScanMaxBytes = 4 << 20 // 4 MiB

// crlfScanExtensions limits the scan to text config formats the UI writes.
// Binary blobs (e.g. a stray image) never legitimately contain "\r\n" as a
// line ending we'd want to rewrite, so excluding them avoids corrupting them.
var crlfScanExtensions = map[string]bool{
	".yaml": true,
	".yml":  true,
	".md":   true,
	".json": true,
}

// checkConfigCRLF scans the deployed config dir (and the daemon's config.yaml)
// for files containing CRLF line endings. WARNING per finding; --fix strips
// the CR bytes in place and reports the count.
func (h *DoctorHandlers) checkConfigCRLF(fix bool) DoctorCheck {
	name := "config_crlf"

	files := h.collectCRLFScanFiles()
	if len(files) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config files to scan (config dir/path not wired)"}
	}

	var tainted []string
	for _, f := range files {
		has, err := fileHasCRLF(f)
		if err != nil || !has {
			continue
		}
		tainted = append(tainted, f)
	}
	sort.Strings(tainted)

	if len(tainted) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("no CRLF line endings across %d config file(s)", len(files))}
	}

	check := DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d config file(s) contain CRLF line endings (UI YAML-writer drift)", len(tainted)),
	}

	items := make([]string, 0, len(tainted))
	if fix {
		for _, f := range tainted {
			if err := stripCRLFInPlace(f); err != nil {
				items = append(items, fmt.Sprintf("%s (strip failed: %v)", relForDisplay(h.configDir, f), err))
				continue
			}
			check.Fixed++
			items = append(items, relForDisplay(h.configDir, f)+" (CR stripped)")
		}
		check.Message = fmt.Sprintf("%d CRLF file(s) found, %d normalized to LF", len(tainted), check.Fixed)
		if check.Fixed == len(tainted) {
			check.Status = "OK"
		}
	} else {
		for _, f := range tainted {
			items = append(items, relForDisplay(h.configDir, f))
		}
	}
	check.Items = items
	return check
}

// collectCRLFScanFiles returns the deduplicated set of text config files to
// scan: every eligible file under the config dir tree plus the daemon's
// explicit config.yaml path (which may live outside the dir).
func (h *DoctorHandlers) collectCRLFScanFiles() []string {
	var files []string
	seen := map[string]bool{}
	addFile := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		files = append(files, p)
	}

	if h.configDir != "" {
		_ = filepath.Walk(h.configDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !crlfScanExtensions[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			addFile(path)
			return nil
		})
	}
	if h.configPath != "" {
		if info, err := os.Stat(h.configPath); err == nil && !info.IsDir() {
			addFile(h.configPath)
		}
	}
	return files
}

// relForDisplay renders a file path relative to the config dir when possible,
// so Items stay readable; falls back to the absolute path otherwise.
func relForDisplay(root, path string) string {
	if root == "" {
		return path
	}
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// fileHasCRLF reports whether the file contains a "\r\n" sequence within the
// first crlfScanMaxBytes bytes.
func fileHasCRLF(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() > crlfScanMaxBytes {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return bytes.Contains(data, []byte("\r\n")), nil
}

// stripCRLFInPlace rewrites the file with all "\r\n" replaced by "\n",
// preserving the original file mode, via an atomic temp+rename so a crash
// mid-write can never leave a truncated config behind.
func stripCRLFInPlace(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cleaned := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(cleaned, data) {
		return nil
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".crlf-fix-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(cleaned); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
