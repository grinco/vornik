package contractreg

// Backend contract coverage.
//
// The persistence layer has two implementations of every repository, and what
// keeps them honest is the shared suite in internal/persistence/repotest —
// one set of assertions run against both. Nothing required a repository to
// HAVE one.
//
// KnowledgeEntityRepository.Get is what that costs: ErrNotFound on SQLite,
// (nil, nil) on Postgres, RunKnowledgeEntitySuite asserting no miss case at
// all, and all eight consumers written for the Postgres shape — so on SQLite,
// which is Community's default and the backend `go test ./...` runs, a missing
// entity aborted graph.Subgraph instead of dropping the seed. A 2026-06-18
// sweep closed eight such gaps by hand; a sweep is a moment, not a gate.
//
// This file is that gate, in the same shape as the miss-contract guard beside
// it: audit the AST, compare against a shrink-only allowlist, and fail on a
// STALE entry too, so a cleanup must delete its line and a covered repository
// cannot leave a slot behind.
//
// See https://docs.vornik.io

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// RepoInterface is a persistence.*Repository interface declaration.
type RepoInterface struct {
	Name string
	File string
	Line int
}

// RepoSuite is an exported Run*Suite in internal/persistence/repotest and the
// repository interfaces its parameters name.
type RepoSuite struct {
	Name string
	// Covers are the persistence.*Repository types this suite takes.
	Covers []string
	File   string
	Line   int
	// Invoked reports that some test in the module calls this suite. A suite
	// that exists and is never called is not coverage — writing one, deleting
	// the allowlist line, and never wiring it up would turn the gate green
	// while nothing runs. Only invoked suites count toward Covered.
	Invoked bool
}

// RepoBackendAudit is the result of scanning a module.
type RepoBackendAudit struct {
	Interfaces []RepoInterface
	Suites     []RepoSuite
	// Covered is the set of interface names named by some suite's parameters.
	Covered map[string]bool
}

// AuditRepoBackendContracts collects the repository interfaces declared in
// internal/persistence and the interfaces the shared suites accept.
//
// It reads SIGNATURES, not names. Suites are named for what they test rather
// than for their argument — RunOpenCheckpointRepairSuite covers two
// repositories and is named for neither — so name matching both misses
// coverage that exists and reports coverage that does not. A check that cries
// wolf gets allowlisted into silence.
func AuditRepoBackendContracts(root string) (RepoBackendAudit, error) {
	audit := RepoBackendAudit{Covered: map[string]bool{}}

	persistenceDir := filepath.Join(root, "internal", "persistence")
	// Non-recursive: the interfaces live in the package root. The postgres/
	// and sqlite/ subpackages hold implementations, and repotest/ holds the
	// suites — neither declares the contract.
	if err := walkGoFilesIn(persistenceDir, false, func(fset *token.FileSet, path, rel string, f *ast.File) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		audit.Interfaces = append(audit.Interfaces, repoInterfacesIn(fset, rel, f)...)
	}); err != nil {
		return RepoBackendAudit{}, err
	}

	repotestDir := filepath.Join(persistenceDir, "repotest")
	if err := walkGoFilesIn(repotestDir, false, func(fset *token.FileSet, path, rel string, f *ast.File) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		audit.Suites = append(audit.Suites, repoSuitesIn(fset, rel, f)...)
	}); err != nil {
		return RepoBackendAudit{}, err
	}

	// A suite counts only if something CALLS it. Not "calls it from both
	// backends": a Postgres-only repository correctly has a Postgres-only
	// suite, and eight of the current sixty are exactly that shape.
	invoked, err := invokedSuites(root)
	if err != nil {
		return RepoBackendAudit{}, err
	}
	for i := range audit.Suites {
		if !invoked[audit.Suites[i].Name] {
			continue
		}
		audit.Suites[i].Invoked = true
		for _, c := range audit.Suites[i].Covers {
			audit.Covered[c] = true
		}
	}

	sort.Slice(audit.Interfaces, func(i, j int) bool { return audit.Interfaces[i].Name < audit.Interfaces[j].Name })
	sort.Slice(audit.Suites, func(i, j int) bool { return audit.Suites[i].Name < audit.Suites[j].Name })
	return audit, nil
}

