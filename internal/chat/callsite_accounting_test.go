package chat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This is the guard, and it is the part of the cost-accounting work with
// lasting value. Three separate times a call site was added that made
// real LLM calls and recorded no spend:
//
//   - instinct.distiller — the field existed and was assigned only in a
//     test; production never wired it (fixed 2026-07-15 / 2026-07-30).
//   - memory.reranker — no usage field at all, on the hottest path in
//     the daemon (fixed 2026-07-31).
//   - memetic.architect — still unaccounted, deliberately; see the
//     registry note.
//
// Each was found by an operator noticing a discrepancy on a bill, not by
// the test suite. The failure mode is silent by construction: a nil
// recorder records nothing without complaining. So the enforcement here
// is deliberately blunt — every call site that exists in production code
// must be CLASSIFIED in the registry below. Adding a new one without a
// decision about its cost accounting breaks this test.
//
// It does not (and cannot cheaply) prove a recorder is non-nil at
// runtime; the per-role unit tests do that. What it proves is that
// nobody added a spender nobody thought about.

type callSiteAccounting struct {
	// accounted is true when a task_llm_usage row is written for every
	// billed call from this site.
	accounted bool
	// note explains the wiring point when accounted, or WHY not and what
	// it would take, when not. Required either way — an undocumented
	// entry is how the last three regressions survived review.
	note string
	// enterpriseOnly marks a call site declared under internal/enterprise,
	// which the CE export strips. The shared registry ships to CE (this is a
	// _test.go file), so without this flag the stale-entries check flags such a
	// site as undeclared on the CE tree and reds the public build — even though
	// it is correctly declared in EE. See TestRegistryHasNoStaleEntries.
	enterpriseOnly bool
}

var callSiteRegistry = map[string]callSiteAccounting{
	"memory.classifier": {
		accounted: true,
		note:      "Classifier.LLMUsage + Pricing, wired container_scheduler.go post-construction.",
	},
	"memory.titler": {
		accounted: true,
		note:      "Titler.LLMUsage + Pricing, wired container_scheduler.go post-construction.",
	},
	"memory.narrative": {
		accounted: true,
		note:      "LLMConsolidateWorker.LLMUsage + Pricing, wired container_scheduler.go.",
	},
	"memory.graph": {
		accounted: true,
		note:      "Pipeline.LLMUsage + Pricing, wired container_autonomy.go; one row per stage.",
	},
	"memory.reranker": {
		accounted: true,
		note: "LLMReranker.LLMUsage + Pricing via WithRerankerUsage, wired container_scheduler.go. " +
			"Records BEFORE parsing, because the degrade-to-RRF path is billed too.",
	},
	"judge": {
		accounted: true,
		note:      "Judge/runner LLMUsage + Pricing, wired container_judge.go.",
	},
	"narrator.line": {
		accounted: true,
		note: "Narrator LLMUsage + Pricing, wired container_narrator.go. Declared via the " +
			"narratorCallSite CONSTANT rather than a literal, which is why a grep-based " +
			"audit of call sites missed it entirely on 2026-07-30.",
	},
	"instinct.distiller": {
		accounted:      true,
		enterpriseOnly: true, // declared in internal/enterprise/instinct/engine/distiller.go — stripped from CE
		note: "Distiller.LLMUsage + Pricing, wired instinct/subsystem.go wireDistiller. " +
			"Previously assigned only in distiller_test.go — the seam existed and " +
			"production never used it.",
	},
	"memetic.architect": {
		accounted: false,
		note: "DELIBERATE, NOT FORGOTTEN (assessed 2026-07-31). Blocked on an attribution " +
			"decision, not on wiring: task_llm_usage.project_id is NOT NULL, and the " +
			"architect has no project in scope — it is constructed over a daemon-level " +
			"fsWorkflowSource (configDir/workflows/<id>.md) with no project anywhere in " +
			"its dependencies. Options are (a) a sentinel project id, which puts a " +
			"non-project row into every per-project rollup, (b) resolving a project from " +
			"workflow frontmatter, if one is ever declared there, or (c) recording " +
			"daemon-level spend somewhere other than task_llm_usage. Frequency is low " +
			"(architect turns are operator- or schedule-triggered, not per-request), so " +
			"this is the smaller half of the 2026-07-30 discrepancy.",
	},
}

