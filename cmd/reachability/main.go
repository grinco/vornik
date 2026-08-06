// Command reachability answers "is everything that exists either called or
// named?" — the code half of
// https://docs.vornik.io
//
// It is NOT in the fast lint path. Whole-program RTA across ten main packages
// takes minutes; a slow lint gets skipped, and a skipped gate is no gate (§8).
// Run it via `make reachability`.
//
// The model is bipartite. A symbol is live when it is either:
//
//   - live-by-code: reachable from some main through the call graph, or
//   - live-by-contract: named by a machine-readable declaration
//     (internal/contractreg).
//
// The second axis is what makes this usable here. Most of vornik's interesting
// entry points are dispatched by NAME — agent tools from entrypoint.sh, system
// handlers from workflow YAML, extractors by MIME type — and a call-graph-only
// analysis reports every one of them as dead. That is not noise to suppress; it
// means the model is wrong.
//
// Verdicts:
//
//	unreferenced   neither reachable nor named. Deliberately NOT called "dead":
//	               the observation cannot distinguish genuinely dead code from an
//	               UNDOCUMENTED ENTRY POINT — a real feature invoked by name that
//	               nobody wrote a contract for. Same signature, different defect,
//	               so the tool reports both readings and asserts neither.
//	test-only      reachable only from _test.go. Reported separately and NOT
//	               ratcheted: a real smell (a helper for a feature that no longer
//	               ships) but it would swamp the primary signal.
//
// Exit status: 0 when the unreferenced count is at or below the ceiling, 1 when
// it rises, 2 on an internal error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/contractreg"
)

// deadcodePkg is one package in `deadcode -json` output.
type deadcodePkg struct {
	Name  string `json:"Name"`
	Path  string `json:"Path"`
	Funcs []struct {
		Name     string `json:"Name"`
		Position struct {
			File string `json:"File"`
			Line int    `json:"Line"`
		} `json:"Position"`
		Generated bool `json:"Generated"`
	} `json:"Funcs"`
}

// symbol identifies one function across runs.
type symbol struct {
	Pkg  string
	Func string
	File string
	Line int
}

func (s symbol) key() string { return s.Pkg + "." + s.Func }