// repoInterfacesIn collects the `type XxxRepository interface` declarations in
// one file.
func repoInterfacesIn(fset *token.FileSet, rel string, f *ast.File) []RepoInterface {
	var out []RepoInterface
	for _, d := range f.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil {
				continue
			}
			if _, isIface := ts.Type.(*ast.InterfaceType); !isIface {
				continue
			}
			if !strings.HasSuffix(ts.Name.Name, "Repository") {
				continue
			}
			out = append(out, RepoInterface{
				Name: ts.Name.Name,
				File: filepath.ToSlash(filepath.Join("internal", "persistence", filepath.Base(rel))),
				Line: fset.Position(ts.Name.Pos()).Line,
			})
		}
	}
	return out
}

// repoSuitesIn collects the exported Run*Suite declarations in one file, with
// the repository interfaces their parameters name.
func repoSuitesIn(fset *token.FileSet, rel string, f *ast.File) []RepoSuite {
	scope := persistenceScopeOf(f)
	var out []RepoSuite
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		if !strings.HasPrefix(name, "Run") || !strings.HasSuffix(name, "Suite") {
			continue
		}
		covers := repoParamsOf(fn, scope)
		if len(covers) == 0 {
			continue
		}
		out = append(out, RepoSuite{
			Name:   name,
			Covers: covers,
			File:   filepath.ToSlash(filepath.Join("internal", "persistence", "repotest", filepath.Base(rel))),
			Line:   fset.Position(fn.Pos()).Line,
		})
	}
	return out
}

// invokedSuites finds the repotest suites some test in the module calls,
// as `repotest.RunXxxSuite(`.
//
// Textual on purpose: a call may be inside a table, a closure or a helper, and
// resolving that properly would need type information the rest of this package
// deliberately does not load. The failure mode is the safe one — a call this
// misses reads as "not invoked", which surfaces as a finding to look at rather
// than as silent coverage.
func invokedSuites(root string) (map[string]bool, error) {
	invoked := map[string]bool{}
	err := walkGoFiles(root, func(_ *token.FileSet, path, _ string, f *ast.File) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "repotest" {
				return true
			}
			invoked[sel.Sel.Name] = true
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	return invoked, nil
}

// persistenceScope is how one file refers to internal/persistence: the local
// names bound to it, and whether it was dot-imported.
//
// Resolved per file rather than assuming the identifier is literally
// "persistence". An alias would otherwise make a suite read as covering
// NOTHING — which fails loudly for an unlisted repository, but for an
// allowlisted one would leave the entry looking legitimately stale-free while
// a suite existed, so the cleanup prompt never fires (review-20260904-0af0,
// finding 2).
type persistenceScope struct {
	names map[string]bool
	dot   bool
}

func persistenceScopeOf(f *ast.File) persistenceScope {
	scope := persistenceScope{names: map[string]bool{}}
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		if path != persistencePkgPath && !strings.HasSuffix(path, "/"+persistencePkgSuffix) {
			continue
		}
		switch {
		case imp.Name == nil:
			scope.names["persistence"] = true
		case imp.Name.Name == ".":
			scope.dot = true
		case imp.Name.Name == "_":
			// Blank import binds no name.
		default:
			scope.names[imp.Name.Name] = true
		}
	}
	return scope
}

const (
	persistencePkgPath   = "vornik.io/vornik/internal/persistence"
	persistencePkgSuffix = "internal/persistence"
)

