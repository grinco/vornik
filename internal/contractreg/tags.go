package contractreg

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// reBuildTag matches a //go:build constraint line. Only the simple forms this
// repo actually uses are decomposed (space/comma/&&/||/! separated idents);
// anything exotic still yields its identifiers, which is all we need.
var reBuildTag = regexp.MustCompile(`^//go:build\s+(.+)$`)

var reTagIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// TagAudit is the third liveness axis: build tags.
//
// It exists because tag-gated code is INVISIBLE to call-graph analysis rather
// than merely unreachable. Code behind a tag that is never set is not compiled,
// so `deadcode` never sees it — it is absent from the analysis, not reported.
// Silent omission is worse than a false positive, and no union of main packages
// can speak about code that was never in a build.
//
// Cost is a directory walk and two greps: no compilation, no build graph.
type TagAudit struct {
	// FilesByTag maps a build tag to the files gated on it.
	FilesByTag map[string][]string
	// SetTags are tags referenced anywhere in the build/CI plumbing.
	SetTags map[string]bool
}

// NeverSet returns tags that gate at least one file but are set nowhere in the
// build plumbing — everything behind them is unbuilt and unrun.
func (a TagAudit) NeverSet() []string {
	var out []string
	for tag := range a.FilesByTag {
		if !a.SetTags[tag] {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

// AuditBuildTags walks root for //go:build constraints and scans the build
// plumbing (Makefile, .github/workflows) for tags ever set.
func AuditBuildTags(root string, plumbing []string) (TagAudit, error) {
	audit := TagAudit{FilesByTag: map[string][]string{}, SetTags: map[string]bool{}}

	// The root itself is never "uninteresting", even when its name matches the
	// export-copy prefix. The CE export lives in .vornik-export/ and its tests
	// run INSIDE it, so pruning by name alone skipped the whole tree: the audit
	// reported zero integration-tagged files in a tree holding 36, and the
	// export refused to publish on a parser that was blind rather than a real
	// fault.
	rootClean := filepath.Clean(root)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not fatal to the audit
		}
		if d.IsDir() {
			if filepath.Clean(path) == rootClean {
				return nil
			}
			return skipUninterestingDir(d)
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		collectFileTags(root, path, audit.FilesByTag)
		return nil
	})
	if err != nil {
		return audit, err
	}
	scanPlumbing(plumbing, audit.SetTags)
	return audit, nil
}

// skipUninterestingDir prunes VCS/vendor trees and the CE export/clone copies —
// those are duplicates of the real tree and would double-count every tag.
func skipUninterestingDir(d fs.DirEntry) error {
	switch d.Name() {
	case ".git", "node_modules", "vendor":
		return fs.SkipDir
	}
	if strings.HasPrefix(d.Name(), ".vornik-") {
		return fs.SkipDir
	}
	return nil
}

// collectFileTags records every build tag gating one file. Only the header is
// inspected: a //go:build constraint must precede the package clause, so parsing
// stops there rather than scanning whole files.
func collectFileTags(root, path string, into map[string][]string) {
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	rel, _ := filepath.Rel(root, path)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "//") && !strings.HasPrefix(line, "package") {
			return
		}
		m := reBuildTag.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, ident := range reTagIdent.FindAllString(m[1], -1) {
			if ident == "ignore" {
				continue
			}
			into[ident] = append(into[ident], rel)
		}
	}
}

// scanPlumbing marks tags referenced anywhere in the build/CI plumbing. Each
// entry may be a file (Makefile) or a directory (.github/workflows).
func scanPlumbing(plumbing []string, into map[string]bool) {
	for _, p := range plumbing {
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil
			}
			if body, rerr := os.ReadFile(path); rerr == nil {
				markTagsSet(string(body), into)
			}
			return nil
		})
		if body, rerr := os.ReadFile(p); rerr == nil {
			markTagsSet(string(body), into)
		}
	}
}

// reTagsFlag finds `-tags=a,b` / `-tags a,b` / `--tags=a` in build plumbing.
var reTagsFlag = regexp.MustCompile(`-{1,2}tags[= ]([A-Za-z0-9_,]+)`)

func markTagsSet(body string, into map[string]bool) {
	for _, m := range reTagsFlag.FindAllStringSubmatch(body, -1) {
		for _, tag := range strings.Split(m[1], ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				into[tag] = true
			}
		}
	}
}
