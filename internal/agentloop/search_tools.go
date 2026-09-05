package agentloop

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func init() {
	Handlers["grep"] = grepTool
	Handlers["glob"] = globTool
}

// skipDirs mirrors the python SKIP_DIRS set.
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, ".mypy_cache": true, "dist": true, "build": true}

// fnmatchRE compiles a python fnmatch pattern: `*` and `?` match any character
// including "/", `[...]` and `[!...]` are classes, everything else is literal,
// and the match is anchored (fnmatch.translate wraps it in (?s:…)\Z).
func fnmatchRE(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`^(?s:`)
	i := 0
	for i < len(pat) {
		c, width := utf8.DecodeRuneInString(pat[i:])
		i += width
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < len(pat) && pat[j] == '!' {
				j++
			}
			if j < len(pat) && pat[j] == ']' {
				j++
			}
			for j < len(pat) && pat[j] != ']' {
				j++
			}
			if j >= len(pat) {
				b.WriteString(`\[`)
				continue
			}
			stuff := pat[i:j]
			i = j + 1
			if stuff == "" {
				b.WriteString(neverMatches) // python 3.14: an empty set matches nothing
				continue
			}
			switch stuff[0] {
			case '!':
				stuff = "^" + stuff[1:]
			case '^':
				stuff = `\` + stuff
			}
			stuff = strings.ReplaceAll(stuff, `\`, `\\`)
			b.WriteString("[" + stuff + "]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile("^" + neverMatches) // never matches, as a hopeless pattern did
	}
	return re
}

// neverMatches is RE2's spelling of an empty set: a negated class of every
// code point (RE2 has no (?!) lookahead).
const neverMatches = `[^\x00-\x{10FFFF}]`

func fnmatch(name, pat string) bool { return fnmatchRE(pat).MatchString(name) }

// grepQuery is the parsed arguments of one grep call.
type grepQuery struct {
	root, mode string
	workspace  string
	re         *regexp.Regexp
	globRE     *regexp.Regexp // nil when no glob was given
	globBase   *regexp.Regexp // the "**/" approximation, nil otherwise
	head       int
}

// grepTool. D2 (design §3.1): python `re` → Go RE2; backreferences and
// lookarounds are rejected with the same "ERROR: invalid regex" line the
// python path produced for a syntactically invalid pattern.
func grepTool(env Env, raw json.RawMessage) string {
	q, refusal := parseGrepQuery(env, decodeArgs(raw))
	if refusal != "" {
		return refusal
	}
	results, fileCounts := q.run()
	if q.mode == "count" {
		for _, p := range sortedKeys(fileCounts) {
			results = append(results, fmt.Sprintf("%s:%d", p, fileCounts[p]))
		}
	}
	if len(results) == 0 {
		return "(no matches)"
	}
	shown := results
	if len(shown) > q.head {
		shown = shown[:q.head]
	}
	out := strings.Join(shown, "\n")
	if len(results) > q.head {
		out += fmt.Sprintf("\n[... truncated at %d of %d results]", q.head, len(results))
	}
	return out
}

func parseGrepQuery(env Env, a args) (*grepQuery, string) {
	pattern := a.str("pattern", "")
	if pattern == "" || pattern == "null" {
		return nil, "ERROR: pattern is required"
	}
	ws := realpath(env.Workspace)
	q := &grepQuery{workspace: ws, root: ws, mode: a.str("output_mode", "files_with_matches"), head: a.intOr("head_limit", 200)}
	if q.head < 1 {
		return nil, "ERROR: head_limit must be at least 1"
	}
	if p := a.str("path", ""); p != "" && p != "null" {
		resolved, err := resolvePath(ws, p)
		if err != nil {
			return nil, err.Error()
		}
		q.root = resolved
	}
	if q.mode == "" {
		q.mode = "files_with_matches"
	}
	expr := pattern
	if a.boolFlag("ignore_case") {
		expr = "(?i)" + pattern
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, "ERROR: invalid regex: " + strings.TrimPrefix(err.Error(), "error parsing regexp: ")
	}
	q.re = re
	if globPat := a.str("glob", ""); globPat != "" {
		q.globRE = fnmatchRE(globPat)
		if strings.HasPrefix(globPat, "**/") {
			q.globBase = fnmatchRE(globPat[3:])
		}
	}
	return q, ""
}

func (q *grepQuery) matchesGlob(rel string) bool {
	if q.globRE == nil {
		return true
	}
	if q.globRE.MatchString(rel) || q.globRE.MatchString(filepath.Base(rel)) {
		return true
	}
	return q.globBase != nil && (q.globBase.MatchString(rel) || q.globBase.MatchString(filepath.Base(rel)))
}

// run walks the tree in os.walk order and returns the result lines (content
// or file mode) and the per-file counts (count mode).
func (q *grepQuery) run() ([]string, map[string]int) {
	var results []string
	fileCounts := map[string]int{}
	root, err := os.OpenRoot(q.workspace)
	if err != nil {
		return results, fileCounts
	}
	defer func() { _ = root.Close() }()
	walkDirs(q.root, func(dir string, files []string) bool {
		for _, name := range files {
			fpath := filepath.Join(dir, name)
			rel := relTo(q.root, fpath)
			if !q.matchesGlob(rel) {
				continue
			}
			matched, stop := q.scanFile(root, fpath, rel, &results)
			if matched > 0 {
				switch q.mode {
				case "files_with_matches":
					results = append(results, rel)
					stop = stop || len(results) >= q.head
				case "count":
					fileCounts[rel] = matched
				}
			}
			if stop {
				return false
			}
		}
		return true
	})
	return results, fileCounts
}

// scanFile counts matching lines in one file, appending content-mode lines to
// results; stop reports that the head limit was reached mid-file.
func (q *grepQuery) scanFile(root *os.Root, fpath, rel string, results *[]string) (matched int, stop bool) {
	// Resolve existing internal absolute links for compatibility, but perform
	// actual I/O through Root: lexical validation alone races a symlink swap.
	name := relTo(q.workspace, realpath(fpath))
	st, err := root.Stat(name)
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	f, err := root.Open(name)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := sc.Text()
		if !q.re.MatchString(line) {
			continue
		}
		matched++
		if q.mode != "content" {
			continue
		}
		*results = append(*results, fmt.Sprintf("%s:%d:%s", rel, lineno, strings.TrimRight(line, " \t\r\n\f\v")))
		if len(*results) >= q.head {
			return matched, true
		}
	}
	return matched, false
}

// walkDirs is os.walk(root) top-down in directory order, not following
// symlinked directories, pruning skipDirs, and visiting files (including
// symlinks to files and broken links) in the order the directory lists them.
// visit returns false to stop the whole walk.
func walkDirs(root string, visit func(dir string, files []string) bool) {
	var walk func(dir string) bool
	walk = func(dir string) bool {
		d, err := os.Open(dir)
		if err != nil {
			return true
		}
		names, err := d.Readdirnames(-1)
		_ = d.Close()
		if err != nil {
			return true
		}
		// Lexical, not directory order (D5, 2026-09-05). Directory order is a
		// property of the filesystem, not of the tool: the goldens recorded on
		// the reference host's filesystem listed sub/two.md before sub/one.txt,
		// and CI's ext4 listed them the other way, so four grep fixtures could
		// not pass on both. python's os.walk had the same non-determinism; the
		// model never depended on it, and a golden cannot tolerate it.
		sort.Strings(names)
		var dirs, files []string
		for _, n := range names {
			full := filepath.Join(dir, n)
			if isDir(full) {
				if !skipDirs[n] {
					dirs = append(dirs, n)
				}
			} else {
				files = append(files, n)
			}
		}
		if !visit(dir, files) {
			return false
		}
		for _, n := range dirs {
			full := filepath.Join(dir, n)
			if l, err := os.Lstat(full); err == nil && l.Mode()&os.ModeSymlink != 0 {
				continue // os.walk(followlinks=False)
			}
			if !walk(full) {
				return false
			}
		}
		return true
	}
	walk(root)
}

// globTool. D3 (design §3.1): python glob.glob(recursive=True) semantics — a
// `**` segment matches zero or more directories, `*` never a leading dot
// unless the pattern segment does, directory symlinks are followed, and the
// result is files only, newest mtime first, capped at 500.
func globTool(env Env, raw json.RawMessage) string {
	a := decodeArgs(raw)
	pattern := a.str("pattern", "")
	if pattern == "" || pattern == "null" {
		return "ERROR: pattern is required"
	}
	ws := realpath(env.Workspace)
	root := ws
	if p := a.str("path", ""); p != "" && p != "null" {
		resolved, err := resolvePath(ws, p)
		if err != nil {
			return err.Error()
		}
		root = resolved
	}
	type entry struct {
		mtime time.Time
		rel   string
	}
	var entries []entry
	for _, rel := range pyGlob(root, pattern) {
		full := filepath.Join(root, rel)
		st, err := os.Stat(full)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		entries = append(entries, entry{mtime: st.ModTime(), rel: rel})
	}
	// (mtime, path) tuples sorted in reverse: newest first, ties by path descending.
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].mtime.Equal(entries[j].mtime) {
			return entries[i].mtime.After(entries[j].mtime)
		}
		return entries[i].rel > entries[j].rel
	})
	if len(entries) == 0 {
		return "(no matches)"
	}
	paths := make([]string, 0, len(entries))
	for i, e := range entries {
		if i >= 500 {
			break
		}
		paths = append(paths, e.rel)
	}
	out := strings.Join(paths, "\n")
	if len(entries) > 500 {
		out += fmt.Sprintf("\n[... truncated at 500 of %d matches]", len(entries))
	}
	return out
}

// pyGlob returns the relative paths under root matching a python glob pattern
// with recursive=True. Directory symlinks are followed; a visited set of
// resolved directories guards against a cycle, which python did not.
func pyGlob(root, pattern string) []string {
	g := &globWalk{seen: map[string]bool{}}
	g.expand(root, "", strings.Split(strings.Trim(pattern, "/"), "/"))
	return g.out
}

type globWalk struct {
	out  []string
	seen map[string]bool
}

func (g *globWalk) expand(dir, rel string, segs []string) {
	if len(segs) == 0 {
		if rel != "" {
			g.out = append(g.out, rel)
		}
		return
	}
	seg, rest := segs[0], segs[1:]
	switch {
	case seg == "**":
		g.expand(dir, rel, rest)  // zero directories …
		g.descend(dir, rel, rest) // … or any depth
	case !strings.ContainsAny(seg, "*?["):
		full := filepath.Join(dir, seg)
		if _, err := os.Lstat(full); err == nil {
			g.expand(full, filepath.Join(rel, seg), rest)
		}
	default:
		g.matchSegment(dir, rel, seg, rest)
	}
}

// descend is the "**" recursion: every non-hidden directory at any depth,
// following symlinks.
func (g *globWalk) descend(dir, rel string, rest []string) {
	key := realpath(dir)
	if g.seen[key] {
		return
	}
	g.seen[key] = true
	defer delete(g.seen, key)
	for _, n := range listDir(dir) {
		if strings.HasPrefix(n, ".") {
			continue
		}
		full := filepath.Join(dir, n)
		if !isDir(full) {
			continue
		}
		sub := filepath.Join(rel, n)
		g.expand(full, sub, rest)
		g.descend(full, sub, rest)
	}
}

func (g *globWalk) matchSegment(dir, rel, seg string, rest []string) {
	re := fnmatchRE(seg)
	for _, n := range listDir(dir) {
		if strings.HasPrefix(n, ".") && !strings.HasPrefix(seg, ".") {
			continue
		}
		if !re.MatchString(n) {
			continue
		}
		full := filepath.Join(dir, n)
		if len(rest) > 0 && !isDir(full) {
			continue
		}
		g.expand(full, filepath.Join(rel, n), rest)
	}
}

func listDir(dir string) []string {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := d.Readdirnames(-1)
	_ = d.Close()
	sort.Strings(names) // D5: lexical, see walkDirs
	return names
}
