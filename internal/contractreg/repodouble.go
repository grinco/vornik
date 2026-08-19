package contractreg

// Repository-double conformance.
//
// A hand-written test double for a repository lookup is a statement about
// what production does. When the statement is wrong in the strict direction —
// an error where production returns a value, or the reverse — the double does
// not merely fail to catch a bug: every test that exercises the caller's miss
// path takes a branch production never takes, and the real branch ships
// unexecuted. That is how a nil dereference reached the daemon on 2026-08-19
// and crash-looped it 28 times in ten minutes.
//
// internal/persistence/misscontract declares what each lookup returns for an
// absent row. This file finds the doubles and requires each one's package to
// assert that contract, so a double cannot be quietly wrong.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/persistence/misscontract"
)

// RepoDouble is a type declared in a _test.go file whose method set includes
// a registered single-entity lookup.
type RepoDouble struct {
	// Package is the module-relative directory holding the declaration.
	Package string
	Type    string
	Method  string
	// Key is the misscontract.Contract key the method implements.
	Key  string
	File string
	Line int
	// Permissive reports that the method body can syntactically return
	// (nil, nil). Against a MissErrNotFound key that makes the double LOOSER
	// than production — the mirror of the defect that shipped the crash, and
	// a distinct hazard: a double that answers a miss with a nil error can
	// swallow a real ErrNotFound and let a broken caller pass.
	//
	// Syntactic, so it under-reports (a double that delegates to a permissive
	// helper is missed) and never over-reports: a literal `return nil, nil`
	// in the body is not a guess.
	Permissive bool
}

// ID is the allowlist token for a double: "<package dir>:<Type>.<Method>".
func (d RepoDouble) ID() string {
	return fmt.Sprintf("%s:%s.%s", d.Package, d.Type, d.Method)
}

// RepoDoubleAudit is the result of scanning a module.
type RepoDoubleAudit struct {
	// Doubles are the test-declared implementations of registered lookups.
	Doubles []RepoDouble
	// Asserted maps a package directory to the contract keys asserted in it
	// via repotest.AssertMiss / AssertMissRepo.
	Asserted map[string]map[string]bool
}

// lookupIndex resolves a method to the contract key it implements. A double
// names no interface, so the method's name plus the type it returns is what
// identifies which lookup it stands in for.
type lookupIndex map[lookupSig]string

type lookupSig struct {
	method     string
	returnType string
}

// AuditRepoDoubles scans the module rooted at root.
func AuditRepoDoubles(root string) (RepoDoubleAudit, error) {
	index, err := buildLookupIndex(root)
	if err != nil {
		return RepoDoubleAudit{}, err
	}
	audit := RepoDoubleAudit{Asserted: map[string]map[string]bool{}}

	err = walkGoFiles(root, func(fset *token.FileSet, path string, rel string, f *ast.File) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv != nil {
				if double, ok := matchDouble(index, fn); ok {
					double.Package = pkgDir
					double.File = rel
					double.Line = fset.Position(fn.Pos()).Line
					audit.Doubles = append(audit.Doubles, double)
				}
				continue
			}
			collectAssertions(audit.Asserted, pkgDir, fn)
		}
	})
	if err != nil {
		return RepoDoubleAudit{}, err
	}
	sort.Slice(audit.Doubles, func(i, j int) bool {
		return audit.Doubles[i].ID() < audit.Doubles[j].ID()
	})
	return audit, nil
}

