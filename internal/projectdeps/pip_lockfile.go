package projectdeps

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// LockfileError names a lockfile line that cannot support the cache key's
// claim. The line number is carried because "your lockfile is not hash-pinned"
// sends an operator to read a thousand lines by hand.
type LockfileError struct {
	Path    string
	Line    int
	Message string
}

func (e LockfileError) Error() string {
	if e.Line == 0 {
		return fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Message)
}

// RequireHashPinned refuses a pip lockfile that cannot identify the exact
// bytes that will be installed.
//
// This is the enforcement half of design §3 property 2. A `pip freeze` with
// pinned versions and NO hashes resolves to different wheels over time — a
// re-upload or a yank changes what the same line installs — so keying the
// cache on those bytes would be a claim the key cannot support. Accepted
// producers are `pip-compile --generate-hashes`, `uv pip compile
// --generate-hashes` and `pip freeze --require-hashes` (design §5.1).
//
// Two refusals beyond the obvious one:
//
//   - `-r` / `-c` includes, because the key is computed over ONE file's bytes
//     and an include puts part of the resolution outside it. A cache hit would
//     then reuse a materialisation whose included file has since changed.
//   - a requirement without `==`, because a hash on a floating specifier
//     pins the artifact but not which artifact is chosen.
func RequireHashPinned(path string, content []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		lineNo      int
		startLine   int
		logical     strings.Builder
		open        bool
		requirement bool
	)
	flush := func() error {
		if !open {
			return nil
		}
		open = false
		stmt := strings.TrimSpace(logical.String())
		logical.Reset()
		if stmt == "" {
			return nil
		}
		return checkPipStatement(path, startLine, stmt, &requirement)
	}

	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if !open {
			open = true
			startLine = lineNo
		}
		if strings.HasSuffix(trimmed, `\`) {
			logical.WriteString(strings.TrimSuffix(trimmed, `\`))
			logical.WriteString(" ")
			continue
		}
		logical.WriteString(trimmed)
		if err := flush(); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return LockfileError{Path: path, Message: fmt.Sprintf("could not be read: %v", err)}
	}
	if err := flush(); err != nil {
		return err
	}

	if !requirement {
		// An empty or options-only lockfile would materialise an empty
		// tree and mount it, so every import would still fail — with a
		// cache that reports itself healthy. Refuse instead: a project
		// that needs nothing should declare no manifest (design §5.4).
		return LockfileError{Path: path, Message: "declares no requirements — a project that needs no dependencies should omit the manifest entry rather than point at an empty lockfile"}
	}
	return nil
}

// checkPipStatement judges one logical line (continuations already joined).
func checkPipStatement(path string, line int, stmt string, sawRequirement *bool) error {
	fields := strings.Fields(stmt)
	head := fields[0]

	switch {
	case head == "-r" || head == "-c" || strings.HasPrefix(head, "--requirement") || strings.HasPrefix(head, "--constraint"):
		return LockfileError{Path: path, Line: line, Message: fmt.Sprintf("%q puts part of the resolution outside this file, and the cache key is computed over this file's bytes alone — inline the included requirements", head)}
	case strings.HasPrefix(head, "-e") || strings.HasPrefix(head, "--editable"):
		return LockfileError{Path: path, Line: line, Message: "an editable install resolves from a working tree rather than from pinned bytes; it cannot be content-addressed"}
	case strings.HasPrefix(head, "-"):
		// Any other option line (--index-url, --find-links, --no-binary…)
		// carries no artifact of its own and is left to pip.
		return nil
	}

	*sawRequirement = true
	if !strings.Contains(stmt, "--hash=") {
		return LockfileError{Path: path, Line: line, Message: fmt.Sprintf("%q carries no --hash=; regenerate the lockfile with pip-compile --generate-hashes, uv pip compile --generate-hashes, or pip freeze --require-hashes", head)}
	}
	if !strings.Contains(head, "==") {
		return LockfileError{Path: path, Line: line, Message: fmt.Sprintf("%q is not pinned with ==; a hash on a floating specifier pins the artifact but not which artifact is chosen", head)}
	}
	return nil
}
