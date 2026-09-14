package configassist

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"vornik.io/vornik/internal/secrethygiene"
)

// Snapshot is the per-request, project-authorized, SANITIZED copy of the
// deployed config tree the assistant's loop is rooted at (design §2.2 as
// amended by review R7). The loop reads, greps and edits ONLY this copy;
// the deployed tree is never its root. Secret-bearing values are replaced
// by opaque placeholder tokens that are meaningless outside the request
// and re-materialised locally after the loop (Rematerialize), with any
// altered, duplicated, moved or dropped placeholder a refusal (test 35).
type Snapshot struct {
	// Root is the private directory the loop is rooted at.
	Root string
	// DeployedRoot is the real tree, read for pre-images and base hashes
	// (design §2.2 item 5) and never exposed to the loop.
	DeployedRoot string
	// Files is the relative path → original (sanitized) bytes at copy time.
	Files map[string][]byte
	// placeholders maps token → real value; the reverse map detects a
	// token that moved into a different field.
	placeholders map[string]placeholder
	limits       Limits
}

type placeholder struct {
	value string
	file  string
	key   string
}

// Limits bound the snapshot (review R7: byte and file limits).
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int
	MaxTotalBytes int
}

// DefaultLimits are conservative: a config tree is small by construction.
var DefaultLimits = Limits{MaxFiles: 2000, MaxFileBytes: 1 << 20, MaxTotalBytes: 32 << 20}

// Errors.
var (
	ErrSnapshotTooLarge    = errors.New("configassist: config tree exceeds the snapshot limits")
	ErrPlaceholderTampered = errors.New("configassist: a secret placeholder was altered, duplicated, moved or dropped; refusing to re-materialise")
	ErrPathOutsideSnapshot = errors.New("configassist: path escapes the snapshot")
	ErrSpecialFileInTree   = errors.New("configassist: special file in the config tree")
	ErrSecretsDirInTree    = errors.New("configassist: the secrets directory is not part of the snapshot")
	ErrAuthorizerRefused   = errors.New("configassist: caller is not authorized to read this file")
)

// Authorizer decides which relative paths a caller may see (review R6: a
// project-limited caller never sees shared files it may not read). Nil
// means "the whole tree".
type Authorizer func(rel string) bool

// placeholderRe matches the tokens Sanitize emits: fixed prefix + 24 hex.
var placeholderRe = regexp.MustCompile(`VORNIK_SECRET_PLACEHOLDER_[0-9a-f]{24}`)

// secretLineRe finds `key: value` lines whose value gets a placeholder when
// the key is secret-bearing (or is named_secrets' `value`).
var secretLineRe = regexp.MustCompile(`^(\s*-?\s*)([A-Za-z0-9_.-]+)(\s*:\s*)(.+?)\s*$`)

// Build copies the authorized subset of deployedRoot into a fresh private
// directory, sanitizing secrets. The caller must Close it.
//
//nolint:gocognit // Safety boundary: path, limit, symlink and secret checks are intentionally adjacent.
func Build(deployedRoot string, authorize Authorizer, limits Limits) (*Snapshot, error) {
	if limits.MaxFiles == 0 {
		limits = DefaultLimits
	}
	realRoot, err := filepath.EvalSymlinks(deployedRoot)
	if err != nil {
		return nil, fmt.Errorf("configassist: deployed root: %w", err)
	}
	root, err := os.MkdirTemp("", "vornik-configassist-")
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Root: root, DeployedRoot: realRoot, Files: map[string][]byte{}, placeholders: map[string]placeholder{}, limits: limits}
	total := 0
	walkErr := filepath.WalkDir(realRoot, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(realRoot, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		// The secrets directory is never part of the snapshot, by root and
		// by name (design §10a, test 31).
		if isSecretsPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Symlinks are not copied: a link out of the tree is exactly the
			// escape R7 names, and a link inside it is copied by its target.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(root, rel), 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrSpecialFileInTree, rel)
		}
		if authorize != nil && !authorize(filepath.ToSlash(rel)) {
			return nil // not authorized: absent from the snapshot, not refused
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Size() > int64(limits.MaxFileBytes) {
			return fmt.Errorf("%w: %s is %d bytes", ErrSnapshotTooLarge, rel, info.Size())
		}
		if len(s.Files) >= limits.MaxFiles {
			return fmt.Errorf("%w: more than %d files", ErrSnapshotTooLarge, limits.MaxFiles)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		total += len(data)
		if total > limits.MaxTotalBytes {
			return fmt.Errorf("%w: total exceeds %d bytes", ErrSnapshotTooLarge, limits.MaxTotalBytes)
		}
		sanitized := s.sanitize(filepath.ToSlash(rel), data)
		s.Files[filepath.ToSlash(rel)] = sanitized
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, sanitized, 0o600)
	})
	if walkErr != nil {
		_ = os.RemoveAll(root)
		return nil, walkErr
	}
	return s, nil
}

