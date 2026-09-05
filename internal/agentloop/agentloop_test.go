package agentloop

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agenttools"
)

// TestImportsAreLeaf — the package ships inside the agent image via
// vornik-agent-helper; daemon code must not ride along. Only the standard
// library and internal/agenttools may be imported.
func TestImportsAreLeaf(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, ".") && p != "vornik.io/vornik/internal/agenttools" {
				t.Errorf("%s imports %s — agentloop is a leaf: stdlib and internal/agenttools only", e.Name(), p)
			}
		}
	}
}

// TestHandlers_MatchTheDeclarationBothWays — the table implements exactly
// the tools declared RuntimeHelper.
func TestHandlers_MatchTheDeclarationBothWays(t *testing.T) {
	if got, want := HandlerNames(), agenttools.HelperNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Handlers = %v, declaration's helper set = %v", got, want)
	}
}

func TestDispatch_RefusesWhatItDoesNotImplement(t *testing.T) {
	env := Env{Workspace: t.TempDir()}
	if got := Dispatch(env, "run_shell", json.RawMessage(`{}`)); got != "ERROR: tool 'run_shell' does not run in the helper" {
		t.Errorf("shell-runtime tool: %q", got)
	}
	if got := Dispatch(env, "mcp__x__y", nil); !strings.HasPrefix(got, "ERROR: tool 'mcp__x__y' does not run") {
		t.Errorf("undeclared name: %q", got)
	}
}