func main() {
	var (
		mapOut  = flag.String("map", "", "write the dependency/reachability map as JSON to this path")
		ratchet = flag.Bool("ratchet", false, "record the current unreferenced count as the new ceiling")
		tool    = flag.String("deadcode", "", "path to the deadcode binary (default: $GOPATH/bin/deadcode)")
	)
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	// Default to `go tool deadcode`, whose version is pinned by the go.mod tool
	// directive. An explicit -deadcode path overrides it.
	//
	// NOT `go install ...@latest`: that made CI non-reproducible, and this gate
	// compares a COUNT against a ceiling. A new upstream release could shift the
	// count and fail the ratchet with no local reproduction — during a release, in
	// the worst case. Pinning also removes the install step from CI entirely.
	bin := *tool

	mains, err := mainPackages(root)
	if err != nil {
		fatal(err)
	}
	if len(mains) == 0 {
		fatal(fmt.Errorf("no main packages found under ./cmd — refusing to report everything as dead"))
	}
	fmt.Printf("reachability: analysing %d main package(s)\n", len(mains))

	// ONE run over ALL mains, not one run per main.
	//
	// The first cut ran deadcode per main and intersected the unreachable sets.
	// That is WRONG, and the bug was visible immediately: cmd/agent-helper
	// reported 0 unreachable, which emptied the intersection and produced a
	// triumphant "0 unreferenced". `deadcode ./cmd/X` only considers packages in
	// X's own import graph, so a function in a package X never imports is absent
	// from X's output entirely — absence means OUT OF SCOPE, not reachable.
	// Intersecting those sets silently answers a different question.
	//
	// Passing every main in one invocation is what the tool is for: all entry
	// points form one program, and a function is reported only when no main
	// reaches it. That also makes the EE/CE union fall out for free, since both
	// assembly mains are in the set.
	unreachable, err := runDeadcode(bin, root, mains, false)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("  %d function(s) unreachable from every main\n", len(unreachable))

	// SECOND run including test executables.
	//
	// Without this the count is inflated and the test-only bucket is a lie. The
	// first real triage showed why: ContextWithScopeForTesting, ContextWithAdmin,
	// ContextWithProjectScope and friends are production-file helpers used only by
	// _test.go, and `deadcode` ignores tests by default — so they looked dead.
	// Deleting any of them would have broken the suite.
	//
	// The difference between the two runs IS the test-only class:
	//   unreachable in both            → genuinely unreferenced
	//   unreachable only without tests → reachable from tests only
	// Scope is ./... , not the mains: -test builds test executables only for the
	// packages NAMED, so passing ./cmd/... analysed cmd's tests and missed every
	// internal test. That mattered enormously — 166 of the first 195 turned out to
	// be test-reachable once the scope was right.
	reachableFromTests, err := runDeadcode(bin, root, []string{"./..."}, true)
	if err != nil {
		fatal(err)
	}

	table, err := buildTable(root)
	if err != nil {
		fatal(err)
	}

	unreferenced, testOnly, rescued := classify(table, unreachable, reachableFromTests)

	report(table, unreferenced)
	fmt.Printf("\nreachability: %d unreferenced, %d test-only, %d rescued by a contract\n",
		len(unreferenced), len(testOnly), len(rescued))

	if *mapOut != "" {
		if werr := writeMap(*mapOut, mains, table, unreferenced, testOnly, rescued); werr != nil {
			fatal(werr)
		}
		fmt.Printf("reachability: wrote %s\n", *mapOut)
	}

	ceilingPath := filepath.Join(root, "scripts", "reachability-ceiling.txt")
	if *ratchet {
		if werr := writeCeiling(ceilingPath, len(unreferenced)); werr != nil {
			fatal(werr)
		}
		fmt.Printf("reachability: ratcheted ceiling to %d\n", len(unreferenced))
		return
	}
	limit, err := loadCeiling(ceilingPath)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("reachability: ceiling %d\n", limit)
	if len(unreferenced) > limit {
		fmt.Fprintf(os.Stderr, "\nreachability: unreferenced count %d EXCEEDS the ceiling %d.\n"+
			"Either delete the code, or add the missing contract — both are valid fixes, and which "+
			"one applies is the judgement this tool deliberately leaves to you.\n",
			len(unreferenced), limit)
		os.Exit(1)
	}
}

// classify splits the unreachable set across the three verdicts.
//
// NOTE on the two zero buckets you will see today, stated so they are not
// mistaken for coverage:
//   - rescued-by-contract is empty because this repo's named-dispatch tools are
//     implemented in SHELL (entrypoint.sh), so no Go symbol carries an agent-tool
//     name. The contract axis still earns its keep in the phantom and
//     registry-agreement checks; it just rescues nothing on the Go side until
//     system-handler and extractor names are enumerated into the table.
func classify(table *contractreg.Table, unreachable, stillUnreachableWithTests map[string]symbol) (unreferenced, testOnly, rescued []symbol) {
	for k, s := range unreachable {
		_, deadEvenWithTests := stillUnreachableWithTests[k]
		switch {
		case strings.HasSuffix(s.File, "_test.go"):
			testOnly = append(testOnly, s)
		case !deadEvenWithTests:
			// Reachable once test executables are included: a production helper
			// exercised only by tests. Not dead — deleting it breaks the suite.
			testOnly = append(testOnly, s)
		case namedByContract(table, s.Func):
			rescued = append(rescued, s)
		default:
			unreferenced = append(unreferenced, s)
		}
	}
	sortSymbols(unreferenced)
	sortSymbols(testOnly)
	sortSymbols(rescued)
	return unreferenced, testOnly, rescued
}

// report prints each unreferenced symbol with the evidence trail behind the
// verdict. A bare label reads as bureaucratic to someone asking "can I delete
// this?", so the output names which registries were consulted and came back
// empty.
func report(table *contractreg.Table, syms []symbol) {
	if len(syms) == 0 {
		return
	}
	consulted := make([]string, 0, len(table.Kinds()))
	for _, k := range table.Kinds() {
		if k == contractreg.KindDeclared {
			continue
		}
		consulted = append(consulted, string(k))
	}
	fmt.Printf("\n== unreferenced (neither reachable from a main nor named by a contract) ==\n")
	for _, s := range syms {
		fmt.Printf("%s.%s\n  %s:%d\n  not named in: %s\n  → likely dead; no contract found. "+
			"Delete it, or add the missing contract.\n",
			s.Pkg, s.Func, s.File, s.Line, strings.Join(consulted, ", "))
	}
}