// repoParamsOf returns the persistence.*Repository types a suite's parameters
// name, deduplicated and ordered.
func repoParamsOf(fn *ast.FuncDecl, scope persistenceScope) []string {
	if fn.Type == nil || fn.Type.Params == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range fn.Type.Params.List {
		name := persistenceRepoType(p.Type, scope)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// persistenceRepoType unwraps a parameter type to the persistence.*Repository
// it names, or "" for anything else. Handles the pointer, slice and variadic
// forms a future suite might use, under whatever local name the file bound the
// package to.
//
// A struct or interface HOLDING repositories is not unwrapped, and neither is
// a func-typed parameter that returns one. Both read as uncovered, which is
// the safe direction — a finding to look at rather than silent coverage — and
// the fix for either shape is to widen the suite's signature, not this
// function. A suite that takes the repository it tests is also the more
// readable suite.
func persistenceRepoType(expr ast.Expr, scope persistenceScope) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return persistenceRepoType(t.X, scope)
	case *ast.ArrayType:
		return persistenceRepoType(t.Elt, scope)
	case *ast.Ellipsis:
		return persistenceRepoType(t.Elt, scope)
	case *ast.Ident:
		// A dot-import puts the type in scope unqualified.
		if scope.dot && strings.HasSuffix(t.Name, "Repository") {
			return t.Name
		}
		return ""
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok || t.Sel == nil || !scope.names[pkg.Name] {
			return ""
		}
		if !strings.HasSuffix(t.Sel.Name, "Repository") {
			return ""
		}
		return t.Sel.Name
	default:
		return ""
	}
}

// RepoBackendAllowEntry is one allowlist line: an uncovered repository and the
// reason it is uncovered.
type RepoBackendAllowEntry struct {
	Name   string
	Reason string
}

// CheckRepoBackendCoverage reports repositories with no shared suite and
// allowlist entries that have gone stale.
//
// The stale half is what keeps the list moving: an entry naming a repository
// that NOW has a suite is itself a failure, so closing a gap must delete its
// line, and a repository that was deleted cannot leave a slot behind for the
// next one to fall into.
func CheckRepoBackendCoverage(audit RepoBackendAudit, allow map[string]RepoBackendAllowEntry) []Finding {
	const check = "repo-backend-contract"
	var out []Finding

	declared := map[string]RepoInterface{}
	for _, iface := range audit.Interfaces {
		declared[iface.Name] = iface
	}

	for _, iface := range audit.Interfaces {
		if audit.Covered[iface.Name] {
			continue
		}
		if _, ok := allow[iface.Name]; ok {
			continue
		}
		out = append(out, Finding{
			Check: check,
			Name:  iface.Name,
			Detail: "no shared repotest suite takes this repository, so its Postgres and SQLite " +
				"implementations have never been asserted to agree. Add a repotest.Run…Suite that " +
				"accepts it (and run it from both backends' contract tests), or add it to " +
				"cmd/lint-lld-contracts/repo_backend_allowlist.txt with the reason it does not need one",
			Sources: []string{fmt.Sprintf("%s:%d", iface.File, iface.Line)},
		})
	}

	names := make([]string, 0, len(allow))
	for name := range allow {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		iface, exists := declared[name]
		if !exists {
			out = append(out, Finding{
				Check: check,
				Name:  name,
				Detail: "allowlisted but no such repository interface exists in internal/persistence — " +
					"delete the line. A slot left behind is one the next repository falls into silently",
			})
			continue
		}
		if audit.Covered[name] {
			out = append(out, Finding{
				Check: check,
				Name:  name,
				Detail: "allowlisted as uncovered, but a shared suite now takes it — delete the line. " +
					"The list is shrink-only; a stale entry is how it would rot into permanent debt",
				Sources: []string{fmt.Sprintf("%s:%d", iface.File, iface.Line)},
			})
		}
	}
	return out
}

// ParseRepoBackendAllowlist reads the "<Repository>  # <reason>" lines.
//
// A line with no reason is rejected rather than accepted: an unexplained
// exemption is exactly what this file exists to prevent, and the reason is
// what the next reader needs in order to close it.
func ParseRepoBackendAllowlist(data string) (map[string]RepoBackendAllowEntry, error) {
	out := map[string]RepoBackendAllowEntry{}
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, found := strings.Cut(line, "#")
		name = strings.TrimSpace(name)
		reason = strings.TrimSpace(reason)
		if name == "" {
			continue
		}
		if !found || reason == "" {
			return nil, fmt.Errorf("repo-backend allowlist line %d (%q): every entry needs a reason after '#' — "+
				"an exemption nobody explained is the thing this list exists to prevent", i+1, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("repo-backend allowlist line %d: %q is listed twice", i+1, name)
		}
		out[name] = RepoBackendAllowEntry{Name: name, Reason: reason}
	}
	return out, nil
}
