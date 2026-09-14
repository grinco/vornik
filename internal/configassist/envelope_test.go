package configassist

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentloop"
)

// Test 1 (design §9): the assistant never execs. Two allowlists, neither
// able to satisfy the other by accident: agentloop's own test permits the
// literal "git" argv in git_tools.go; THIS test asserts (a) this package
// spawns nothing and imports no os/exec, and (b) every handler agentloop
// registers from its subprocess-bearing file is OUT of the envelope — so
// the only subprocess in the callgraph is unreachable through Dispatch.
func TestEnvelope_NeverExecs(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); p == "os/exec" || strings.HasSuffix(p, "/exec") {
				t.Errorf("%s imports %s — the assistant never execs (2026-08-03 ruling, design §1)", e.Name(), p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "exec" || pkg.Name == "syscall") && strings.HasPrefix(sel.Sel.Name, "Command") || ok && pkg.Name == "syscall" && strings.HasPrefix(sel.Sel.Name, "Exec") {
					t.Errorf("%s:%d spawns a subprocess", e.Name(), fset.Position(call.Pos()).Line)
				}
			}
			return true
		})
	}

	// (b) the subprocess-bearing agentloop file's registrations are all OUT.
	// Parsed from source so a new git-backed handler upstream cannot slip
	// into IN through a stale list here.
	gitFile := filepath.Join("..", "agentloop", "git_tools.go")
	f, err := parser.ParseFile(fset, gitFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", gitFile, err)
	}
	registered := registeredHandlerNames(f)
	if len(registered) == 0 {
		t.Fatal("no Handlers[...] registrations found in git_tools.go — if the file moved, move this law with it")
	}
	for _, n := range registered {
		if Allowed(n) {
			t.Errorf("%q is registered by the subprocess-bearing git_tools.go and is IN the envelope", n)
		}
		if _, ok := Classify(n); !ok {
			t.Errorf("%q is registered by git_tools.go but not classified — it must be an explicit OUT", n)
		}
	}
}

// registeredHandlerNames extracts the string keys of `Handlers["x"] = …`
// assignments in a parsed file.
func registeredHandlerNames(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			idx, ok := lhs.(*ast.IndexExpr)
			if !ok {
				continue
			}
			if id, ok := idx.X.(*ast.Ident); !ok || id.Name != "Handlers" {
				continue
			}
			if lit, ok := idx.Index.(*ast.BasicLit); ok {
				out = append(out, strings.Trim(lit.Value, `"`))
			}
		}
		return true
	})
	return out
}

// Test 12 (design §9): a tool agentloop declares but the envelope does not
// classify fails — the new-tool-widens-the-surface case. The envelope must
// be exactly seven IN and every OUT named with a reason.
func TestEnvelope_EveryAgentloopToolClassified(t *testing.T) {
	if u := Unclassified(); len(u) != 0 {
		t.Fatalf("agentloop tools not classified in the envelope (OUT by default, but LOUD): %v — add a Classification row with a reason", u)
	}
	want := []string{"current_time", "file_edit", "file_read", "file_write", "glob", "grep", "read_many_files"}
	got := AllowedNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("IN set = %v, want exactly %v (design §2.0)", got, want)
	}
	for _, c := range envelope {
		if strings.TrimSpace(c.Reason) == "" {
			t.Errorf("%q has no reason — a classification is a decision recorded as data", c.Name)
		}
		if _, ok := agentloop.Handlers[c.Name]; !ok {
			t.Errorf("%q is classified but agentloop does not implement it", c.Name)
		}
	}
}

func TestDispatch_RefusesOutAndUnknown(t *testing.T) {
	env := agentloop.Env{Workspace: t.TempDir()}
	if got := Dispatch(env, "git_status", json.RawMessage(`{}`)); !strings.HasPrefix(got, ErrToolOutOfEnvelope) || !strings.Contains(got, "git_status") {
		t.Errorf("git_status must be refused by name: %q", got)
	}
	if got := Dispatch(env, "run_shell", json.RawMessage(`{}`)); !strings.HasPrefix(got, ErrToolOutOfEnvelope) || !strings.Contains(got, "not classified") {
		t.Errorf("unclassified tool must be refused as OUT by default: %q", got)
	}
	// An IN tool reaches agentloop (and its own refusals, e.g. missing path).
	if got := Dispatch(env, "file_read", json.RawMessage(`{}`)); got != "ERROR: path is required" {
		t.Errorf("file_read must be dispatched to agentloop: %q", got)
	}
	// Test 2 at this seam: a path outside the root is refused by name.
	if got := Dispatch(env, "file_read", json.RawMessage(`{"path":"../../etc/passwd"}`)); !strings.Contains(got, "escapes") && !strings.Contains(got, "not found") {
		t.Errorf("path escape must be refused: %q", got)
	}
}

func TestTools_SevenDefinitionsWithConfigRootWording(t *testing.T) {
	tools := Tools()
	if len(tools) != 7 {
		t.Fatalf("Tools() = %d definitions, want 7", len(tools))
	}
	for _, tl := range tools {
		if !Allowed(tl.Function.Name) {
			t.Errorf("advertised tool %q is not IN", tl.Function.Name)
		}
		if strings.Contains(tl.Function.Description, "/app/workspace") || strings.Contains(string(tl.Function.Parameters), "/app/workspace") {
			t.Errorf("%q still describes the container workspace: %s", tl.Function.Name, tl.Function.Description)
		}
		if strings.Contains(tl.Function.Description, "run_shell") {
			t.Errorf("%q advertises run_shell, which does not exist here", tl.Function.Name)
		}
		if !json.Valid(tl.Function.Parameters) {
			t.Errorf("%q parameters are not valid JSON after rewrite", tl.Function.Name)
		}
	}
}
