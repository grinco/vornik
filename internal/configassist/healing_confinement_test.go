package configassist

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSystemEntrypoint_HasExactlyOnePermittedAssembler_BySymbol is the
// confinement §6.3.4 rests on, built to the standard the record sets rather
// than the one the design's first draft described.
//
// That draft said "a source contract says so". This codebase's precedent
// (internal/service/apikey_door_wiring_test.go, and
// 2026-08-06-code-contract-reachability-design.md's live-by-contract) is
// stronger: attribute each use to its enclosing function and require the
// permitted one to be the NAMED one.
//
// COUNTING IS NOT CONFINING, which is the trap this avoids. A test asserting
// "at most one caller names EntrypointSystem" passes for a NEW caller too —
// that caller simply becomes the one (round 3 F5; review-20260915-cc32 F3.1).
// So the permitted assembler is named by symbol, and a second one fails the
// build.
//
// What it protects: a caller reaching the system entrypoint without a healing
// trial behind it would file healing candidates nothing ever trialled. That is
// quieter than an unreviewed apply — the class gate stops the apply — and it
// is the same wrong.
func TestSystemEntrypoint_HasExactlyOnePermittedAssembler_BySymbol(t *testing.T) {
	const permitted = "ProposeForHealing"
	use := regexp.MustCompile(`\bEntrypointSystem\b`)

	// Where the constant may legitimately appear without being a caller: its
	// own declaration, and the auto-apply predicate that reads it.
	declarationSites := map[string]bool{
		"entrypoints.go": true,
	}

	var assemblers []string
	err := filepath.Walk("..", func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if declarationSites[filepath.Base(path)] {
			return nil
		}
		for _, fn := range splitFuncs(string(src)) {
			if use.MatchString(stripLineComments(fn.body)) {
				assemblers = append(assemblers, filepath.Base(path)+":"+fn.name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	if len(assemblers) != 1 {
		t.Fatalf("EntrypointSystem is assembled by %v; exactly one function may, and it must be "+
			"%s. A second assembler can file healing candidates nothing ever trialled",
			assemblers, permitted)
	}
	if got := strings.SplitN(assemblers[0], ":", 2)[1]; got != permitted {
		t.Errorf("EntrypointSystem's one assembler is %q, want %q. The identity is what confines "+
			"it — a test that only counted would accept this silently", got, permitted)
	}
}

type fnBody struct{ name, body string }

func splitFuncs(src string) []fnBody {
	var out []fnBody
	decl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	var cur *fnBody
	for _, line := range strings.Split(src, "\n") {
		if m := decl.FindStringSubmatch(line); m != nil {
			out = append(out, fnBody{name: m[1]})
			cur = &out[len(out)-1]
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	return out
}

func stripLineComments(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
