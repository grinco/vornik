package executor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/promptblock"
)

// The aggregate budget for injected guidance blocks (LLD 09 §13.3(2)).
//
// This is the bound that binds. Per-block ceilings let N blocks each inside its own
// budget compose one large prompt — the exact failure the whole token-reduction line
// of work started from.
const injectedBlocksAggregateBudget = 3000

func TestInjectedBlocks_AggregateStaysBounded(t *testing.T) {
	total := injectedBlocksAggregateBytes()
	if total > injectedBlocksAggregateBudget {
		var b strings.Builder
		for _, blk := range injectedBlocks {
			b.WriteString("\n  ")
			b.WriteString(blk.Name)
			b.WriteString(": ")
			b.WriteString(itoa(len(blk.Text)))
		}
		t.Errorf("injected blocks total %d bytes, over the %d-byte budget.%s\n"+
			"A new block must fit the whole allowance, not just its own — adding blocks is "+
			"how a token-reduction feature turns into a token cost. Trim an existing block "+
			"rather than raising this bound.", total, injectedBlocksAggregateBudget, b.String())
	}
	t.Logf("injected guidance: %d/%d bytes used", total, injectedBlocksAggregateBudget)
}

// TestInjectedBlockRegistry_IsComplete is what makes the budget enforceable rather
// than advisory.
//
// The budget test sums the registry, so a block that is injected but NOT registered
// is invisible to it — the prompt grows and CI stays green. Absence would be read as
// a value, again. This reads the package source and requires every declared
// *SystemPromptBlock constant to appear in the registry, so the only way to add a
// block is to add it to the budget too.
func TestInjectedBlockRegistry_IsComplete(t *testing.T) {
	declared := declaredBlockConstants(t)
	if len(declared) == 0 {
		t.Fatal("found no *SystemPromptBlock constants in the package — the scan is broken, " +
			"which would make this law silently vacuous")
	}

	registered := map[string]bool{}
	for _, b := range injectedBlocks {
		registered[b.Const] = true
	}
	for _, name := range declared {
		if !registered[name] {
			t.Errorf("%s is declared but not in injectedBlocks. An unregistered block is "+
				"excluded from the aggregate budget, so the prompt can grow past %d bytes "+
				"with CI green. Add it to the registry with its advisory/invariant class.",
				name, injectedBlocksAggregateBudget)
		}
	}
	for _, b := range injectedBlocks {
		if b.Name == "" {
			t.Errorf("registry entry %+v is missing a name; the name is what an operator writes "+
				"in suppressedGuidanceBlocks and what carries the class (LLD 09 §13.3(4))", b)
			continue
		}
		if _, ok := promptblock.ClassOf(b.Name); !ok {
			t.Errorf("block %q is registered here but not declared in internal/promptblock, so it "+
				"has no class — config validation cannot decide whether an operator may "+
				"suppress it (LLD 09 §13.3(4))", b.Name)
		}
	}
}

// TestInjectedBlockRegistry_MatchesPromptblockDeclaration keeps the two halves of the
// registry in step. The TEXT lives here, next to the composition that emits it; the
// NAME and CLASS live in internal/promptblock, because internal/registry has to
// validate an operator's suppression list and cannot import this package.
//
// A block declared there but never registered here is a name an operator can suppress
// with no effect. One registered here but not declared there has no class, so
// suppression cannot reason about it at all. Both directions are failures.
func TestInjectedBlockRegistry_MatchesPromptblockDeclaration(t *testing.T) {
	registered := map[string]bool{}
	for _, b := range injectedBlocks {
		registered[b.Name] = true
	}
	for _, name := range promptblock.Names() {
		if !registered[name] {
			t.Errorf("promptblock declares %q but no registry entry emits it: an operator could "+
				"suppress a block that does not exist and see no change", name)
		}
	}
	for name := range registered {
		if !promptblock.Known(name) {
			t.Errorf("registry emits %q but promptblock does not declare it: the block has no "+
				"advisory/invariant class", name)
		}
	}
}

// TestInjectedBlockRegistry_ReportingIntegrityIsInvariant pins the one classification
// that carries a rule rather than a hint. If this block were ever reclassified
// advisory, an operator could suppress being TOLD about a check that still runs — a
// deployment misdescribing a rule it is subject to.
func TestInjectedBlockRegistry_ReportingIntegrityIsInvariant(t *testing.T) {
	for _, b := range injectedBlocks {
		if b.Const == "claimVerificationSystemPromptBlock" {
			if b.Name != promptblock.ReportingIntegrity {
				t.Fatalf("reporting-integrity block is registered under name %q", b.Name)
			}
			if c, _ := promptblock.ClassOf(b.Name); c != promptblock.Invariant {
				t.Errorf("reporting integrity is classed %q; verifyRoleClaims runs whatever the "+
					"prompt says, so suppressing the block removes the warning and not the rule", c)
			}
			return
		}
	}
	t.Error("reporting-integrity block is not registered at all")
}

var blockConstRE = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w*SystemPromptBlock)\s*=`)

// declaredBlockConstants scans the package's own non-test sources for block constant
// declarations. Source-level because the property is about what EXISTS in the package,
// which no amount of runtime reflection over a registry can observe.
func declaredBlockConstants(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range blockConstRE.FindAllStringSubmatch(string(src), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
