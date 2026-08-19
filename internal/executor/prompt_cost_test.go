package executor

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/promptblock"
)

// updateGolden regenerates the goldens and the fixture pin. It exists because
// hand-maintaining them is worse, not because regenerating is routine: a golden
// diff is the finding, so `-update` belongs in the commit that intends the
// growth and nowhere else.
var updateGolden = flag.Bool("update", false, "regenerate prompt-cost goldens and fixture hashes")

// L1 of the agent-quality benchmark
// (https://docs.vornik.io §3.1).
//
// These tests are the gate itself, not a check on it. The golden files record
// what each role's prompt costs by source, so a change that grows the prompt
// fails here with the growth attributed rather than surfacing months later as a
// bigger bill.

// --- §3.1(a): the gate is on bytes -----------------------------------------

func TestAttributePromptCost_TotalIsTheSumOfItsParts(t *testing.T) {
	cost := AttributePromptCost(fixtureAllBlocks())
	sum := 0
	for _, s := range cost.Sources {
		sum += s.Bytes
	}
	if sum != cost.TotalBytes {
		t.Fatalf("attribution does not close: sources sum to %d, total is %d — "+
			"an unattributed byte is a byte nobody is accountable for", sum, cost.TotalBytes)
	}
}

func TestAttributePromptCost_AttributesEachBlockSeparately(t *testing.T) {
	cost := AttributePromptCost(fixtureAllBlocks())

	// A block's cost is its text PLUS the separator the composer joins it with,
	// because that is what the prompt actually grows by. canonical-context
	// joins with "\n\n"; the other two with "\n". Pinning the join here means a
	// composer that changes its separator shows up as an attribution change
	// rather than sliding silently into every golden.
	want := map[string]int{
		promptblock.CanonicalContext:   len(canonicalContextSystemPromptBlock) + len("\n\n"),
		promptblock.ToolBudget:         len(toolGrantSystemPromptBlock) + len("\n"),
		promptblock.ReportingIntegrity: len(claimVerificationSystemPromptBlock) + len("\n"),
	}
	for name, wantBytes := range want {
		got, ok := cost.Source(name)
		if !ok {
			t.Fatalf("block %q missing from attribution — every injected block must be priced", name)
		}
		if got.Bytes != wantBytes {
			t.Errorf("block %q attributed %d bytes, block constant is %d",
				name, got.Bytes, wantBytes)
		}
	}
}

// A suppressed block must cost nothing. This is the measurement that prices
// 22989f97 (per-swarm suppression of advisory blocks).
func TestAttributePromptCost_SuppressedBlockCostsNothing(t *testing.T) {
	base := AttributePromptCost(fixtureAllBlocks())

	f := fixtureAllBlocks()
	f.SuppressedGuidanceBlocks = []string{promptblock.CanonicalContext}
	suppressed := AttributePromptCost(f)

	if _, ok := suppressed.Source(promptblock.CanonicalContext); ok {
		t.Error("suppressed block still appears in the attribution")
	}
	saved := base.TotalBytes - suppressed.TotalBytes
	wantSaved := len(canonicalContextSystemPromptBlock) + len("\n\n")
	if saved != wantSaved {
		t.Errorf("suppressing canonical-context saved %d bytes, want %d — "+
			"the saving must equal the block plus its join, or the attribution is lying "+
			"about what suppression buys", saved, wantSaved)
	}
}

// The invariant block is not suppressible. Config validation refuses such a
// list and agent_input_context.go does not route it through suppressed(); this
// asserts the attribution reflects that rather than implying a saving an
// operator cannot have.
func TestAttributePromptCost_InvariantBlockIgnoresSuppression(t *testing.T) {
	f := fixtureAllBlocks()
	f.SuppressedGuidanceBlocks = []string{promptblock.ReportingIntegrity}

	cost := AttributePromptCost(f)
	got, ok := cost.Source(promptblock.ReportingIntegrity)
	if !ok {
		t.Fatal("reporting-integrity vanished when an operator named it — " +
			"it is an invariant block and may not be suppressed")
	}
	if want := len(claimVerificationSystemPromptBlock) + len("\n"); got.Bytes != want {
		t.Errorf("reporting-integrity attributed %d bytes, want %d", got.Bytes, want)
	}
}

// --- Completeness: every injected block must be priced ----------------------

func TestAttributePromptCost_PricesEveryRegisteredBlock(t *testing.T) {
	cost := AttributePromptCost(fixtureAllBlocks())
	for _, b := range injectedBlocks {
		if _, ok := cost.Source(b.Name); !ok {
			t.Errorf("registered block %q is injected but never priced — "+
				"a block can be added to injectedBlocks and escape L1 without this law", b.Name)
		}
	}
}

// --- §3.1(c) law 1: the composers are pure by signature ---------------------

// The production composers are what L1 runs; a separate composer would gate a
// copy. Purity is therefore a property of the real chain, asserted here over
// its source. Impure identifiers may appear in a RESOLVER (canonical_context.go
// legitimately reads files) — the law covers the compose* functions only.
func TestComposeChain_IsPureBySignature(t *testing.T) {
	// Matched on the package qualifier, not by substring: a substring test for
	// "os." also fires on a variable named `pos`, and a law with false
	// positives gets weakened by the first person it inconveniences.
	bannedPkg := map[string]bool{"os": true, "sql": true, "http": true}
	bannedPair := map[string]bool{"time.Now": true, "context.Context": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "composeSystemPromptWith") {
				continue
			}
			checked++
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				pair := id.Name + "." + sel.Sel.Name
				if bannedPkg[id.Name] || bannedPair[pair] {
					t.Errorf("%s references %s — a composer that reaches for live state "+
						"makes the L1 golden track the developer's machine (§3.1c)",
						fn.Name.Name, pair)
				}
				return true
			})
		}
	}

	// A law that checks nothing passes silently. injected_blocks.go guards its
	// own scanner for the same reason.
	if checked == 0 {
		t.Fatal("found no composeSystemPromptWith* functions — the scan is broken, " +
			"which would make this law vacuous")
	}
}

