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
	// baseHashes is the relative path → sha256 of the REAL (pre-sanitize)
	// bytes as captured, so the proposal's stale-base record authenticates
	// the content the model reasoned over rather than whatever the file
	// holds when the proposal is finally filed (audit 2026-09-15 CA-05).
	baseHashes map[string]string
	// externalBaseHashes records hashes for virtual files whose real backing
	// file is outside DeployedRoot.
	externalBaseHashes map[string]string
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
	// ErrSecretRedactionIncomplete means a secret-bearing field survived
	// redaction. The request is refused: a sanitiser that passes what it did
	// not understand reports "clean" and means "not examined" (re-audit
	// 2026-09-15, CA-03).
	ErrSecretRedactionIncomplete = errors.New("configassist: a secret-bearing field could not be redacted")
)

// Authorizer decides which relative paths a caller may see (review R6: a
// project-limited caller never sees shared files it may not read). Nil
// means "the whole tree".
type Authorizer func(rel string) bool

// placeholderRe matches the tokens Sanitize emits: fixed prefix + 24 hex.
// placeholderPrefix is the redaction token's fixed prefix. Named because the
// post-condition check in secretspans.go asks whether a value IS a
// placeholder.
const placeholderPrefix = "VORNIK_SECRET_PLACEHOLDER_"