// Close removes the private root. Overlay cleanup is confined to it (R7).
func (s *Snapshot) Close() {
	if s == nil || s.Root == "" {
		return
	}
	_ = os.RemoveAll(s.Root)
}

func isSecretsPath(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range parts {
		if strings.EqualFold(p, "secrets") {
			return true
		}
	}
	return false
}

// sanitize replaces secret-bearing scalar values with placeholders. It is
// line-oriented over YAML/markdown-frontmatter text: `key: value` where the
// key is secret-bearing by name, OR the value looks like a raw secret, OR
// the key is `value` under a named_secrets list item (which is replaced
// UNCONDITIONALLY — design §10a). Placeholders are stable within the
// request only.
func (s *Snapshot) sanitize(rel string, data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	inNamedSecrets := false
	for i, line := range lines {
		text := string(line)
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "named_secrets:") {
			inNamedSecrets = true
			continue
		}
		if inNamedSecrets && trimmed != "" && !strings.HasPrefix(text, " ") && !strings.HasPrefix(text, "-") && !strings.HasPrefix(text, "\t") {
			inNamedSecrets = false
		}
		m := secretLineRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		key, val := m[2], m[4]
		unquoted := strings.Trim(val, `"'`)
		if unquoted == "" || strings.HasPrefix(unquoted, "${") || strings.HasPrefix(unquoted, "$") {
			continue // env reference: a NAME, editable, never a value
		}
		secretKey := (inNamedSecrets && key == "value") || secrethygiene.IsSecretBearingName(key) || secrethygiene.LooksLikeRawSecret(unquoted) && secrethygiene.IsSecretBearingName(strings.TrimSuffix(key, "_file"))
		if !secretKey {
			continue
		}
		tok := s.newPlaceholder(rel, key, unquoted)
		lines[i] = []byte(m[1] + key + m[3] + strings.Replace(val, unquoted, tok, 1))
	}
	return bytes.Join(lines, []byte("\n"))
}

func (s *Snapshot) newPlaceholder(rel, key, value string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	tok := "VORNIK_SECRET_PLACEHOLDER_" + hex.EncodeToString(b[:])
	s.placeholders[tok] = placeholder{value: value, file: rel, key: key}
	return tok
}

// Placeholders returns how many secrets were sanitized (for the result's
// record; never the values).
func (s *Snapshot) Placeholders() int { return len(s.placeholders) }