// CheckRepoDoubleConformance reports doubles whose package never asserts the
// contract they implement, and allowlist entries that no longer name one.
//
// The allowlist is shrink-only by construction: a stale entry is a finding,
// so a cleanup must delete its entry and a deleted double cannot leave a slot
// behind for a new offender to occupy silently.
func CheckRepoDoubleConformance(audit RepoDoubleAudit, allow map[string]bool) []Finding {
	var findings []Finding
	used := map[string]bool{}

	for _, d := range audit.Doubles {
		if audit.Asserted[d.Package][d.Key] {
			continue
		}
		if allow[d.ID()] {
			used[d.ID()] = true
			continue
		}
		findings = append(findings, Finding{
			Check: "repo-double-conformance",
			Name:  d.ID(),
			Detail: fmt.Sprintf(
				"implements %s but package %s never asserts the miss contract; "+
					"add repotest.AssertMiss(t, %q, …) or allowlist it",
				d.Key, d.Package, d.Key),
			Sources: []string{fmt.Sprintf("%s:%d", d.File, d.Line)},
		})
	}

	stale := make([]string, 0, len(allow))
	for id := range allow {
		if !used[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		findings = append(findings, Finding{
			Check:  "repo-double-conformance",
			Name:   id,
			Detail: "stale allowlist entry: it names no unasserted double. The list is shrink-only — delete the entry.",
		})
	}
	return findings
}

// CheckRepoLookupRegistration reports single-entity lookups on persistence
// interfaces that misscontract.Contract neither registers nor excludes, so
// the table cannot silently fall behind the interfaces it describes.
func CheckRepoLookupRegistration(root string) ([]Finding, error) {
	lookups, err := scanInterfaceLookups(root)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, key := range lookups {
		if _, ok := misscontract.Behavior(key); ok {
			continue
		}
		if _, excluded := misscontract.Excluded[key]; excluded {
			continue
		}
		findings = append(findings, Finding{
			Check: "repo-lookup-registration",
			Name:  key,
			Detail: "returns (*T, error) but is in neither misscontract.Contract nor " +
				"misscontract.Excluded; declare what an absent row returns",
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Name < findings[j].Name })
	return findings, nil
}

// scanInterfaceLookups returns "<Interface>.<Method>" for every interface
// method in the persistence package returning (*T, error).
func scanInterfaceLookups(root string) ([]string, error) {
	var out []string
	dir := filepath.Join(root, "internal", "persistence")
	err := walkGoFilesIn(dir, false, func(_ *token.FileSet, _ string, _ string, f *ast.File) {
		forEachInterfaceMethod(f, func(iface, method string, ft *ast.FuncType) {
			if _, ok := singleEntityReturn(ft); ok {
				out = append(out, iface+"."+method)
			}
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// buildLookupIndex maps (method name, returned type) to the contract key it
// belongs to, for every registered lookup. A collision would make a double
// unattributable, so it is reported rather than resolved arbitrarily.
func buildLookupIndex(root string) (lookupIndex, error) {
	index := lookupIndex{}
	dir := filepath.Join(root, "internal", "persistence")
	err := walkGoFilesIn(dir, false, func(_ *token.FileSet, _ string, _ string, f *ast.File) {
		forEachInterfaceMethod(f, func(iface, method string, ft *ast.FuncType) {
			ret, ok := singleEntityReturn(ft)
			if !ok {
				return
			}
			key := iface + "." + method
			if _, registered := misscontract.Behavior(key); !registered {
				return
			}
			index[lookupSig{method: method, returnType: ret}] = key
		})
	})
	if err != nil {
		return nil, err
	}
	return index, nil
}

// matchDouble decides whether a method declaration stands in for a registered
// lookup. The receiver type names the double; the method name and its
// returned entity type name the lookup.
func matchDouble(index lookupIndex, fn *ast.FuncDecl) (RepoDouble, bool) {
	ret, ok := singleEntityReturn(fn.Type)
	if !ok {
		return RepoDouble{}, false
	}
	key, ok := index[lookupSig{method: fn.Name.Name, returnType: ret}]
	if !ok {
		return RepoDouble{}, false
	}
	return RepoDouble{
		Type:       receiverTypeName(fn),
		Method:     fn.Name.Name,
		Key:        key,
		Permissive: returnsNilNil(fn.Body),
	}, true
}

// returnsNilNil reports whether body contains a literal `return nil, nil`.
// Closures are skipped: they carry their own contract, not the method's.
func returnsNilNil(body *ast.BlockStmt) bool {
	if body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, isLit := n.(*ast.FuncLit); isLit {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 2 {
			return true
		}
		if isNilIdent(ret.Results[0]) && isNilIdent(ret.Results[1]) {
			found = true
		}
		return true
	})
	return found
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// collectAssertions records every repotest.AssertMiss / AssertMissRepo call
// in fn, keyed by the contract key its first string literal names.
func collectAssertions(into map[string]map[string]bool, pkgDir string, fn *ast.FuncDecl) {
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isAssertMissCall(call.Fun) {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			key := strings.Trim(lit.Value, "`\"")
			if into[pkgDir] == nil {
				into[pkgDir] = map[string]bool{}
			}
			into[pkgDir][key] = true
		}
		return true
	})
}

func isAssertMissCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name == "AssertMiss" || f.Sel.Name == "AssertMissRepo"
	case *ast.Ident:
		return f.Name == "AssertMiss" || f.Name == "AssertMissRepo"
	case *ast.IndexExpr:
		return isAssertMissCall(f.X)
	case *ast.IndexListExpr:
		return isAssertMissCall(f.X)
	}
	return false
}

// singleEntityReturn reports the bare name of T for a signature returning
// (*T, error) — the shape of every keyed lookup.
func singleEntityReturn(ft *ast.FuncType) (string, bool) {
	if ft.Results == nil {
		return "", false
	}
	var results []ast.Expr
	for _, f := range ft.Results.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			results = append(results, f.Type)
		}
	}
	if len(results) != 2 {
		return "", false
	}
	star, ok := results[0].(*ast.StarExpr)
	if !ok {
		return "", false
	}
	if id, ok := results[1].(*ast.Ident); !ok || id.Name != "error" {
		return "", false
	}
	return baseTypeName(star.X), true
}

// baseTypeName drops any package qualifier, so persistence.ExtractedDocument
// in a consumer package and ExtractedDocument inside the persistence package
// compare equal.
func baseTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.StarExpr:
		return baseTypeName(t.X)
	case *ast.IndexExpr: // generic double, e.g. fakeRepo[T]
		return baseTypeName(t.X)
	case *ast.IndexListExpr:
		return baseTypeName(t.X)
	}
	return ""
}

func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	return baseTypeName(fn.Recv.List[0].Type)
}

func forEachInterfaceMethod(f *ast.File, visit func(iface, method string, ft *ast.FuncType)) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			for _, m := range it.Methods.List {
				ft, ok := m.Type.(*ast.FuncType)
				if !ok || len(m.Names) == 0 {
					continue
				}
				visit(ts.Name.Name, m.Names[0].Name, ft)
			}
		}
	}
}

func walkGoFiles(root string, visit func(fset *token.FileSet, path, rel string, f *ast.File)) error {
	return walkGoFilesIn(root, true, visit)
}

func walkGoFilesIn(dir string, recurse bool, visit func(fset *token.FileSet, path, rel string, f *ast.File)) error {
	base := dir
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			name := info.Name()
			// Skip every dotted directory. Besides .git, this repo keeps
			// full exported copies of itself under .vornik-export and
			// .vornik-public-clone; scanning those would triple every count
			// and put paths in the allowlist that no commit can change.
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			switch name {
			case "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			if !recurse && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file this package cannot parse is not a place a double can
			// hide from the compiler either; skip rather than fail the lint.
			return nil
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			rel = path
		}
		visit(fset, path, filepath.ToSlash(rel), f)
		return nil
	})
}