var placeholderRe = regexp.MustCompile(placeholderPrefix + `[0-9a-f]{24}`)

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
	s := &Snapshot{Root: root, DeployedRoot: realRoot, Files: map[string][]byte{}, baseHashes: map[string]string{}, externalBaseHashes: map[string]string{}, placeholders: map[string]placeholder{}, limits: limits}
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
		if err := s.checkFileLimits(d, rel, limits); err != nil {
			return err
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		total += len(data)
		if total > limits.MaxTotalBytes {
			return fmt.Errorf("%w: total exceeds %d bytes", ErrSnapshotTooLarge, limits.MaxTotalBytes)
		}
		sanitized, leftover := s.capture(filepath.ToSlash(rel), data)
		if leftover != "" {
			// A secret-bearing field the redactor could not neutralise. The
			// snapshot is the model's whole view, so a file it cannot fully
			// redact makes the WHOLE request unsafe — refuse rather than ship
			// it (re-audit 2026-09-15, CA-03). The key is named; the value
			// never is.
			return fmt.Errorf("%w: %s in %s", ErrSecretRedactionIncomplete, leftover, filepath.ToSlash(rel))
		}
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

// checkFileLimits refuses a file that would breach the snapshot's per-file or
// file-count bounds (review R7).
func (s *Snapshot) checkFileLimits(d fs.DirEntry, rel string, limits Limits) error {
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
	return nil
}

// capture records a file's pre-image hash and returns its sanitized bytes.
//
// The hash is taken HERE, of the bytes the model is about to reason over —
// not re-read from disk when the proposal is filed, minutes later, after the
// assistant and the judge have run. Re-reading authenticated whatever the
// file had become, so an operator's concurrent edit passed the apply engine's
// stale-base check and was silently overwritten by a replacement computed
// from content that no longer existed (audit 2026-09-15 CA-05).
func (s *Snapshot) capture(rel string, data []byte) ([]byte, string) {
	sum := sha256.Sum256(data)
	s.baseHashes[rel] = hex.EncodeToString(sum[:])
	return s.sanitize(rel, data)
}

// AddExternalFile adds one real file from outside DeployedRoot to the private
// snapshot under a virtual relative path. The assistant sees and edits only the
// virtual path; BaseHashes still reports the real file's hash for stale-base
// checks.
func (s *Snapshot) AddExternalFile(rel, sourcePath string) error {
	if s == nil {
		return errors.New("configassist: nil snapshot")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || isSecretsPath(rel) {
		return fmt.Errorf("%w: %s", ErrPathOutsideSnapshot, rel)
	}
	rel = filepath.ToSlash(clean)
	data, err := os.ReadFile(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		s.externalBaseHashes[rel] = ""
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > s.limits.MaxFileBytes {
		return fmt.Errorf("%w: %s is %d bytes", ErrSnapshotTooLarge, rel, len(data))
	}
	sanitized, leftover := s.sanitize(rel, data)
	if leftover != "" {
		return fmt.Errorf("%w: %s in %s", ErrSecretRedactionIncomplete, leftover, rel)
	}
	s.Files[rel] = sanitized
	sum := sha256.Sum256(data)
	s.externalBaseHashes[rel] = hex.EncodeToString(sum[:])
	dst := filepath.Join(s.Root, clean)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, sanitized, 0o600)
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
//
// sanitize redacts every secret-bearing value in a file.
//
// It prefers the PARSER-driven span walker (secretspans.go), which identifies
// secret-bearing keys through decoded YAML rather than by matching text, and
// falls back to the historical line pass for documents the parser cannot read
// (markdown frontmatter, non-YAML files). Two audits found secrets reaching
// the model because a text matcher did not know a YAML representation —
// block scalars, then anchors and quoted keys — so identification is no
// longer a matcher's job (re-audit 2026-09-15, CA-03).
//
// Returns the sanitized bytes and the first secret-bearing key it could NOT
// redact, if any; the caller refuses the snapshot in that case.
func (s *Snapshot) sanitize(rel string, data []byte) ([]byte, string) {
	if spans, ok := secretSpans(data); ok {
		out := s.redactSpans(rel, data, spans)
		if leftover, bad := residualSecret(out); bad {
			return out, leftover
		}
		return out, ""
	}
	return s.sanitizeLines(rel, data), ""
}

// redactSpans replaces each located value with one placeholder, editing TEXT
// so everything it does not touch stays byte-identical and re-materialisation
// restores the original exactly.
func (s *Snapshot) redactSpans(rel string, data []byte, spans []secretSpan) []byte {
	if len(spans) == 0 {
		return data
	}
	lines := bytes.Split(data, []byte("\n"))
	drop := map[int]bool{}
	for _, sp := range spans {
		start := sp.startLine - 1
		if start < 0 || start >= len(lines) {
			continue
		}
		header := string(lines[start])
		if sp.valueCol > len(header) {
			continue
		}
		// The original value text: the remainder of the header line plus
		// every continuation line. Anchors and tags are INSIDE this span, so
		// restoring it restores them too.
		var b strings.Builder
		b.WriteString(header[sp.valueCol:])
		for j := start + 1; j <= sp.endLine-1 && j < len(lines); j++ {
			b.WriteString("\n")
			b.WriteString(string(lines[j]))
			drop[j] = true
		}
		tok := s.newPlaceholder(rel, sp.key, b.String())
		prefix := header[:sp.valueCol]
		if sp.valueCol >= len(header) && !strings.HasSuffix(prefix, " ") {
			// The value began on a later line; YAML needs a space after the
			// colon or `key:TOKEN` parses as a scalar, not a mapping.
			prefix += " "
		}
		lines[start] = []byte(prefix + tok)
	}
	out := make([][]byte, 0, len(lines))
	for i, l := range lines {
		if drop[i] {
			continue
		}
		out = append(out, l)
	}
	return bytes.Join(out, []byte("\n"))
}

//nolint:gocognit // One pass over the document: the block-scalar branch has to see the same line state as the scalar branch.
func (s *Snapshot) sanitizeLines(rel string, data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	inNamedSecrets := false
	for i := 0; i < len(lines); i++ {
		text := string(lines[i])
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
		// A BLOCK SCALAR carries its value on the following indented lines,
		// not on this one. Replacing just the `|` marker left the secret
		// body sitting in the snapshot, in plain sight, and file_read then
		// handed it to the model — defeating the unconditional redaction
		// §10a promises for named_secrets[].value (audit 2026-09-15 CA-03).
		// The whole block is one value and is redacted as one.
		if isBlockScalarIndicator(unquoted) {
			if !s.secretBearing(inNamedSecrets, key, "") {
				continue
			}
			body, end := blockScalarBody(lines, i)
			if end == i {
				continue // an indicator with no body: nothing to redact
			}
			// The placeholder stands in for the VALUE — the indicator plus
			// its body — so re-materialisation rebuilds the header line from
			// the same prefix/key/separator it was written with.
			tok := s.newPlaceholder(rel, key, val+"\n"+body)
			lines[i] = []byte(m[1] + key + m[3] + tok)
			lines = append(lines[:i+1], lines[end+1:]...)
			continue
		}
		if !s.secretBearing(inNamedSecrets, key, unquoted) {
			continue
		}
		tok := s.newPlaceholder(rel, key, unquoted)
		lines[i] = []byte(m[1] + key + m[3] + strings.Replace(val, unquoted, tok, 1))
	}
	return bytes.Join(lines, []byte("\n"))
}

// secretBearing is the single rule for "this field holds a secret". Passing
// an empty value asks the question by KEY alone, which is what a block
// scalar needs: its value has not been read yet, and a block whose body
// merely fails to look random is still a declared secret.
func (s *Snapshot) secretBearing(inNamedSecrets bool, key, value string) bool {
	if inNamedSecrets && key == "value" {
		return true // §10a: unconditional
	}
	if secrethygiene.IsSecretBearingName(key) {
		return true
	}
	if value == "" {
		return false
	}
	return secrethygiene.LooksLikeRawSecret(value) && secrethygiene.IsSecretBearingName(strings.TrimSuffix(key, "_file"))
}

// isBlockScalarIndicator reports whether a YAML value position holds a block
// scalar header: `|` or `>`, with any chomping (`-`, `+`) and explicit
// indentation digit, optionally followed by a comment.
func isBlockScalarIndicator(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || (v[0] != '|' && v[0] != '>') {
		return false
	}
	rest := strings.TrimSpace(v[1:])
	if idx := strings.IndexByte(rest, '#'); idx >= 0 {
		rest = strings.TrimSpace(rest[:idx])
	}
	for _, r := range rest {
		if r != '-' && r != '+' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// blockScalarBody returns the block's body text and the index of its LAST
// line, given the index of the header line. The body is every following line
// that is blank or indented deeper than the header — the YAML rule — so the
// scan stops at the next key at or above the header's indentation.
func blockScalarBody(lines [][]byte, header int) (string, int) {
	headerIndent := indentWidth(string(lines[header]))
	end := header
	var body []string
	for j := header + 1; j < len(lines); j++ {
		text := string(lines[j])
		if strings.TrimSpace(text) == "" {
			// A blank line belongs to the block only if the block continues
			// after it; look ahead rather than ending on trailing blanks.
			continues := false
			for k := j + 1; k < len(lines); k++ {
				if strings.TrimSpace(string(lines[k])) == "" {
					continue
				}
				continues = indentWidth(string(lines[k])) > headerIndent
				break
			}
			if !continues {
				break
			}
			body = append(body, text)
			continue
		}
		if indentWidth(text) <= headerIndent {
			break
		}
		body = append(body, text)
		end = j
	}
	// Drop any trailing blanks that were provisionally collected.
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	return strings.Join(body, "\n"), end
}

func indentWidth(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8
		default:
			return n
		}
	}
	return n // an all-whitespace line
}

func (s *Snapshot) newPlaceholder(rel, key, value string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	tok := placeholderPrefix + hex.EncodeToString(b[:])
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
			return "", fmt.Errorf("%w (%s in %s)", ErrPlaceholderTampered, tok[:len(placeholderPrefix)+6], rel)
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

// BaseHashes returns sha256 of the REAL bytes this snapshot captured for the
// given relative paths ("" when absent) — the pre-image record the apply
// engine's stale-base check and the journal's read set consume (design §2.2
// item 5).
//
// The hash is the one taken AT CAPTURE, so it authenticates the content the
// proposal was actually derived from. A path with no captured hash (one the
// snapshot never read — a file the assistant creates) still falls through to
// the deployed tree, where "absent" is the meaningful answer.
func (s *Snapshot) BaseHashes(rels []string) (map[string]string, error) {
	out := map[string]string{}
	for _, rel := range rels {
		if h, ok := s.externalBaseHashes[rel]; ok {
			out[rel] = h
			continue
		}
		if h, ok := s.baseHashes[rel]; ok {
			out[rel] = h
			continue
		}
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