// --- §3.1(c) law 2: the fixture loader reads only from testdata -------------

func TestFixtureLoader_ReadsOnlyFromTestdata(t *testing.T) {
	src, err := os.ReadFile("prompt_cost.go")
	if err != nil {
		t.Fatalf("read prompt_cost.go: %v", err)
	}
	body := string(src)

	// The loader is the only thing in this file permitted to touch the
	// filesystem, and only under the fixture root. Any other read path is how
	// live state reaches the golden without tripping law 1.
	for _, forbidden := range []string{"os.Getenv", "filepath.Walk", "http.", "sql."} {
		if strings.Contains(body, forbidden) {
			t.Errorf("prompt_cost.go references %q — the hydration path must reach "+
				"nothing but %s (§3.1c law 2)", forbidden, promptCostFixtureRoot)
		}
	}
	if !strings.Contains(body, promptCostFixtureRoot) {
		t.Fatalf("prompt_cost.go never names %s — the scan cannot prove what the loader reads",
			promptCostFixtureRoot)
	}
}

// A fixture edited without updating its hash fails. Without this, the golden
// silently tracks whatever the fixture happens to say today.
func TestFixtures_MatchTheirPinnedHashes(t *testing.T) {
	if *updateGolden {
		names, err := FixtureNames()
		if err != nil {
			t.Fatalf("list fixtures: %v", err)
		}
		var b strings.Builder
		b.WriteString("# sha256 of each pinned L1 fixture (§3.1c law 2).\n")
		b.WriteString("# Regenerate with: go test ./internal/executor/ -run PromptCost -update\n")
		for _, n := range names {
			h, err := HashFixture(n)
			if err != nil {
				t.Fatalf("hash %q: %v", n, err)
			}
			b.WriteString(h + "  " + n + ".fixture.json\n")
		}
		if err := os.WriteFile(promptCostHashFile, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write pin: %v", err)
		}
	}

	pinned, err := LoadFixtureHashes()
	if err != nil {
		t.Fatalf("load pinned hashes: %v", err)
	}
	if len(pinned) == 0 {
		t.Fatal("no pinned fixture hashes — the pin is vacuous")
	}
	for name, wantHash := range pinned {
		gotHash, err := HashFixture(name)
		if err != nil {
			t.Errorf("hash fixture %q: %v", name, err)
			continue
		}
		if gotHash != wantHash {
			t.Errorf("fixture %q changed (%s, pinned %s) — update %s deliberately, "+
				"since every golden below it moves too",
				name, gotHash[:12], wantHash[:12], promptCostHashFile)
		}
	}
}

// --- The golden itself ------------------------------------------------------

func TestAttributePromptCost_MatchesGolden(t *testing.T) {
	names, err := FixtureNames()
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no fixtures found — the golden gate would pass vacuously")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			f, err := LoadFixture(name)
			if err != nil {
				t.Fatalf("load fixture: %v", err)
			}
			got := AttributePromptCost(f)

			goldenPath := filepath.Join(promptCostFixtureRoot, name+".golden.json")
			if *updateGolden {
				blob, err := json.MarshalIndent(got, "", "  ")
				if err != nil {
					t.Fatalf("marshal golden: %v", err)
				}
				if err := os.WriteFile(goldenPath, append(blob, '\n'), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("regenerated %s (%d bytes attributed)", goldenPath, got.TotalBytes)
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (regenerate deliberately, never blindly): %v", err)
			}
			var wantCost PromptCost
			if err := json.Unmarshal(want, &wantCost); err != nil {
				t.Fatalf("parse golden: %v", err)
			}

			if got.TotalBytes != wantCost.TotalBytes {
				t.Errorf("total bytes %d, golden %d (delta %+d)",
					got.TotalBytes, wantCost.TotalBytes, got.TotalBytes-wantCost.TotalBytes)
			}
			if len(got.Sources) != len(wantCost.Sources) {
				t.Fatalf("source count %d, golden %d", len(got.Sources), len(wantCost.Sources))
			}
			for i, src := range got.Sources {
				w := wantCost.Sources[i]
				if src.Name != w.Name || src.Bytes != w.Bytes {
					t.Errorf("source %d: got %s=%d, golden %s=%d",
						i, src.Name, src.Bytes, w.Name, w.Bytes)
				}
			}
		})
	}
}

// fixtureAllBlocks is the in-code fixture for the unit laws above: every block
// eligible, nothing suppressed. The testdata fixtures drive the golden.
func fixtureAllBlocks() PromptCostFixture {
	return PromptCostFixture{
		Role:               "lead",
		RolePrompt:         "You are the lead.",
		ToolGrantAvailable: true,
		// The fixture is named "all blocks", and the completeness law below
		// prices whatever it produces — so a block gated on a deployment fact
		// has to have that fact set here or it silently escapes L1.
		WorktreeGitReadOnly: true,
		CanonicalContext: CanonicalContext{
			ProjectContext: "project context body",
			Source:         "dot_autonomy",
		},
		Skills: []SkillIndexEntry{{Name: "deploy", Description: "how to deploy"}},
	}
}
