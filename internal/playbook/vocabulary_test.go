package playbook

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The playbook corpus must cover BOTH failure-class vocabularies, and the
// single flat map it uses is only safe while they stay disjoint.
//
// Incident 2026-08-26 (Finding B of docs/audits/2026-08-26-silent-controls-audit.md):
// `vornikctl playbook show container_non_zero_exit` answered "Unrecognised
// failure class" for the fleet's LARGEST failure class — 3,027 of 5,791
// classified step failures. The cause was not a missing entry for that one
// class. `internal/stepoutcome`'s 19 step classes were absent from the corpus
// ENTIRELY, because the corpus is keyed on `internal/persistence`'s
// TaskFailureClass* vocabulary and nothing connected the two.
//
// The guard that should have caught it could not: the old
// TestPlaybookCoversAllFailureClasses hand-listed its classes in a Go slice,
// so it mirrored the registry it was protecting. It named 19 of 23 task
// classes, and three of the four it missed are emitted in production today.
//
// These tests therefore derive the vocabularies from the DECLARATIONS via
// go/ast rather than from a maintained list. Adding a constant to either
// package without a corpus entry now fails the build.
//
// Design: https://docs.vornik.io (D5, D6)

var (
	taskClassRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	stepClassRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// exemptRe marks a declaration deliberately kept out of the corpus.
	// Same escape hatch, and the same visibility, as `doctor-vacuous:` in
	// the Finding A design — it must carry a justification after the colon.
	exemptRe = regexp.MustCompile(`playbook-exempt:\s*\S`)
)

// declaredClass is one string constant lifted from a vocabulary package.
type declaredClass struct {
	Ident  string // Go identifier, e.g. TaskFailureClassTimeout
	Value  string // wire value, e.g. TIMEOUT
	Exempt bool
}

// declaredClasses parses a package directory and returns every string
// constant whose identifier carries the given prefix, along with whether the
// declaration is marked playbook-exempt.
func declaredClasses(t *testing.T, dir, identPrefix string) []declaredClass {
	t.Helper()
	fset := token.NewFileSet()
	// os.ReadDir + ParseFile rather than parser.ParseDir: ParseDir is
	// deprecated (it ignores build tags), and we deliberately want EVERY
	// non-test .go file in the directory regardless of tags — a failure class
	// declared behind a build tag still reaches operators through
	// `playbook show`.
	dirents, err := os.ReadDir(dir)
	require.NoError(t, err, "reading %s", dir)

	var files []*ast.File
	for _, de := range dirents {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		require.NoError(t, err, "parsing %s", name)
		files = append(files, f)
	}
	require.NotEmpty(t, files, "no .go files found in %s", dir)

	var out []declaredClass
	for _, file := range files {
		{
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					// A doc comment may sit on the spec or, for a
					// single-spec block, on the GenDecl.
					doc := ""
					if vs.Doc != nil {
						doc = vs.Doc.Text()
					} else if len(gd.Specs) == 1 && gd.Doc != nil {
						doc = gd.Doc.Text()
					}
					if vs.Comment != nil {
						doc += vs.Comment.Text()
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, identPrefix) {
							continue
						}
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						val, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						out = append(out, declaredClass{
							Ident:  name.Name,
							Value:  val,
							Exempt: exemptRe.MatchString(doc),
						})
					}
				}
			}
		}
	}
	require.NotEmpty(t, out, "parsed no %s* constants from %s — the parser, not the corpus, is broken", identPrefix, dir)
	return out
}

func taskVocabulary(t *testing.T) []declaredClass {
	return declaredClasses(t, "../persistence", "TaskFailureClass")
}

func stepVocabulary(t *testing.T) []declaredClass {
	return declaredClasses(t, "../stepoutcome", "Class")
}

// TestCorpusCoversEveryDeclaredTaskClass — derived from the declarations, so
// a new TaskFailureClass* cannot ship without a playbook entry.
func TestCorpusCoversEveryDeclaredTaskClass(t *testing.T) {
	for _, c := range taskVocabulary(t) {
		t.Run(c.Value, func(t *testing.T) {
			if c.Exempt {
				t.Skipf("%s is marked playbook-exempt", c.Ident)
			}
			entry, ok := corpus[c.Value]
			require.True(t, ok, "playbook corpus has no entry for task class %s (%s)", c.Value, c.Ident)
			assert.Equal(t, c.Value, entry.Class, "entry.Class must match its key")
			assert.NotEmpty(t, strings.TrimSpace(entry.Cause), "Cause is required")
			assert.NotEmpty(t, entry.Suggestions, "at least one Suggestion required")
			assert.NotEmpty(t, strings.TrimSpace(entry.HumanMessage),
				"HumanMessage is required for the end-user-facing failed-task surface")
			assert.Equal(t, ScopeTask, entry.Scope, "task-vocabulary entry must declare Scope=task")
		})
	}
}

// TestCorpusCoversEveryDeclaredStepClass — the gap that produced the incident.
// internal/stepoutcome's classes reach operators through the same
// `playbook show` surface and must answer there too.
func TestCorpusCoversEveryDeclaredStepClass(t *testing.T) {
	for _, c := range stepVocabulary(t) {
		t.Run(c.Value, func(t *testing.T) {
			if c.Exempt {
				t.Skipf("%s is marked playbook-exempt", c.Ident)
			}
			entry, ok := corpus[c.Value]
			require.True(t, ok, "playbook corpus has no entry for step class %s (%s)", c.Value, c.Ident)
			assert.Equal(t, c.Value, entry.Class, "entry.Class must match its key")
			assert.NotEmpty(t, strings.TrimSpace(entry.Cause), "Cause is required")
			assert.NotEmpty(t, entry.Suggestions, "at least one Suggestion required")
			assert.NotEmpty(t, strings.TrimSpace(entry.HumanMessage), "HumanMessage is required")
			assert.Equal(t, ScopeStep, entry.Scope, "step-vocabulary entry must declare Scope=step")
		})
	}
}

// TestVocabulariesUseDistinctCaseConventions — the single flat corpus map is
// safe only because a key identifies its own vocabulary. Enforce the
// convention that makes that true, rather than trusting it.
func TestVocabulariesUseDistinctCaseConventions(t *testing.T) {
	for _, c := range taskVocabulary(t) {
		assert.Regexp(t, taskClassRe, c.Value,
			"task class %s must be UPPER_SNAKE — the corpus map relies on case to tell the vocabularies apart", c.Ident)
	}
	for _, c := range stepVocabulary(t) {
		assert.Regexp(t, stepClassRe, c.Value,
			"step class %s must be lower_snake — the corpus map relies on case to tell the vocabularies apart", c.Ident)
	}
}

// TestVocabulariesAreDisjoint — a collision would make one vocabulary's entry
// silently answer for the other's class.
func TestVocabulariesAreDisjoint(t *testing.T) {
	task := map[string]string{}
	for _, c := range taskVocabulary(t) {
		task[c.Value] = c.Ident
	}
	for _, c := range stepVocabulary(t) {
		if ident, clash := task[c.Value]; clash {
			t.Errorf("step class %s (%s) collides with task class %s — the flat corpus map cannot distinguish them",
				c.Value, c.Ident, ident)
		}
	}
}