// Op is one create|replace operation the shipped apply engine consumes
// (design §3.1). Content is the FULL new text, re-materialised.
type Op struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Walk compares the (edited) snapshot against the ORIGINAL sanitized copy
// and returns the op bundle: a file present in the overlay and absent from
// the deployed tree is a create; one with different bytes is a replace;
// identical bytes emit no op; a file present in the deployed tree but
// absent from the overlay produces NO op (test 15 — no silent deletion;
// the deletions are reported separately so the operator sees the attempt).
// Contents are re-materialised: every placeholder is substituted back
// (test 35), and a tampered placeholder aborts the walk.
//
//nolint:gocognit // This keeps file-type, path and delete refusal checks in one audited walk.
func (s *Snapshot) Walk() (ops []Op, ignoredDeletions []string, err error) {
	seen := map[string]bool{}
	walkErr := filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != s.Root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrSpecialFileInTree, p)
		}
		rel, rerr := filepath.Rel(s.Root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if isSecretsPath(rel) {
			return fmt.Errorf("%w: %s", ErrSecretsDirInTree, rel)
		}
		seen[rel] = true
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if len(data) > s.limits.MaxFileBytes {
			return fmt.Errorf("%w: %s", ErrSnapshotTooLarge, rel)
		}
		orig, existed := s.Files[rel]
		if existed && bytes.Equal(orig, data) {
			return nil
		}
		content, rerr := s.Rematerialize(rel, data)
		if rerr != nil {
			return rerr
		}
		if existed {
			ops = append(ops, Op{Op: "replace", Path: rel, Content: content})
		} else {
			ops = append(ops, Op{Op: "create", Path: rel, Content: content})
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	for rel := range s.Files {
		if !seen[rel] {
			ignoredDeletions = append(ignoredDeletions, rel)
		}
	}
	sort.Strings(ignoredDeletions)
	sort.Slice(ops, func(i, j int) bool { return ops[i].Path < ops[j].Path })
	// Placeholder integrity across the WHOLE bundle: every placeholder the
	// original file carried must appear exactly once, in the same file, on
	// the same key; a placeholder must never appear in a file it did not
	// come from.
	if err := s.checkPlaceholderIntegrity(ops); err != nil {
		return nil, nil, err
	}
	return ops, ignoredDeletions, nil
}

// Rematerialize substitutes real secret values back into content. Every
// token in the content must be one this snapshot issued for THIS file.
func (s *Snapshot) Rematerialize(rel string, data []byte) (string, error) {
	out := string(data)
	for _, tok := range placeholderRe.FindAllString(out, -1) {
		ph, ok := s.placeholders[tok]
		if !ok || ph.file != rel {
			return "", fmt.Errorf("%w (%s in %s)", ErrPlaceholderTampered, tok[:len("VORNIK_SECRET_PLACEHOLDER_")+6], rel)
		}
		out = strings.Replace(out, tok, ph.value, 1)
	}
	return out, nil
}

// checkPlaceholderIntegrity: for each op's ORIGINAL file, every token
// present before must be present exactly once after, on a line whose key
// is the token's key; the re-materialised content must contain the value
// exactly once. Dropped, duplicated or moved placeholders refuse.
func (s *Snapshot) checkPlaceholderIntegrity(ops []Op) error {
	for _, op := range ops {
		orig := s.Files[op.Path]
		edited, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(op.Path)))
		if err != nil {
			return err
		}
		origToks := placeholderRe.FindAllString(string(orig), -1)
		editedToks := placeholderRe.FindAllString(string(edited), -1)
		count := map[string]int{}
		for _, t := range editedToks {
			count[t]++
		}
		for _, t := range origToks {
			if count[t] != 1 {
				return fmt.Errorf("%w (%s: placeholder for %s dropped or duplicated)", ErrPlaceholderTampered, op.Path, s.placeholders[t].key)
			}
			if !placeholderOnKey(string(edited), t, s.placeholders[t].key) {
				return fmt.Errorf("%w (%s: placeholder for %s moved to a different field)", ErrPlaceholderTampered, op.Path, s.placeholders[t].key)
			}
			delete(count, t)
		}
		for t := range count {
			ph, ok := s.placeholders[t]
			if !ok || ph.file != op.Path {
				return fmt.Errorf("%w (%s: foreign placeholder)", ErrPlaceholderTampered, op.Path)
			}
		}
	}
	return nil
}

// placeholderOnKey checks the line carrying tok still names key.
func placeholderOnKey(text, tok, key string) bool {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, tok) {
			continue
		}
		m := secretLineRe.FindStringSubmatch(line)
		return m != nil && m[2] == key
	}
	return false
}

// BaseHashes returns sha256 of the REAL deployed bytes for the given
// relative paths ("" when absent) — the pre-image record the apply engine's
// stale-base check and the journal's read set consume (design §2.2 item 5).
func (s *Snapshot) BaseHashes(rels []string) (map[string]string, error) {
	out := map[string]string{}
	for _, rel := range rels {
		full, err := s.deployedPath(rel)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			out[rel] = ""
			continue
		}
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

// deployedPath resolves rel under the deployed root, refusing escapes.
func (s *Snapshot) deployedPath(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrPathOutsideSnapshot, rel)
	}
	full := filepath.Join(s.DeployedRoot, clean)
	if !strings.HasPrefix(full, s.DeployedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrPathOutsideSnapshot, rel)
	}
	return full, nil
}

// TouchedFiles returns the relative paths of the ops.
func TouchedFiles(ops []Op) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Path)
	}
	return out
}
