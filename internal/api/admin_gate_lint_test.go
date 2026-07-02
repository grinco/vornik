package api

// Lint guard: every HTTP handler registered on an /api/v1/admin/ route MUST
// enforce requireAdminGate somewhere in its call tree.
//
// Admin authorization in this package is applied PER HANDLER (each admin
// handler self-calls s.requireAdminGate), not by a route-group middleware
// wrapper. That convention is easy to forget: a new admin route whose handler
// omits the gate would be reachable by any authenticated (non-admin) key. This
// test fails the build if any admin-registered handler — directly or via a
// dispatch router — never reaches requireAdminGate.
//
// Mechanism: parse the package sources, extract the set of handler function
// names bound to "/api/v1/admin/..." paths in routes.go, build a call graph
// over every func/method in the package, and assert each admin handler
// transitively reaches a requireAdminGate call.
//
// Known boundary: this is reachability, not per-branch control-flow analysis.
// A *router* that gates only some of its dispatch branches would still pass
// (the gate is reachable). The guard targets the common failure — a handler
// (or an entire router) with no gate anywhere in its tree.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

const adminRoutePrefix = "/api/v1/admin/"

// parsePackageFiles parses every non-test .go file in the current directory
// (the package under test) and returns the ASTs.
func parsePackageFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no package source files parsed")
	}
	return files
}

// adminRouteHandlers returns the set of handler function names registered on an
// /api/v1/admin/ path via mux.HandleFunc("/api/v1/admin/...", <recv>.Handler).
func adminRouteHandlers(files []*ast.File) map[string]bool {
	handlers := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HandleFunc" || len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(path, adminRoutePrefix) {
				return true
			}
			if name := calleeName(call.Args[1]); name != "" {
				handlers[name] = true
			}
			return true
		})
	}
	return handlers
}

// calleeName extracts the identifier name from a handler reference, whether
// written as a bare identifier (Handler) or a selector (server.Handler).
func calleeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// buildCallGraph returns, for every function/method declared in the package,
// the set of function/method names it calls, plus the set of functions that
// call requireAdminGate directly.
func buildCallGraph(files []*ast.File) (calls map[string]map[string]bool, gatesDirect map[string]bool) {
	calls = map[string]map[string]bool{}
	gatesDirect = map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if calls[name] == nil {
				calls[name] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := calleeName(call.Fun)
				if callee == "" {
					return true
				}
				if callee == "requireAdminGate" {
					gatesDirect[name] = true
				}
				calls[name][callee] = true
				return true
			})
		}
	}
	return calls, gatesDirect
}

// reachesGate reports whether fn, or any function transitively called by it,
// invokes requireAdminGate.
func reachesGate(fn string, calls map[string]map[string]bool, gatesDirect map[string]bool) bool {
	visited := map[string]bool{}
	var walk func(string) bool
	walk = func(cur string) bool {
		if gatesDirect[cur] {
			return true
		}
		if visited[cur] {
			return false
		}
		visited[cur] = true
		for callee := range calls[cur] {
			if walk(callee) {
				return true
			}
		}
		return false
	}
	return walk(fn)
}

// TestAdminHandlersRequireAdminGate is the guard: it fails if any handler
// registered on an /api/v1/admin/ route never reaches requireAdminGate.
func TestAdminHandlersRequireAdminGate(t *testing.T) {
	files := parsePackageFiles(t)

	handlers := adminRouteHandlers(files)
	if len(handlers) == 0 {
		t.Fatal("no /api/v1/admin/ routes discovered — the route scanner is broken; " +
			"this guard would silently pass and must be fixed before merging")
	}

	calls, gatesDirect := buildCallGraph(files)

	var ungated []string
	for name := range handlers {
		if !reachesGate(name, calls, gatesDirect) {
			ungated = append(ungated, name)
		}
	}
	if len(ungated) > 0 {
		t.Fatalf("admin route handler(s) never reach requireAdminGate: %v\n"+
			"every /api/v1/admin/ handler must call s.requireAdminGate (directly or "+
			"via the router/leaf it dispatches to)", ungated)
	}
}

// TestAdminGateLintDetectsMissingGate proves the guard has teeth: fed a
// synthetic package where one admin handler omits the gate and another
// includes it, the checker must flag exactly the ungated one. Without this,
// a bug in the AST logic could make TestAdminHandlersRequireAdminGate a
// vacuous pass.
func TestAdminGateLintDetectsMissingGate(t *testing.T) {
	const src = `package api

import "net/http"

type Server struct{}

func (s *Server) requireAdminGate(w http.ResponseWriter, r *http.Request) bool { return true }

func (s *Server) AdminGated(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminGate(w, r) {
		return
	}
}

func (s *Server) AdminUngated(w http.ResponseWriter, r *http.Request) {
	_ = r
}

// AdminRouter dispatches to a gated leaf — reachable, so it should pass.
func (s *Server) AdminRouter(w http.ResponseWriter, r *http.Request) {
	s.AdminGated(w, r)
}

func register(mux *http.ServeMux, server *Server) {
	mux.HandleFunc("/api/v1/admin/gated", server.AdminGated)
	mux.HandleFunc("/api/v1/admin/ungated", server.AdminUngated)
	mux.HandleFunc("/api/v1/admin/router", server.AdminRouter)
	mux.HandleFunc("/api/v1/projects/x", server.AdminUngated) // non-admin: ignored
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	files := []*ast.File{f}

	handlers := adminRouteHandlers(files)
	if !handlers["AdminGated"] || !handlers["AdminUngated"] || !handlers["AdminRouter"] {
		t.Fatalf("route scanner missed admin handlers: got %v", handlers)
	}
	if handlers["projects"] {
		t.Fatal("route scanner picked up a non-admin route")
	}

	calls, gatesDirect := buildCallGraph(files)

	if !reachesGate("AdminGated", calls, gatesDirect) {
		t.Error("AdminGated should be detected as gated")
	}
	if !reachesGate("AdminRouter", calls, gatesDirect) {
		t.Error("AdminRouter should be detected as gated via its dispatch leaf")
	}
	if reachesGate("AdminUngated", calls, gatesDirect) {
		t.Error("AdminUngated must be detected as UNGATED — the guard has no teeth")
	}
}
