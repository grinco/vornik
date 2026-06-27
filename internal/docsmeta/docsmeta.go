// Package docsmeta handles the provenance front-matter on customer-docs pages
// under docs/public/. A narrative page anchors itself to the internal
// source(s) it was derived from, with a content hash per source:
//
//	---
//	sources:
//	  - path: docs/operator/cost-and-caching.md
//	    sha256: <hash at last review>
//	---
//
// The staleness lint (cmd/lint-lld-contracts) fails when a source's current
// hash differs from the anchor — the source moved, the doc may not have. The
// stamp tool (cmd/docs-gen stamp) re-anchors after a human review.
//
// Front-matter on these pages MUST contain only `sources:` — Restamp
// re-marshals the front-matter and would drop unrecognised keys.
package docsmeta

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// forbiddenPatterns are GENERIC structural markers of internal IP that must
// never appear in a published customer-docs page: Go internal package paths,
// LLD/audit references, schema migration numbers, and internal Prometheus
// metric names. They must carry NO operator-identity token — the employer,
// private org/domain, operator handle, and source-repo URL are supplied per
// call from scripts/docs-ip-denylist.txt (the single source of truth, pruned
// from every public export). This package SHIPS to the public CE via
// cmd/docs-gen, so a hardcoded identity token here would itself be a leak —
// see the CE-export IP-leak fix (2026-06-27) and the guard test
// TestForbiddenPatterns_NoHardcodedOperatorIdentity.
var forbiddenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`internal/[a-z]`),
	regexp.MustCompile(`docs/low-level-design`),
	regexp.MustCompile(`AUDIT-\d`),
	regexp.MustCompile(`\bmigration \d+`),
	regexp.MustCompile(`vornik_[a-z]+_(?:total|seconds|count|bucket)`),
}

// ForbiddenHits returns human-readable descriptions of any lines in text that
// match a structural IP marker or one of the extra denylist substrings
// (matched case-insensitively). An empty result means the text is clean.
func ForbiddenHits(text string, denySubstrings []string) []string {
	lower := make([]string, len(denySubstrings))
	for i, s := range denySubstrings {
		lower[i] = strings.ToLower(s)
	}
	var hits []string
	for n, line := range strings.Split(text, "\n") {
		ll := strings.ToLower(line)
		for _, re := range forbiddenPatterns {
			if m := re.FindString(line); m != "" {
				hits = append(hits, fmt.Sprintf("line %d: %q (matched /%s/)", n+1, strings.TrimSpace(line), re.String()))
			}
		}
		for _, sub := range lower {
			if sub != "" && strings.Contains(ll, sub) {
				hits = append(hits, fmt.Sprintf("line %d: %q (denylisted %q)", n+1, strings.TrimSpace(line), sub))
			}
		}
	}
	return hits
}

// LoadDenylist reads newline-separated substrings (blank lines and #-comments
// ignored). A missing file yields an empty list (no error) so the guard still
// runs with just its built-in patterns.
func LoadDenylist(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// Source is one provenance anchor: an internal file/dir and its hash at the
// time the doc was last reviewed.
type Source struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// Frontmatter is the parsed leading YAML block of a docs page.
type Frontmatter struct {
	Sources []Source `yaml:"sources"`
}

const fence = "---\n"

// SplitFrontmatter separates a leading "---\n...\n---\n" block from the body.
// had is false (and body == md) when there is no front-matter.
func SplitFrontmatter(md []byte) (fmBytes, body []byte, had bool) {
	s := string(md)
	if !strings.HasPrefix(s, fence) {
		return nil, md, false
	}
	rest := s[len(fence):]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return nil, md, false
	}
	return []byte(rest[:end]), []byte(rest[end+len("\n"+fence):]), true
}

// ParseFrontmatter returns the parsed front-matter and whether any was present.
func ParseFrontmatter(md []byte) (Frontmatter, bool, error) {
	fmBytes, _, had := SplitFrontmatter(md)
	if !had {
		return Frontmatter{}, false, nil
	}
	var fm Frontmatter
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return Frontmatter{}, true, err
	}
	return fm, true, nil
}

// HashPath returns a hex sha256 of a file, or a composite hash of a directory
// tree (sorted "relpath\tfilehash" lines) so adding, removing, or changing any
// file under a directory source changes the hash.
func HashPath(p string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return hashFile(p)
	}
	var lines []string
	walkErr := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		h, herr := hashFile(path)
		if herr != nil {
			return herr
		}
		rel, _ := filepath.Rel(p, path)
		lines = append(lines, rel+"\t"+h)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func hashFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// StaleSources returns the source paths whose current hash differs from the
// page's anchored hash. A page with no `sources:` block returns nil — it is
// not anchored, so it is not checked.
func StaleSources(root, mdPath string) ([]string, error) {
	b, err := os.ReadFile(mdPath)
	if err != nil {
		return nil, err
	}
	fm, had, err := ParseFrontmatter(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", mdPath, err)
	}
	if !had || len(fm.Sources) == 0 {
		return nil, nil
	}
	var stale []string
	for _, s := range fm.Sources {
		cur, err := HashPath(filepath.Join(root, s.Path))
		if err != nil {
			return nil, fmt.Errorf("%s: source %q: %w", mdPath, s.Path, err)
		}
		if cur != s.SHA256 {
			stale = append(stale, s.Path)
		}
	}
	return stale, nil
}

// Restamp rewrites each source's sha256 in the page's front-matter to the
// current hash, preserving the body. Returns true if the file changed. Pages
// without a `sources:` block are left untouched.
func Restamp(root, mdPath string) (bool, error) {
	b, err := os.ReadFile(mdPath)
	if err != nil {
		return false, err
	}
	fmBytes, body, had := SplitFrontmatter(b)
	if !had {
		return false, nil
	}
	var fm Frontmatter
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return false, fmt.Errorf("%s: %w", mdPath, err)
	}
	if len(fm.Sources) == 0 {
		return false, nil
	}
	for i := range fm.Sources {
		h, err := HashPath(filepath.Join(root, fm.Sources[i].Path))
		if err != nil {
			return false, fmt.Errorf("%s: source %q: %w", mdPath, fm.Sources[i].Path, err)
		}
		fm.Sources[i].SHA256 = h
	}
	out, err := yaml.Marshal(fm)
	if err != nil {
		return false, err
	}
	newContent := fence + string(out) + fence + string(body)
	if newContent == string(b) {
		return false, nil
	}
	return true, os.WriteFile(mdPath, []byte(newContent), 0o644)
}
