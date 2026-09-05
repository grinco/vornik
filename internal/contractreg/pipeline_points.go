package contractreg

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/pipeline"
)

// pipelineImportPath is the package whose constructors this check audits.
const pipelineImportPath = "vornik.io/vornik/internal/pipeline"

// PipelineConstruction is one call to pipeline.NewDecide / NewObserve /
// NewAround found in the tree, with the point it names (when resolvable).
//
// The design of record (2026-09-04-pipeline-points-design.md §4) requires the
// point argument to be the selector literal `pipeline.<Name>` — nothing else.
// A variable holding a point, a function returning one, or a composite
// literal reports as Unresolvable, which is a finding: the point's identity
// is meant to be greppable.
type PipelineConstruction struct {
	// Constructor is NewDecide, NewObserve or NewAround.
	Constructor string
	// Mode is the mode the constructor implies.
	Mode pipeline.Mode
	// Point is the declared point name the argument selects, "" when Unresolvable.
	Point        string
	Unresolvable bool
	// Package is the directory (relative to root) the construction lives in;
	// Test is true for a _test.go file.
	Package string
	Source  string
	Test    bool
}

var constructorModes = map[string]pipeline.Mode{
	"NewDecide":  pipeline.ModeDecide,
	"NewObserve": pipeline.ModeObserve,
	"NewAround":  pipeline.ModeAround,
}

// AuditPipelineConstructions walks internal/ and cmd/ under root, skipping
// *.generated.go, and returns every constructor call it finds. Parsing only —
// no type information — which is enough because the accepted argument shape is
// syntactic by design.
func AuditPipelineConstructions(root string) ([]PipelineConstruction, error) {
	var out []PipelineConstruction
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".generated.go") {
				return nil
			}
			found, err := pipelineConstructionsInFile(fset, root, path)
			if err != nil {
				return err
			}
			out = append(out, found...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

func pipelineConstructionsInFile(fset *token.FileSet, root, path string) ([]PipelineConstruction, error) {
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	local := ""
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != pipelineImportPath {
			continue
		}
		local = "pipeline"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" {
		return nil, nil
	}
	rel, _ := filepath.Rel(root, path)
	pkgDir := filepath.Dir(rel)
	isTest := strings.HasSuffix(path, "_test.go")
	var out []PipelineConstruction
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ctor := constructorName(call.Fun, local)
		if ctor == "" {
			return true
		}
		c := PipelineConstruction{Constructor: ctor, Mode: constructorModes[ctor], Package: pkgDir, Test: isTest,
			Source: fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line)}
		if len(call.Args) > 0 {
			if sel, ok := call.Args[0].(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
					c.Point = declaredPointName(sel.Sel.Name)
				}
			}
		}
		c.Unresolvable = c.Point == ""
		out = append(out, c)
		return true
	})
	return out, nil
}

// constructorName returns NewDecide/NewObserve/NewAround when fun is a call to
// one of them on the pipeline package — with or without explicit type
// arguments (`pipeline.NewDecide[T](…)`, `pipeline.NewAround[A, B](…)`).
func constructorName(fun ast.Expr, local string) string {
	switch e := fun.(type) {
	case *ast.IndexExpr:
		return constructorName(e.X, local)
	case *ast.IndexListExpr:
		return constructorName(e.X, local)
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok || id.Name != local {
			return ""
		}
		if _, ok := constructorModes[e.Sel.Name]; ok {
			return e.Sel.Name
		}
	}
	return ""
}

// declaredPointName maps the Go identifier of a declared point variable
// (DispatcherPreTool) to its name ("dispatcher.pre_tool"); "" when the
// identifier is not a declared point.
func declaredPointName(ident string) string {
	for _, p := range pipeline.Points {
		if pointIdent(p.Name) == ident {
			return p.Name
		}
	}
	return ""
}

// pointIdent is the Go identifier convention for a point name:
// "dispatcher.pre_tool" → "DispatcherPreTool".
func pointIdent(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		if r == '.' || r == '_' {
			upper = true
			continue
		}
		if upper {
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParsePipelinePointAllowlist reads the shrink-only list of declared points
// that are not yet constructed anywhere. Every entry needs a reason; the list
// exists only while the conversion is in flight (design §8 steps 2–4) and an
// entry naming a point that IS constructed is itself a finding.
func ParsePipelinePointAllowlist(data string) (map[string]string, error) {
	out := map[string]string{}
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, found := strings.Cut(line, "#")
		name, reason = strings.TrimSpace(name), strings.TrimSpace(reason)
		if name == "" {
			continue
		}
		if !found || reason == "" {
			return nil, fmt.Errorf("pipeline-point allowlist line %d (%q): every entry needs a reason after '#'", i+1, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("pipeline-point allowlist line %d: %q is listed twice", i+1, name)
		}
		if _, ok := pipeline.Lookup(name); !ok {
			return nil, fmt.Errorf("pipeline-point allowlist line %d: %q is not a declared point", i+1, name)
		}
		out[name] = reason
	}
	return out, nil
}

// CheckPipelinePoints reports (design §4):
//   - a construction whose point argument is not a `pipeline.<Name>` selector
//     of a declared point (unresolvable);
//   - a construction whose constructor disagrees with the point's declared mode;
//   - a declared point with no non-test construction, unless allowlisted;
//   - an allowlist entry for a point that IS constructed (stale);
//   - a point constructed in more than one non-test package.
//
// _test.go constructions count for the first two findings — a test must not
// construct a point wrongly either — and are ignored by the rest, since tests
// legitimately build throwaway chains.
func CheckPipelinePoints(cons []PipelineConstruction, allow map[string]string) []Finding {
	const check = "pipeline-points"
	var out []Finding
	owners := map[string]map[string]bool{}
	for _, c := range cons {
		if c.Unresolvable {
			out = append(out, Finding{Check: check, Name: c.Constructor, Sources: []string{c.Source},
				Detail: "point argument is not a literal pipeline.<Name> selector of a declared point — a variable, " +
					"helper or composite literal cannot be resolved by the lint and is refused; name the point"})
			continue
		}
		p, _ := pipeline.Lookup(c.Point)
		if p.Mode != c.Mode {
			out = append(out, Finding{Check: check, Name: c.Point, Sources: []string{c.Source},
				Detail: fmt.Sprintf("%s constructs %q, which is declared %s — the constructor would panic at boot", c.Constructor, c.Point, p.Mode)})
		}
		if c.Test {
			continue
		}
		if owners[c.Point] == nil {
			owners[c.Point] = map[string]bool{}
		}
		owners[c.Point][c.Package] = true
	}
	for _, p := range pipeline.Points {
		pkgs := owners[p.Name]
		if len(pkgs) == 0 {
			if reason, ok := allow[p.Name]; ok {
				_ = reason
				continue
			}
			out = append(out, Finding{Check: check, Name: p.Name,
				Detail: "declared in pipeline.Points but constructed nowhere outside tests — a seam documented in the design that nothing wires (a phantom)"})
			continue
		}
		if _, ok := allow[p.Name]; ok {
			out = append(out, Finding{Check: check, Name: p.Name,
				Detail: "listed in the pipeline-point allowlist as not yet constructed, but it is — delete the entry (the list is shrink-only)"})
		}
		if len(pkgs) > 1 {
			names := make([]string, 0, len(pkgs))
			for k := range pkgs {
				names = append(names, k)
			}
			sort.Strings(names)
			out = append(out, Finding{Check: check, Name: p.Name,
				Detail: "constructed in more than one package (" + strings.Join(names, ", ") + ") — a chain is per-owner, and two owners of one point is two pipelines with one name"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}