// namedByContract asks whether a Go function corresponds to a contract-declared
// name. The match is on the function's simple name against the contract
// vocabulary, in both the Go and snake_case spellings.
//
// LIMITATION, stated rather than hidden: this is a NAME correspondence, not a
// proof of registration. A handler called Render implementing the contract name
// "render" is rescued whether or not it is the one actually registered. The
// asymmetry is deliberate — a false rescue costs one missed report, while a false
// unreferenced verdict costs trust, and trust is what this tool spends.
func namedByContract(table *contractreg.Table, funcName string) bool {
	simple := funcName
	if i := strings.LastIndex(simple, "."); i >= 0 {
		simple = simple[i+1:]
	}
	for _, cand := range []string{simple, toSnake(simple), strings.ToLower(simple)} {
		if table.AnyNamed(cand) {
			return true
		}
	}
	return false
}

func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// runDeadcode invokes deadcode for one main package and returns its unreachable
// symbols keyed by pkg.func.
func runDeadcode(bin, root string, mainPkgs []string, withTests bool) (map[string]symbol, error) {
	args := []string{"-json"}
	if withTests {
		args = append(args, "-test")
	}
	args = append(args, mainPkgs...)

	// bin == "" means "use the pinned tool" — `go tool deadcode <args>`.
	name := bin
	if name == "" {
		name = "go"
		args = append([]string{"tool", "deadcode"}, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, stderrOf(err))
	}
	var pkgs []deadcodePkg
	if uerr := json.Unmarshal(out, &pkgs); uerr != nil {
		return nil, fmt.Errorf("parse deadcode json: %w", uerr)
	}
	res := map[string]symbol{}
	for _, p := range pkgs {
		for _, f := range p.Funcs {
			if f.Generated {
				continue // generated code is not the author's to delete
			}
			s := symbol{Pkg: p.Path, Func: f.Name, File: f.Position.File, Line: f.Position.Line}
			res[s.key()] = s
		}
	}
	return res, nil
}

func stderrOf(err error) string {
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return ""
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// mainPackages lists every main package under ./cmd. "Dead" only means anything
// relative to the FULL set: a symbol reached only by vornikctl is live, and both
// edition assembly mains (cmd/vornik for CE, cmd/vornik-enterprise for EE) are
// here, so the union covers both editions without an export-then-analyse dance.
func mainPackages(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, "./cmd/"+e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func buildTable(root string) (*contractreg.Table, error) {
	t := contractreg.New()
	t.AddAgentToolsGo()
	t.AddSystemHandlers()
	if err := t.AddEntrypointSurfaces(filepath.Join(root, "images", "vornik-agent", "entrypoint.sh")); err != nil {
		return nil, err
	}
	return t, nil
}

func writeMap(path string, mains []string, table *contractreg.Table, unref, testOnly, rescued []symbol) error {
	type doc struct {
		Mains        []string            `json:"mains"`
		Contracts    map[string][]string `json:"contracts"`
		Unreferenced []symbol            `json:"unreferenced"`
		TestOnly     []symbol            `json:"test_only"`
		Rescued      []symbol            `json:"rescued_by_contract"`
	}
	// Coerce nil slices to empty ones. A nil slice marshals as `null`, which
	// forces every consumer of this artifact to null-check before iterating —
	// hostile for something whose whole purpose is being queried. An empty
	// verdict list is a real, common state (7 unreferenced today, 0 rescued).
	d := doc{
		Mains:        mains,
		Contracts:    map[string][]string{},
		Unreferenced: nonNil(unref),
		TestOnly:     nonNil(testOnly),
		Rescued:      nonNil(rescued),
	}
	for _, k := range table.Kinds() {
		d.Contracts[string(k)] = table.Names(k)
	}
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// nonNil guarantees a JSON array rather than null.
func nonNil(s []symbol) []symbol {
	if s == nil {
		return []symbol{}
	}
	return s
}

func sortSymbols(s []symbol) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Pkg != s[j].Pkg {
			return s[i].Pkg < s[j].Pkg
		}
		return s[i].Func < s[j].Func
	})
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "reachability: %v\n", err)
	os.Exit(2)
}
