package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Incident 2026-09-24: deploying 2026.9.6-32 crash-looped production at
// startup. initTelegram → linkCodeExposureGuard passed observabilityRegistry()
// — a nil *prometheus.Registry before observability is wired — straight into
// chatauth.NewExposureMetrics(prometheus.Registerer). A typed nil makes the
// interface non-nil, the callee's `reg == nil` guard passes, and promauto
// panics in MustRegister. The fourth crash-loop from this one trap
// (2026-06-06, 2026-06-27, the route-queue registry panic, 2026-09-24).
func TestLinkCodeExposureGuard_NoObservabilityDoesNotPanic(t *testing.T) {
	c := &Container{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("linkCodeExposureGuard panicked with no observability wired: %v", r)
		}
	}()
	if c.linkCodeExposureGuard() == nil {
		t.Fatal("the guard must still be built without metrics — its scrub half needs none")
	}
}

func TestObservabilityRegisterer_IsAnUntypedNilWhenUnwired(t *testing.T) {
	c := &Container{}
	if r := c.observabilityRegisterer(); r != nil {
		t.Fatalf("observabilityRegisterer() = %#v, want an untyped nil interface", r)
	}
}

// The structural guard: the registry accessors — observabilityRegistry() and
// the exported ObservabilityRegistry() — may appear only where the concrete
// pointer is safe: bound to a variable, returned, or compared with nil. Any
// other position (a call argument, a composite-literal field, a method value
// handed on) can widen a nil *prometheus.Registry into a non-nil Registerer.
// Hand a Registerer parameter observabilityRegisterer() instead. A new unsafe
// use fails here rather than crash-looping a deployment. Scope: this package,
// where the container hands the registry out (the design states the limit).
func TestNoCallPassesObservabilityRegistryDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	accessors := map[string]bool{"observabilityRegistry": true, "ObservabilityRegistry": true}
	sawPrimitive := false
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == "observabilityRegisterer" {
				sawPrimitive = true
			}
			if sel, ok := n.(*ast.SelectorExpr); ok && accessors[sel.Sel.Name] && len(stack) > 0 {
				parent := stack[len(stack)-1]
				call, isCall := parent.(*ast.CallExpr)
				if !isCall || call.Fun != sel {
					// A method value, handed on: the same trap one step removed.
					t.Errorf("%s: %s used as a method value", fset.Position(sel.Pos()), sel.Sel.Name)
				} else if len(stack) > 1 {
					switch stack[len(stack)-2].(type) {
					case *ast.AssignStmt, *ast.ValueSpec, *ast.ReturnStmt, *ast.BinaryExpr:
						// bound, returned, or nil-compared: the concrete pointer is safe
					default:
						t.Errorf("%s: %s() used directly (%T) — a nil *prometheus.Registry becomes a non-nil "+
							"Registerer and panics in promauto. Use observabilityRegisterer(), or bind it and "+
							"nil-check the pointer first.", fset.Position(sel.Pos()), sel.Sel.Name, stack[len(stack)-2])
					}
				}
			}
			stack = append(stack, n)
			return true
		})
	}
	// Not a file-count floor that rots: the guard must have examined the file
	// that defines the primitive, or it looked at the wrong package.
	if !sawPrimitive {
		t.Fatal("the guard did not examine the file defining observabilityRegisterer — it looked at nothing that matters")
	}
}
