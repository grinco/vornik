package agentloop

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func init() {
	Handlers["file_read"] = fileRead
	Handlers["file_write"] = fileWrite
	Handlers["file_edit"] = fileEdit
	Handlers["read_many_files"] = readManyFiles
}

const (
	fileReadCap      = 30000  // bytes — bash `head -c`
	readManyFileCap  = 30000  // characters — python str slicing
	readManyTotalCap = 120000 // characters
)

func fileRead(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	path := a.str("path", "")
	if path == "" || path == "null" {
		return "ERROR: path is required"
	}
	resolved, err := resolvePath(env.Workspace, path)
	if err != nil {
		return err.Error()
	}
	if strings.HasPrefix(resolved, toolResultsDir(env.Workspace)) {
		return "ERROR: .tool_results is only readable through tool_result_read"
	}
	if !isRegularFile(resolved) {
		return "ERROR: file not found: " + resolved
	}
	data, size, err := readFilePrefix(resolved, fileReadCap)
	if err != nil {
		return "ERROR: file not found: " + resolved
	}
	if len(data) > fileReadCap {
		// Cap file output to 30KB to avoid blowing up the LLM context
		// window. Large files cause degenerate tool loops.
		return string(data[:fileReadCap]) + fmt.Sprintf("\n\n[... truncated at 30KB, total %d bytes]", size)
	}
	return string(data)
}

func fileWrite(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	path := a.str("path", "")
	content := a.str("content", "")
	if path == "" || path == "null" {
		return "ERROR: path is required"
	}
	if content == "" || content == "null" {
		return "ERROR: content is required for file_write. If the content was cut off, your context window may be exhausted — try writing a shorter version of the file, or break it into multiple smaller file_write calls."
	}
	resolved, err := resolvePath(env.Workspace, path)
	if err != nil {
		return err.Error()
	}
	root, rel, err := workspaceRoot(env.Workspace, resolved)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return "ERROR: " + err.Error()
	}
	if err := root.WriteFile(rel, []byte(content), 0o644); err != nil {
		return "ERROR: " + err.Error()
	}
	return fmt.Sprintf("OK: wrote %d bytes to %s", len(content), resolved)
}

// fileEdit is the exact-string replace. D1 (design §3.1): the python
// implementation decoded with errors="replace" and re-encoded, so an invalid
// UTF-8 byte in an edited file became U+FFFD; this works on bytes and leaves
// it. The reported length is a character count, as python's len() was.
func fileEdit(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	path := a.str("path", "")
	oldStr := a.str("old_string", "")
	newStr := a.str("new_string", "")
	replaceAll := a.boolFlag("replace_all")
	if path == "" || path == "null" {
		return "ERROR: path is required"
	}
	if oldStr == "" {
		return "ERROR: old_string is required (empty match would match everywhere)"
	}
	resolved, err := resolvePath(env.Workspace, path)
	if err != nil {
		return err.Error()
	}
	root, rel, err := workspaceRoot(env.Workspace, resolved)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer func() { _ = root.Close() }()
	st, err := root.Stat(rel)
	if err != nil || !st.Mode().IsRegular() {
		return "ERROR: file not found: " + resolved
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return "ERROR: file not found: " + resolved
	}
	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "ERROR: old_string not found in file"
	}
	if count > 1 && !replaceAll {
		return fmt.Sprintf("ERROR: old_string matches %d times — pass replace_all=true to replace every occurrence, or provide a longer old_string that uniquely identifies the location", count)
	}
	var updated string
	replaced := 1
	if replaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
		replaced = count
	} else {
		updated = strings.Replace(content, oldStr, newStr, 1)
	}
	tmp := filepath.Join(filepath.Dir(rel), ".vornik-edit-"+rand.Text())
	if err := replaceInRoot(root, rel, tmp, []byte(updated), st.Mode().Perm()); err != nil {
		return "ERROR: " + err.Error()
	}
	return fmt.Sprintf("OK: replaced %d occurrence(s) in %s (%d bytes)", replaced, resolved, utf8.RuneCountInString(updated))
}

func readManyFiles(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	paths := a.strList("paths")
	if len(paths) == 0 {
		return "ERROR: paths array is required"
	}
	ws := realpath(env.Workspace)
	var parts []string
	total := 0
	for _, p := range paths {
		if total >= readManyTotalCap {
			parts = append(parts, fmt.Sprintf("===== SKIPPED (total cap reached): %s =====", p))
			continue
		}
		resolved, err := resolvePath(ws, p)
		if err != nil {
			parts = append(parts, fmt.Sprintf("===== ERROR: path escapes workspace: %s =====", p))
			continue
		}
		if !isRegularFile(resolved) {
			parts = append(parts, fmt.Sprintf("===== FILE: %s =====", p), "ERROR: file not found")
			continue
		}
		// The cap is CHARACTERS (python sliced the decoded text), so the
		// bounded read has to admit up to UTFMax bytes per character plus one
		// sentinel character: 120,001 bytes, still O(cap) and independent of
		// the file's length. Reading cap+1 BYTES here (01cbba7a, kept by the
		// bounded-read fix) truncated a 40,000-byte file of 20,000 two-byte
		// runes that python returned whole and split the last rune, handing
		// the model invalid UTF-8 (found 2026-09-05 validating the audit).
		data, size, err := readFilePrefix(resolved, readManyFileCap*utf8.UTFMax)
		if err != nil {
			parts = append(parts, fmt.Sprintf("===== FILE: %s =====", p), "ERROR: "+err.Error())
			continue
		}
		text := string(data)
		truncated := utf8.RuneCountInString(text) > readManyFileCap
		if truncated {
			text = runeSlice(text, readManyFileCap)
		}
		parts = append(parts, fmt.Sprintf("===== FILE: %s =====", p), text)
		if truncated {
			parts = append(parts, fmt.Sprintf("[... truncated at 30KB, total %d bytes]", size))
		}
		total += utf8.RuneCountInString(text)
	}
	body := strings.Join(parts, "\n")
	if utf8.RuneCountInString(body) > readManyTotalCap {
		body = runeSlice(body, readManyTotalCap) + "\n[... output truncated at 120KB]"
	}
	return body
}

// workspaceRoot keeps compatibility path resolution separate from the actual
// I/O boundary. Root rejects symlinks escaping the workspace, including links
// with missing targets and links swapped after resolvePath (audit 2026-09-05).
func workspaceRoot(workspace, resolved string) (*os.Root, string, error) {
	ws := realpath(workspace)
	rel, err := filepath.Rel(ws, resolved)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(ws)
	return root, rel, err
}

// replaceInRoot creates exclusively before registering cleanup: a preexisting
// file (including a symlink) must never be overwritten or removed on collision.
func replaceInRoot(root *os.Root, name, tmp string, data []byte, mode os.FileMode) error {
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// readFilePrefix bounds input allocation independently of the file's length.
// The extra byte detects truncation even if stat size is stale or unavailable
// for a virtual file; size is informational and never smaller than bytes read.
func readFilePrefix(path string, limitBytes int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limitBytes)+1))
	if err != nil {
		return nil, 0, err
	}
	return data, max(st.Size(), int64(len(data))), nil
}