// TestEveryCallSiteIsClassified fails when production code declares a
// chat call site the registry above does not classify.
func TestEveryCallSiteIsClassified(t *testing.T) {
	root := repoRoot(t)
	found := discoverCallSites(t, root)

	if len(found) == 0 {
		t.Fatal("discovered no call sites at all — the scanner is broken, not the code")
	}

	for site, file := range found {
		entry, ok := callSiteRegistry[site]
		if !ok {
			t.Errorf(
				"call site %q (%s) makes LLM calls but is not classified in callSiteRegistry.\n"+
					"Decide whether it records spend, wire it if it should, then add an entry.\n"+
					"An unclassified spender is how the reranker went ~1,637 calls/day unbilled.",
				site, file)
			continue
		}
		if strings.TrimSpace(entry.note) == "" {
			t.Errorf("call site %q has a registry entry with no note; document the wiring point or the gap", site)
		}
	}
}

// TestRegistryHasNoStaleEntries catches the opposite drift: a registry
// entry whose call site no longer exists, which would leave a false
// record of a spender that was removed or renamed.
func TestRegistryHasNoStaleEntries(t *testing.T) {
	root := repoRoot(t)
	found := discoverCallSites(t, root)

	// The CE export strips internal/enterprise, so an enterprise-only call site
	// is legitimately undiscoverable on the CE tree. Skip such entries when the
	// enterprise tree is absent, so the shared registry can carry them for EE
	// without reding the public CE build. On EE the tree is present and the
	// entries are discovered, so this changes nothing there.
	_, enterpriseErr := os.Stat(filepath.Join(root, "internal", "enterprise"))
	enterprisePresent := enterpriseErr == nil

	for site, entry := range callSiteRegistry {
		if _, ok := found[site]; ok {
			continue
		}
		if entry.enterpriseOnly && !enterprisePresent {
			continue // EE-only site, correctly absent on the CE tree
		}
		t.Errorf("callSiteRegistry lists %q but no production code declares it — remove the entry or restore the call site", site)
	}
}

// TestUnaccountedCallSitesAreDocumented keeps a known gap honest. It
// permits accounted:false, but only with a real explanation, so the next
// reader learns why rather than assuming an oversight.
func TestUnaccountedCallSitesAreDocumented(t *testing.T) {
	for site, entry := range callSiteRegistry {
		if entry.accounted {
			continue
		}
		if len(strings.TrimSpace(entry.note)) < 80 {
			t.Errorf("call site %q is unaccounted; its note must explain why and what would fix it (got %d chars)",
				site, len(strings.TrimSpace(entry.note)))
		}
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod above the test directory")
		}
		dir = parent
	}
}

// discoverCallSites parses every non-test .go file under internal/ and
// cmd/ and returns each distinct chat.WithCallSite label mapped to the
// file that declares it.
//
// Constants are resolved, not just literals. The narrator declares its
// site as `const narratorCallSite = "narrator.line"`, and a scan that
// only understood string literals would silently omit it — which is
// exactly what happened to the hand audit this test replaces.
func discoverCallSites(t *testing.T, root string) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	type parsed struct {
		file *ast.File
		path string
	}
	var files []parsed
	// consts is keyed by "<dir>\x00<name>" so a constant resolves only
	// within its own package, never across packages that reuse a name.
	consts := map[string]string{}

	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "testdata" || info.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// A file this test cannot parse is not this test's
				// problem — the build catches it.
				return nil
			}
			files = append(files, parsed{file: f, path: path})
			dir := filepath.Dir(path)
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
								consts[dir+"\x00"+name.Name] = v
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}

	found := map[string]string{}
	for _, p := range files {
		dir := filepath.Dir(p.path)
		ast.Inspect(p.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "WithCallSite" {
				return true
			}
			switch arg := call.Args[1].(type) {
			case *ast.BasicLit:
				if arg.Kind == token.STRING {
					if v, err := strconv.Unquote(arg.Value); err == nil && v != "" {
						found[v] = mustRel(root, p.path)
					}
				}
			case *ast.Ident:
				if v, ok := consts[dir+"\x00"+arg.Name]; ok && v != "" {
					found[v] = mustRel(root, p.path)
				} else {
					t.Errorf("%s: WithCallSite uses identifier %q that this scanner could not resolve to a string constant; "+
						"the guard cannot classify it", mustRel(root, p.path), arg.Name)
				}
			default:
				// Anything else — a concatenation (`"memory." + name`), a
				// function call, a struct field, a variable assigned at
				// runtime — is a label this scanner cannot resolve, and
				// therefore a spender it cannot classify. Falling through
				// silently here would be a false green of exactly the kind
				// this guard exists to prevent (companion review
				// task_20260731103649, finding THREE). Fail instead, and
				// require the label be a literal or a package-scoped
				// constant. Every one of the 9 current sites already is.
				t.Errorf("%s: WithCallSite argument is a %T, which this guard cannot resolve to a static label. "+
					"Use a string literal or a package-scoped string constant so the call site can be classified.",
					mustRel(root, p.path), arg)
			}
			return true
		})
	}
	return found
}

func mustRel(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}
